package hubserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeFailureSignatureReads(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, RunDetails: &memoryRunDetails{}})
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
	for _, test := range []struct {
		name, status, want, class string
		data                      tracker.NativeRunData
		malformedPhases           bool
	}{
		{"recorded error", "failed", "count <n> failed", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "count 123 failed\nprivate details"}}, false},
		{"malformed phases preserve recorded error", "failed", "count <n> failed", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "count 123 failed\nprivate details"}}, true},
		{"execution error precedes finalization", "failed", "provider failed", "infrastructure", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "provider failed", ErrorClass: "protocol"}, Finalization: &tracker.NativeFinalization{Error: "publish failed"}}, false},
		{"source finalization gate", "failed", "source finalization gate failed: exit status 1", "attempt", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Stage: gate.StageSourceFinalization, Command: "go test ./internal/example", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: 1, DurationNS: 1}}}, false},
		{"landing gate", "succeeded", "landing gate failed: exit status 1", "merge", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{GateFailed: true, Gate: &gate.CommandResult{Stage: gate.StageLanding, Command: "make check-land", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: 1}, ObservedAt: now}}}, false},
		{"issue configuration before provider", "failed", "agent override rejected", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "agent override rejected", ErrorClass: "issue_configuration", Stage: "pre_provider"}}, false},
		{"unknown before provider", "failed", "unknown setup error", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "unknown setup error", Stage: "pre_provider"}}, false},
		{"source recovery after provider", "failed", "native checkpoint requires recovery: source changed", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "native checkpoint requires recovery: source changed", ErrorClass: "source_recovery"}}, false},
		{"workspace", "failed", "workspace hook failed", "infrastructure", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "workspace hook failed", ErrorClass: "workspace_hook"}}, false},
		{"protocol", "interrupted", "provider request failed", "infrastructure", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "provider request failed", ErrorClass: "protocol"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Stage: gate.StageSourceFinalization, Command: "go test example", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: 1, DurationNS: 1}}}, false},
		{"historical metadata", "failed", "codex turn/start input_too_large: provider error", "attempt", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Provider: "codex", Operation: "turn/start", ProviderCode: "input_too_large", Summary: "provider error"}}, false},
		{"finalization", "failed", "local_checkpoint_changed: count <n> changed", "attempt", tracker.NativeRunData{Finalization: &tracker.NativeFinalization{VersionCode: "local_checkpoint_changed", VersionError: "count 142 changed"}}, false},
		{"conflict", "succeeded", "landing refused: conflict", "merge", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: now}}}, false},
		{"base protected", "failed", "landing refused: base_protected", "merge", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "base_protected", ObservedAt: now}}}, false},
		{"successful landing", "succeeded", "", "", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, RefusalKind: "conflict", ObservedAt: now}}}, false},
		{"cancelled", "cancelled", "", "", tracker.NativeRunData{}, false},
		{"missing error", "failed", "attempt failed: recorded error unavailable", "attempt", tracker.NativeRunData{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.data.Runtime != nil && test.data.Runtime.Validation != nil {
				if err := validateNativeRuntime(test.data.Runtime); err != nil {
					t.Fatalf("source gate failure receipt rejected: %v", err)
				}
			}
			fixture := newNativeFixture(t, f.service, f.project.OrganizationID, test.name)
			scope := analyticsTestScope(t, fixture)
			issue := seedAnalyticsTestWorkItem(t, f.service, scope.Organization, scope.Project, "failure", 1, test.name, "Todo", now.Add(-time.Hour))
			attemptID := "attempt-" + string(scope.Project)
			seedAnalyticsTestAttempt(t, hostedPostgresAdmin(t, f.service), f.service, scope, issue.WorkItemID, attemptID, test.status, now.Add(-3*time.Minute), now, test.data, test.malformedPhases)
			if err := f.service.config.TenantRepository.(*postgres.Repository).ImportReportUsageFacts(t.Context(), []reportstore.UsageRecord{
				{OrganizationID: scope.Organization, UpdatedAt: now, UsageFact: reportstore.UsageFact{ProjectID: scope.Project, WorkItemID: issue.WorkItemID, AttemptID: attemptID, Period: now.Add(-2 * time.Hour), Provider: "codex", Model: "test", Input: 100, CachedInput: 50, Output: 20, Cost: 0.5, Currency: "USD"}},
				{OrganizationID: scope.Organization, UpdatedAt: now, UsageFact: reportstore.UsageFact{ProjectID: scope.Project, WorkItemID: issue.WorkItemID, AttemptID: attemptID, Period: now.Add(-2 * time.Hour), Provider: "codex", Model: "other", Input: 10, CachedInput: 10, Cost: 0.25, Currency: "USD"}},
			}); err != nil {
				t.Fatal(err)
			}
			out, err := f.service.readNativeAnalyticsTenant(t.Context(), scope, operatortool.AnalyticsRequest{Limit: 10}, w)
			if err != nil {
				t.Fatal(err)
			}
			failures, partial, _, err := f.service.readNativeFailuresTenant(t.Context(), scope, issue.WorkItemID, operatortool.AnalyticsWindow{From: time.Time{}, To: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)})
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
			foreign.Project = "prj_foreign"
			hidden, _, _, err := f.service.readNativeFailuresTenant(t.Context(), foreign, "", w)
			if !hubapi.IsNativeNotFound(err) || len(hidden) != 0 {
				t.Fatalf("scope leak: %#v %v", hidden, err)
			}
		})
	}

	t.Run("failure window boundaries", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			at       time.Time
			landing  bool
			want     int
			recorded *time.Time
		}{
			{"attempt lower bound", w.From, false, 1, nil},
			{"attempt upper bound", w.To, false, 0, nil},
			{"attempt before window", w.From.Add(-time.Second), false, 0, nil},
			{"landing lower bound", w.From, true, 1, nil},
			{"landing upper bound", w.To, true, 0, nil},
			{"landing before window", w.From.Add(-time.Second), true, 0, nil},
			{"landing recorded after window", now, true, 1, nil},
			{"landing observed after recording", now, true, 1, new(w.From.Add(-time.Minute))},
		} {
			t.Run(test.name, func(t *testing.T) {
				data := tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "count 1 failed"}}
				updated := test.at
				if test.landing {
					data.Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: test.at}}
					updated = w.To.Add(time.Minute)
				}
				if test.recorded != nil {
					updated = *test.recorded
				}
				fixture := newNativeFixture(t, f.service, f.project.OrganizationID, test.name)
				scope := analyticsTestScope(t, fixture)
				issue := seedAnalyticsTestWorkItem(t, f.service, scope.Organization, scope.Project, "boundary", 1, test.name, "Todo", test.at.Add(-time.Hour))
				seedAnalyticsTestAttempt(t, hostedPostgresAdmin(t, f.service), f.service, scope, issue.WorkItemID, "boundary-"+string(scope.Project), "failed", hubapi.MinTime(test.at, updated).Add(-3*time.Minute), updated, data)
				failures, partial, _, err := f.service.readNativeFailuresTenant(t.Context(), scope, "", w)
				if err != nil || partial || len(failures) != test.want {
					t.Fatalf("window failures=%#v partial=%v err=%v", failures, partial, err)
				}
			})
		}

	})

	for _, count := range []int{maxAnalyticsPopulation, maxAnalyticsPopulation + 1} {
		t.Run(fmt.Sprintf("population %d", count), func(t *testing.T) {
			fixture := newNativeFixture(t, f.service, f.project.OrganizationID, fmt.Sprintf("population %d", count))
			scope := analyticsTestScope(t, fixture)
			issue := seedAnalyticsTestWorkItem(t, f.service, scope.Organization, scope.Project, "population", 1, "population", "Todo", now.Add(-time.Hour))
			db := hostedPostgresAdmin(t, f.service)
			for index := range count {
				seedAnalyticsTestAttempt(t, db, f.service, scope, issue.WorkItemID, fmt.Sprintf("%s-failure-%04d", scope.Project, index), "failed", now.Add(-3*time.Minute), now, tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "count 1 failed"}})
			}
			ctx, paging, err := analyticsPagingContext(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			failures, partial, _, err := f.service.readNativeFailuresTenant(ctx, scope, issue.WorkItemID, w)
			if err != nil || partial != (count > maxAnalyticsPopulation) || len(failures) != maxAnalyticsPopulation {
				t.Fatalf("population=%d partial=%v err=%v", len(failures), partial, err)
			}
			if count > maxAnalyticsPopulation {
				token := paging.Next["failures"]
				if token == "" {
					t.Fatal("missing failure continuation")
				}
				tailCtx, _, err := analyticsPagingContext(t.Context(), token)
				if err != nil {
					t.Fatal(err)
				}
				tail, tailPartial, _, err := f.service.readNativeFailuresTenant(tailCtx, scope, issue.WorkItemID, w)
				if err != nil || len(tail) != 1 || !tailPartial || tail[0].AttemptID != fmt.Sprintf("%s-failure-0000", scope.Project) {
					t.Fatalf("failure tail=%+v partial=%v error=%v", tail, tailPartial, err)
				}
			}
			latest := latestNativeFailureSignature(failures, partial)
			if latest == nil || latest.Count != maxAnalyticsPopulation || latest.Partial != partial {
				t.Fatalf("latest=%#v", latest)
			}
		})
	}
}
