package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNormalizeFailureSignature(t *testing.T) {
	for _, test := range []struct {
		name, a, b, want string
	}{
		{"ids", "attempt 123 wi_abc123 lease_deadbeef", "attempt 456 wi_def456 lease_abcd1234", "attempt <id> <id> <id>"},
		{"hashes", "checkpoint deadbeef12345678 changed", "checkpoint abcdef1234567890 changed", "checkpoint <hash> changed"},
		{"numeric hashes", "checkpoint " + strings.Repeat("1", 40) + " changed", "checkpoint " + strings.Repeat("2", 64) + " changed", "checkpoint <hash> changed"},
		{"paths", "open /work/123/file42.go failed", "open /work/456/file97.go failed", "open /work/<n>/file<n>.go failed"},
		{"windows paths", `open C:\work\123\file42.go failed`, `open C:\work\456\file97.go failed`, `open C:\work\<n>\file<n>.go failed`},
		{"counts", "actual 1048576 characters exceeds limit 65536", "actual 2222222 characters exceeds limit 99999", "actual <n> characters exceeds limit <n>"},
		{"bytes", "received 1048576 bytes, needed 32 KiB", "received 2097152 bytes, needed 64 KiB", "received <bytes>, needed <bytes>"},
		{"timestamps", "failed at 2026-10-06T10:23:12.123Z", "failed at 2025-01-01T12:59:59-05:00", "failed at <timestamp>"},
		{"first line", "  local_checkpoint_changed\nprivate details", "local_checkpoint_changed\r\nother details", "local_checkpoint_changed"},
		{"bounded", strings.Repeat("界", 200), strings.Repeat("界", 200), strings.Repeat("界", 85)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, input := range []string{test.a, test.b} {
				got := normalizeFailureSignature(input)
				if got != test.want || len(got) > maxFailureSignatureLength || !utf8.ValidString(got) {
					t.Fatalf("normalize %q = %q, want %q", input, got, test.want)
				}
			}
		})
	}
	for _, pair := range [][2]string{{"local_checkpoint_changed", "local_checkpoint_missing"}, {"HTTP 401 unauthorized", "HTTP 403 forbidden"}, {"RPC code -32602", "RPC code -32603"}, {"open /work/123/a.go failed", "open /work/456/b.go failed"}} {
		if normalizeFailureSignature(pair[0]) == normalizeFailureSignature(pair[1]) {
			t.Fatalf("distinct errors collapsed: %q", pair)
		}
	}
}

func TestGroupNativeFailureSignatures(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, class, want string
		count             int
		step              time.Duration
		separateItems     bool
		olderFailure      bool
	}{
		{"sliding window", "attempt", "retry storm", 6, time.Minute, false, true},
		{"ordinary", "attempt", "attempt", 4, time.Minute, false, false},
		{"storm inclusive boundary", "attempt", "retry storm", 5, 15 * time.Minute, false, false},
		{"outside storm window", "attempt", "attempt", 5, 15*time.Minute + time.Second, false, false},
		{"different items", "attempt", "attempt", 5, time.Minute, true, false},
		{"infrastructure", "infrastructure", "infrastructure", 1, time.Minute, false, false},
		{"infrastructure storm", "infrastructure", "retry storm", 5, time.Minute, false, false},
		{"merge", "merge", "merge", 1, time.Minute, false, false},
		{"merge storm", "merge", "retry storm", 5, time.Minute, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			failures := []nativeFailure{}
			for i := range test.count {
				item := "wi_one"
				if test.separateItems {
					item = fmt.Sprintf("wi_%d", i)
				}
				at := now.Add(time.Duration(i) * test.step)
				if test.olderFailure && i == 0 {
					at = now.Add(-2 * time.Hour)
				}
				failures = append(failures, nativeFailure{workItem: item, example: fmt.Sprintf("count %d failed\nsecond line", i), class: test.class, at: at, started: at.Add(-2 * time.Minute), cost: 0.5, tokens: 10})
			}
			got := groupNativeFailures(failures)
			if len(got) != 1 {
				t.Fatalf("groups=%#v", got)
			}
			g := got[0]
			if g.Class != test.want || g.Count != test.count || g.Cost != 0.5*float64(test.count) || g.Tokens != 10*int64(test.count) || g.SlotMinutes != 2*float64(test.count) || g.FirstSeen != failures[0].at || g.LastSeen != failures[len(failures)-1].at || strings.Contains(g.Example, "\n") {
				t.Fatalf("group=%#v", g)
			}
			for i, j := 0, len(failures)-1; i < j; i, j = i+1, j-1 {
				failures[i], failures[j] = failures[j], failures[i]
			}
			if reverse := groupNativeFailures(failures); !reflect.DeepEqual(reverse, got) {
				t.Fatalf("order-dependent groups: %#v / %#v", reverse, got)
			}
		})
	}
	t.Run("latest signature counts only matching failures", func(t *testing.T) {
		failures := []nativeFailure{
			{attemptID: "attempt_a", example: "count 1 failed", at: now},
			{attemptID: "attempt_b", example: "other error", at: now.Add(-time.Minute)},
			{attemptID: "attempt_c", example: "count 2 failed", at: now.Add(-time.Minute)},
		}
		got := latestNativeFailureSignature(failures, true)
		if got == nil || got.Signature != "count <n> failed" || got.Count != 2 || !got.Partial {
			t.Fatalf("latest=%#v", got)
		}
		groups := groupNativeFailures(failures)
		if len(groups) != 2 || groups[0].Count != 2 || groups[1].Count != 1 {
			t.Fatalf("signature grouping order=%#v", groups)
		}
		if latestNativeFailureSignature(nil, false) != nil {
			t.Fatal("empty failures fabricated a signature")
		}
	})

	t.Run("bounded items examples and pages", func(t *testing.T) {
		failures := []nativeFailure{}
		for i := range maxFailureWorkItems + 3 {
			failures = append(failures, nativeFailure{workItem: fmt.Sprintf("wi_%02d", i), example: strings.Repeat("界", 1000), class: "attempt", at: now, started: now.Add(time.Minute)})
		}
		got := groupNativeFailures(failures)[0]
		if got.WorkItemsCount != len(failures) || len(got.WorkItems) != maxFailureWorkItems || !got.WorkItemsPartial || len(got.Example) > maxFailureExampleLength || !utf8.ValidString(got.Example) || got.SlotMinutes != 0 {
			t.Fatalf("bounds=%#v", got)
		}
		groups := make([]nativeFailureSignature, 200)
		for i := range groups {
			groups[i] = got
			groups[i].WorkItems = make([]string, maxFailureWorkItems)
			for j := range groups[i].WorkItems {
				groups[i].WorkItems[j] = strings.Repeat("w", 128)
			}
		}
		page := nativeFailureSignaturePage(groups, 0, 200)
		raw, err := json.Marshal(page.Items)
		if err != nil || len(raw) > operatortool.WorkListPageBytes || page.NextOffset == nil || *page.NextOffset != len(page.Items) {
			t.Fatalf("page size=%d continuation=%v err=%v", len(raw), page.NextOffset, err)
		}
	})
}

func TestNativeFailureSignatureReads(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "failure signatures")
	worker := f.worker(t, "signatures")
	lease := claimNativeAttempt(t, f, worker, "signature-machine", "signature-session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
	for _, test := range []struct {
		name, status, want, class string
		data                      tracker.NativeRunData
	}{
		{"recorded error", "failed", "count <n> failed", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "count 123 failed\nprivate details"}}},
		{"execution error precedes finalization", "failed", "provider failed", "infrastructure", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "provider failed", ErrorClass: "protocol"}, Finalization: &tracker.NativeFinalization{Error: "publish failed"}}},
		{"source finalization gate", "failed", "source finalization gate failed: exit status 1", "attempt", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Stage: gate.StageSourceFinalization, Command: "go test ./internal/example", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: 1, DurationNS: 1}}}},
		{"landing gate", "succeeded", "landing gate failed: exit status 1", "merge", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{GateFailed: true, Gate: &gate.CommandResult{Stage: gate.StageLanding, Command: "make check-land", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: 1}, ObservedAt: now}}}},
		{"workspace", "failed", "workspace hook failed", "infrastructure", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "workspace hook failed", ErrorClass: "workspace_hook"}}},
		{"protocol", "interrupted", "provider request failed", "infrastructure", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "provider request failed", ErrorClass: "protocol"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Stage: gate.StageSourceFinalization, Command: "go test example", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: 1, DurationNS: 1}}}},
		{"historical metadata", "failed", "codex turn/start input_too_large: provider error", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Provider: "codex", Operation: "turn/start", ProviderCode: "input_too_large", Summary: "provider error"}}},
		{"finalization", "failed", "local_checkpoint_changed: count <n> changed", "attempt", tracker.NativeRunData{Finalization: &tracker.NativeFinalization{VersionCode: "local_checkpoint_changed", VersionError: "count 142 changed"}}},
		{"conflict", "succeeded", "landing refused: conflict", "merge", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: now}}}},
		{"base protected", "failed", "landing refused: base_protected", "merge", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "base_protected", ObservedAt: now}}}},
		{"successful landing", "succeeded", "", "", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, RefusalKind: "conflict", ObservedAt: now}}}},
		{"cancelled", "cancelled", "", "", tracker.NativeRunData{}},
		{"missing error", "failed", "attempt failed: recorded error unavailable", "attempt", tracker.NativeRunData{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.data.Runtime != nil && test.data.Runtime.Validation != nil {
				if err := validateNativeRuntime(test.data.Runtime); err != nil {
					t.Fatalf("source gate failure receipt rejected: %v", err)
				}
			}
			raw, err := json.Marshal(test.data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET status=?,data_json=?,started_at=?,updated_at=? WHERE id=?", test.status, string(raw), formatHubTime(now.Add(-3*time.Minute)), formatHubTime(now), start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT OR REPLACE INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,input,cached_input,output,cost_estimate,updated_at) VALUES(?,?,?,?,?,?,100,50,20,0.5,?)`, start.Data.AttemptID, scope.organization, scope.project, formatHubTime(now.Add(-2*time.Hour)), "codex", "test", formatHubTime(now)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT OR REPLACE INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,input,cached_input,output,cost_estimate,updated_at) VALUES(?,?,?,?,?,?,10,10,0,0.25,?)`, start.Data.AttemptID, scope.organization, scope.project, formatHubTime(now.Add(-2*time.Hour)), "codex", "other", formatHubTime(now)); err != nil {
				t.Fatal(err)
			}
			out, err := readNativeAnalytics(t.Context(), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 10}, w)
			if err != nil {
				t.Fatal(err)
			}
			failures, partial, err := readNativeFailures(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), nil)
			if err != nil || partial {
				t.Fatalf("read failures: partial=%v err=%v", partial, err)
			}
			latest := latestNativeFailureSignature(failures, partial)
			if test.want == "" {
				if len(out.FailureSignatures.Items) != 0 || latest != nil {
					t.Fatalf("nonfailure grouped: %#v %#v", out.FailureSignatures, latest)
				}
				return
			}
			if len(out.FailureSignatures.Items) != 1 || latest == nil || latest.Signature != test.want || latest.Count != 1 {
				t.Fatalf("read groups=%#v latest=%#v", out.FailureSignatures, latest)
			}
			g := out.FailureSignatures.Items[0]
			if g.Signature != test.want || g.Class != test.class || g.Cost != 0.75 || g.Tokens != 130 || g.SlotMinutes != 3 || g.WorkItems[0] != string(issue.WorkItemID) {
				t.Fatalf("group=%#v", g)
			}
			foreign := scope
			foreign.project = "prj_foreign"
			hidden, _, err := readNativeFailures(t.Context(), f.service.database.db, foreign, "", &w)
			if err != nil || len(hidden) != 0 {
				t.Fatalf("scope leak: %#v %v", hidden, err)
			}
		})
	}

	t.Run("failure window boundaries", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			at      time.Time
			landing bool
			want    int
		}{
			{"attempt lower bound", w.From, false, 1},
			{"attempt upper bound", w.To, false, 0},
			{"attempt before window", w.From.Add(-time.Second), false, 0},
			{"landing lower bound", w.From, true, 1},
			{"landing upper bound", w.To, true, 0},
			{"landing before window", w.From.Add(-time.Second), true, 0},
			{"landing recorded after window", now, true, 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				data := tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "count 1 failed"}}
				updated := test.at
				if test.landing {
					data.Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: test.at}}
					updated = w.To.Add(time.Minute)
				}
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET status='failed',data_json=?,updated_at=? WHERE id=?", string(raw), formatHubTime(updated), start.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
				failures, partial, err := readNativeFailures(t.Context(), f.service.database.db, scope, "", &w)
				if err != nil || partial || len(failures) != test.want {
					t.Fatalf("window failures=%#v partial=%v err=%v", failures, partial, err)
				}
			})
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json='{}',updated_at=? WHERE id=?", formatHubTime(now), start.Data.AttemptID); err != nil {
			t.Fatal(err)
		}
	})

	for _, count := range []int{maxAnalyticsPopulation, maxAnalyticsPopulation + 1} {
		t.Run(fmt.Sprintf("population %d", count), func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=json_set(data_json,'$.terminal_failure',json('{"error":"count 1 failed"}')) WHERE id=?`, start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at)
SELECT 'lease_signature_'||i,l.issue_id,l.machine_id,'session_signature_'||i,l.expires_at,l.acquired_at,l.renewed_at,l.updated_at,l.created_at,l.updated_at
FROM n CROSS JOIN leases l WHERE l.lease_id=?`, count-1, lease.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at)
SELECT 'attempt_signature_'||l.fencing_token,a.organization_id,a.project_id,a.work_item_id,l.lease_id,l.fencing_token,'run_signature_'||l.fencing_token,1,'failed',a.data_json,a.started_at,a.updated_at
FROM native_attempts a CROSS JOIN leases l WHERE a.id=? AND l.lease_id LIKE 'lease_signature_%'`, start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			failures, partial, err := readNativeFailures(t.Context(), tx, scope, string(issue.WorkItemID), &w)
			if err != nil || partial != (count > maxAnalyticsPopulation) || len(failures) != maxAnalyticsPopulation {
				t.Fatalf("population=%d partial=%v err=%v", len(failures), partial, err)
			}
			latest := latestNativeFailureSignature(failures, partial)
			if latest == nil || latest.Count != maxAnalyticsPopulation || latest.Partial != partial {
				t.Fatalf("latest=%#v", latest)
			}
		})
	}
}
