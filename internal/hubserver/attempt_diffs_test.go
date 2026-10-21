package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// attemptDiffFixture is a claimed issue with a running attempt, which is the
// only producer the hub honours today (decisions section 18.5).
type attemptDiffFixture struct {
	nativeFixture
	worker   string
	runnerID string
	issue    tracker.NativeIssue
	policy   string
	lease    tracker.NativeLease
	attempt  string
	run      string
}

func newAttemptDiffFixture(t *testing.T) *attemptDiffFixture {
	t.Helper()
	fixture := newClaimedAttemptDiffFixture(t)
	fixture.start(t)
	return fixture
}

// newClaimedAttemptDiffFixture claims the issue and stops before run.started,
// so a test can change the item between the claim and the start.
func newClaimedAttemptDiffFixture(t *testing.T) *attemptDiffFixture {
	t.Helper()
	return claimedAttemptDiffFixture(t, false)
}

func claimedAttemptDiffFixture(t *testing.T, enrolled bool) *attemptDiffFixture {
	t.Helper()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	f := newNativeFixture(t, service, "", "attempt-diff")
	fixture := &attemptDiffFixture{nativeFixture: f}
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	fixture.policy = policy.ID
	fixture.issue = f.create(t, "diff-work")
	machine := tracker.MachineID("diff-machine")
	if enrolled {
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.enroll(t)
		fixture.worker, machine, fixture.runnerID = r.redemption.Credential, r.binding.MachineID, r.binding.RunnerID
	} else {
		fixture.worker = f.worker(t, "attempt-diff-worker")
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", fixture.worker,
			map[string]any{"id": machine, "hostname": "fixture", "display_name": "Fixture", "version": "test", "capacity": 1}), http.StatusOK)
	}
	capabilities := []string{"native_issues", "scoped_collaboration"}
	if enrolled {
		capabilities = append(capabilities, tracker.NativeExecutionCapability)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", fixture.worker,
		tracker.NativeClaim{PolicyID: fixture.policy, WorkItemID: fixture.issue.WorkItemID, MachineID: machine, SessionID: newNativeID("session"),
			TTLSeconds: 600, ProtocolMajor: 2, Capabilities: capabilities})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &fixture.lease)
	fixture.attempt = newNativeID("attempt")
	fixture.run = newNativeID("run")
	return fixture
}

// start records run.started for the fixture's attempt.
func (f *attemptDiffFixture) start(t *testing.T) {
	t.Helper()
	f.event(t, "run.started", 1, "")
}

// event appends one run event for the fixture's attempt.
func (f *attemptDiffFixture) event(t *testing.T, kind string, sequence int64, outcome string) {
	t.Helper()
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("event")}, Type: kind, SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: sequence, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: outcome},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
}

func (f *attemptDiffFixture) producer() tracker.DiffProducer {
	return tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: f.attempt, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}
}

func (f *attemptDiffFixture) request(seq int64, files ...tracker.AttemptDiffFile) tracker.AttemptDiffRequest {
	return tracker.AttemptDiffRequest{
		Producer:   f.producer(),
		Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: seq},
		BaseSHA:    strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Files: files,
	}
}

func (f *attemptDiffFixture) post(t *testing.T, request tracker.AttemptDiffRequest) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+f.attempt+"/diff", f.worker, request)
}

func (f *attemptDiffFixture) get(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/attempts/"+f.attempt+"/diff"+query, f.token, nil)
}

func requireNativeCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) nativeError {
	t.Helper()
	requireNativeStatus(t, response, status)
	var failure nativeError
	decodeHubResponse(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
	return failure
}

func TestEnrolledRunnerAttemptDiffAuthorization(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *attemptDiffFixture, *tracker.AttemptDiffRequest) (string, string)
		status int
		code   string
	}{
		{name: "current owner", status: http.StatusAccepted},
		{name: "missing claim", status: http.StatusForbidden, code: "insufficient_scope", mutate: func(t *testing.T, f *attemptDiffFixture, _ *tracker.AttemptDiffRequest) (string, string) {
			_, err := f.service.database.db.ExecContext(t.Context(), `UPDATE runner_identities SET operations_json = '["read","collaborate","heartbeat","events"]'`)
			if err != nil {
				t.Fatal(err)
			}
			return f.worker, f.base
		}},
		{name: "wrong runner", status: http.StatusNotFound, code: "not_found", mutate: func(t *testing.T, f *attemptDiffFixture, _ *tracker.AttemptDiffRequest) (string, string) {
			r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			return r.redemption.Credential, f.base
		}},
		{name: "wrong project", status: http.StatusNotFound, code: "not_found", mutate: func(t *testing.T, f *attemptDiffFixture, _ *tracker.AttemptDiffRequest) (string, string) {
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign")
			return f.worker, other.base
		}},
		{name: "wrong attempt", status: http.StatusConflict, code: "stale_execution", mutate: func(_ *testing.T, f *attemptDiffFixture, request *tracker.AttemptDiffRequest) (string, string) {
			request.Producer.ID = newNativeID("attempt")
			return f.worker, f.base
		}},
		{name: "attempt from another work item", status: http.StatusConflict, code: "stale_execution", mutate: func(t *testing.T, f *attemptDiffFixture, request *tracker.AttemptDiffRequest) (string, string) {
			item := f.create(t, "other-diff-work")
			claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: item.WorkItemID, MachineID: f.lease.MachineID, SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
			requireNativeStatus(t, claimed, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, claimed, &lease)
			event := nativeStartedEvent(lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			f.attempt = event.Data.AttemptID
			request.Producer.ID = f.attempt
			return f.worker, f.base
		}},
		{name: "revoked token", status: http.StatusUnauthorized, code: "unauthorized", mutate: func(t *testing.T, f *attemptDiffFixture, _ *tracker.AttemptDiffRequest) (string, string) {
			_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE scope = 'worker'", formatHubTime(f.service.config.now()))
			if err != nil {
				t.Fatal(err)
			}
			return f.worker, f.base
		}},
		{name: "expired lease", status: http.StatusConflict, code: "stale_execution", mutate: func(t *testing.T, f *attemptDiffFixture, _ *tracker.AttemptDiffRequest) (string, string) {
			_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", formatHubTime(f.service.config.now().Add(-time.Second)), f.lease.ID)
			if err != nil {
				t.Fatal(err)
			}
			return f.worker, f.base
		}},
		{name: "superseded fence", status: http.StatusConflict, code: "stale_execution", mutate: func(t *testing.T, f *attemptDiffFixture, _ *tracker.AttemptDiffRequest) (string, string) {
			_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET fencing_token = fencing_token + 1 WHERE lease_id = ?", f.lease.ID)
			if err != nil {
				t.Fatal(err)
			}
			return f.worker, f.base
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := claimedAttemptDiffFixture(t, true)
			f.start(t)
			request := f.request(2, tracker.AttemptDiffFile{Path: "main.go", Status: tracker.DiffStatusModified, Patch: "+fixed"})
			token, base := f.worker, f.base
			if test.mutate != nil {
				token, base = test.mutate(t, f, &request)
			}
			path := base + "/attempts/" + f.attempt + "/diff"
			check := performHubAPIRequest(t, f.service, http.MethodPost, path+"/check", token, request)
			if test.status != http.StatusAccepted {
				requireNativeCode(t, check, test.status, test.code)
				requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodPost, path, token, request), test.status, test.code)
			} else {
				requireNativeStatus(t, check, http.StatusNoContent)
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_diffs WHERE attempt_id = ?", f.attempt).Scan(&count); err != nil || count != 0 {
				t.Fatalf("preflight or denial stored %d diffs: %v", count, err)
			}
			if test.status != http.StatusAccepted {
				return
			}
			posted := performHubAPIRequest(t, f.service, http.MethodPost, path, token, request)
			requireNativeStatus(t, posted, http.StatusAccepted)
			var receipt tracker.AttemptDiffReceipt
			decodeHubResponse(t, posted, &receipt)
			read := performHubAPIRequest(t, f.service, http.MethodGet, path+"?at=2", token, nil)
			requireNativeStatus(t, read, http.StatusOK)
			var diff tracker.AttemptDiff
			decodeHubResponse(t, read, &diff)
			if diff.ID != receipt.DiffID || diff.AttemptID != f.attempt || diff.Generation.Seq != 2 || diff.Producer.LeaseID != f.lease.ID || diff.Producer.FencingToken != f.lease.FencingToken || diff.Producer.RunnerID != f.runnerID || diff.BaseSHA != request.BaseSHA || diff.HeadSHA != request.HeadSHA || len(diff.Files) != 1 || diff.Files[0].Patch != "+fixed" {
				t.Fatalf("stored source identity or diff mismatch: %+v", diff)
			}
			request.Generation.Seq = 3
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/check", token, request), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/release", token, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodPost, path, token, request), http.StatusConflict, "stale_execution")
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_diffs WHERE attempt_id = ?", f.attempt).Scan(&count); err != nil || count != 1 {
				t.Fatalf("released lease changed stored generations: %d, %v", count, err)
			}
		})
	}
}

// The default read is the latest attempt-produced diff; ?at=<seq> reads that
// generation; an attempt with no diff is 404.
func TestAttemptDiffStoreAndRead(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)
	requireNativeStatus(t, f.get(t, ""), http.StatusNotFound)

	first := f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified, Additions: 2, Deletions: 1, Patch: "@@ first"})
	response := f.post(t, first)
	requireNativeStatus(t, response, http.StatusAccepted)
	var receipt tracker.AttemptDiffReceipt
	decodeHubResponse(t, response, &receipt)
	if !receipt.Accepted || !strings.HasPrefix(receipt.DiffID, "diff_") || receipt.FileCount != 1 || receipt.Generation.Seq != 2 {
		t.Fatalf("receipt = %#v", receipt)
	}

	second := f.request(5, tracker.AttemptDiffFile{Path: "b.go", Status: tracker.DiffStatusAdded, Additions: 9, Patch: "@@ second"})
	requireNativeStatus(t, f.post(t, second), http.StatusAccepted)

	var latest tracker.AttemptDiff
	response = f.get(t, "")
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &latest)
	if latest.Generation.Seq != 5 || len(latest.Files) != 1 || latest.Files[0].Path != "b.go" {
		t.Fatalf("latest = %#v", latest)
	}
	if latest.Producer.LeaseID != f.lease.ID || latest.Producer.FencingToken != f.lease.FencingToken || latest.Producer.Kind != tracker.DiffSourceAttempt {
		t.Fatalf("producer = %#v", latest.Producer)
	}
	if latest.BaseSHA != strings.Repeat("a", 40) || latest.HeadSHA != strings.Repeat("b", 40) {
		t.Fatalf("shas = %q %q", latest.BaseSHA, latest.HeadSHA)
	}

	var pinned tracker.AttemptDiff
	response = f.get(t, "?at=2")
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &pinned)
	if pinned.Generation.Seq != 2 || pinned.Files[0].Path != "a.go" || pinned.Files[0].Patch != "@@ first" {
		t.Fatalf("pinned = %#v", pinned)
	}
	requireNativeStatus(t, f.get(t, "?at=99"), http.StatusNotFound)
	// A workspace-produced diff cannot exist yet, so the source is accepted
	// and answers nothing rather than changing shape later.
	requireNativeStatus(t, f.get(t, "?source=workspace"), http.StatusNotFound)
	requireNativeCode(t, f.get(t, "?at=0"), http.StatusUnprocessableEntity, "invalid_request")
	requireNativeCode(t, f.get(t, "?source=relay"), http.StatusUnprocessableEntity, "invalid_request")

	// Catches direct MCP selectors joining an authorized item to another item's
	// attempt, while preserving exact diff generations and run identity.
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	executor := hubOperatorExecutor{f.service}
	other := f.create(t, "other-diff-item")
	for _, test := range []struct {
		name, tool string
		item       tracker.NativeWorkItemID
		sequence   int64
		denied     bool
		source     string
		absent     bool
	}{
		{"latest", operatortool.GetAttemptDiff, f.issue.WorkItemID, 0, false, "", false},
		{"pinned", operatortool.GetAttemptDiff, f.issue.WorkItemID, 2, false, "", false},
		{"work item", operatortool.GetWorkItemDiff, f.issue.WorkItemID, 0, false, "", false},
		{"workspace without generation", operatortool.GetWorkItemDiff, f.issue.WorkItemID, 0, false, tracker.DiffSourceWorkspace, true},
		{"work item without generation", operatortool.GetWorkItemDiff, other.WorkItemID, 0, false, "", true},
		{"run", operatortool.GetNativeRun, f.issue.WorkItemID, 0, false, "", false},
		{"foreign diff", operatortool.GetAttemptDiff, other.WorkItemID, 0, true, "", false},
		{"foreign run", operatortool.GetNativeRun, other.WorkItemID, 0, true, "", false},
	} {
		t.Run("operator/"+test.name, func(t *testing.T) {
			args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(test.item), Sequence: test.sequence, Source: test.source}
			if test.tool != operatortool.GetWorkItemDiff {
				args.AttemptID = f.attempt
			}
			result, err := executor.Execute(ctx, changeToolCall(test.tool, args))
			if test.denied {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("foreign ownership: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var value operatortool.ChangeResult
			if err := json.Unmarshal(result.Content, &value); err != nil {
				t.Fatal(err)
			}
			if test.tool == operatortool.GetNativeRun {
				if value.Attempt == nil || value.Attempt.AttemptID != f.attempt {
					t.Fatal("missing run identity")
				}
			} else if test.absent {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(result.Content, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["diff"]) != "null" || value.Freshness != "live" || value.GeneratedAt.IsZero() {
					t.Fatalf("missing explicit stored-generation absence or read freshness: %s", result.Content)
				}
			} else {
				want := int64(5)
				if test.sequence != 0 {
					want = test.sequence
				}
				if value.Diff == nil || value.Diff.Generation.Seq != want || value.Diff.AttemptID != f.attempt {
					t.Fatal("incorrect diff identity")
				}
				if value.Diff.Producer != latest.Producer || value.Diff.Generation.Source != tracker.DiffSourceAttempt || value.Diff.CreatedAt.IsZero() {
					t.Fatal("lost stored generation provenance")
				}
				storedAt := latest.CreatedAt
				if test.sequence == 2 {
					storedAt = pinned.CreatedAt
				}
				if !value.Diff.CreatedAt.Equal(storedAt) || value.Freshness != "live" || value.GeneratedAt.Before(storedAt) {
					t.Fatal("stored timestamp or read freshness changed")
				}
			}
		})
	}
}

// "A seq at or below the stored one is rejected with stale_generation."
func TestAttemptDiffStaleGeneration(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)
	requireNativeStatus(t, f.post(t, f.request(4, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})), http.StatusAccepted)
	tests := []struct {
		name string
		seq  int64
	}{
		{name: "same generation", seq: 4},
		{name: "earlier generation", seq: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireNativeCode(t, f.post(t, f.request(test.seq, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})),
				http.StatusConflict, "stale_generation")
		})
	}
	requireNativeStatus(t, f.post(t, f.request(5, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})), http.StatusAccepted)
}

// Producer fencing: while the attempt runs its own lease is the producer, and
// once that lease is released nothing may write to it, because workspace
// sessions -- the only other legal producer -- do not exist yet.
func TestAttemptDiffProducerFencing(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)

	t.Run("wrong fencing token", func(t *testing.T) {
		request := f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})
		request.Producer.FencingToken = f.lease.FencingToken + 1
		requireNativeCode(t, f.post(t, request), http.StatusConflict, "stale_execution")
	})
	t.Run("another attempt's id", func(t *testing.T) {
		request := f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})
		request.Producer.ID = newNativeID("attempt")
		requireNativeCode(t, f.post(t, request), http.StatusConflict, "stale_execution")
	})
	t.Run("a workspace producer has no lease the hub issued", func(t *testing.T) {
		request := f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})
		request.Producer.Kind = tracker.DiffSourceWorkspace
		request.Generation = tracker.DiffGeneration{Source: tracker.DiffSourceWorkspace, ID: "ws_" + strings.Repeat("0", 32), Seq: 1}
		requireNativeCode(t, f.post(t, request), http.StatusConflict, "stale_execution")
	})
	t.Run("the running lease may write", func(t *testing.T) {
		requireNativeStatus(t, f.post(t, f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})), http.StatusAccepted)
	})
	t.Run("a released lease may not", func(t *testing.T) {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/release", f.worker,
			tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		requireNativeCode(t, f.post(t, f.request(3, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified})),
			http.StatusConflict, "stale_execution")
	})
}

// "The same rule applies to the stored diff, so a later reader is not shown
// what the live reader was not." The filter runs on write.
func TestAttemptDiffDenylistAppliedOnWrite(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)
	request := f.request(2,
		tracker.AttemptDiffFile{Path: ".env", Status: tracker.DiffStatusAdded, Additions: 3, Patch: "+TOKEN=secret"},
		tracker.AttemptDiffFile{Path: "web/node_modules/pkg/index.js", Status: tracker.DiffStatusAdded, Additions: 1, Patch: "+vendored"},
		tracker.AttemptDiffFile{Path: "settings.txt", OldPath: ".env", Status: tracker.DiffStatusRenamed, Additions: 1, Patch: "+TOKEN=secret"},
		tracker.AttemptDiffFile{Path: "internal/app/main.go", Status: tracker.DiffStatusModified, Additions: 4, Deletions: 2, Patch: "+ordinary"},
	)
	requireNativeStatus(t, f.post(t, request), http.StatusAccepted)
	var stored tracker.AttemptDiff
	response := f.get(t, "")
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if len(stored.Files) != 4 {
		t.Fatalf("files = %d, want 4", len(stored.Files))
	}
	for index, want := range []struct {
		path      string
		denied    bool
		additions int
	}{
		{path: ".env", denied: true, additions: 3},
		{path: "web/node_modules/pkg/index.js", denied: true, additions: 1},
		{path: "settings.txt", denied: true, additions: 1},
		{path: "internal/app/main.go", additions: 4},
	} {
		file := stored.Files[index]
		if file.Path != want.path || file.Denied != want.denied || file.Additions != want.additions {
			t.Fatalf("file %d = %#v, want %+v", index, file, want)
		}
		if want.denied && file.Patch != "" {
			t.Fatalf("denied file %s kept a patch", file.Path)
		}
		if !want.denied && file.Patch == "" {
			t.Fatalf("allowed file %s lost its patch", file.Path)
		}
	}
	// The secret never reached the database, not merely the response.
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM attempt_diff_files WHERE patch LIKE '%TOKEN=secret%'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stored %d denied patches", count)
	}
}

// "A patch over 1 MB is stored truncated with truncated: true; a diff over
// 20 MB is rejected with diff_too_large and the runner posts the file list
// without patches."
func TestAttemptDiffSizeCaps(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)

	long := tracker.AttemptDiffFile{Path: "big.go", Status: tracker.DiffStatusModified, Additions: 5, Patch: strings.Repeat("x", tracker.MaxDiffPatchBytes+512)}
	requireNativeStatus(t, f.post(t, f.request(2, long)), http.StatusAccepted)
	var stored tracker.AttemptDiff
	response := f.get(t, "")
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if !stored.Truncated || !stored.Files[0].Truncated || len(stored.Files[0].Patch) != tracker.MaxDiffPatchBytes {
		t.Fatalf("truncation = %v/%v len=%d", stored.Truncated, stored.Files[0].Truncated, len(stored.Files[0].Patch))
	}
	if stored.Files[0].Additions != 5 {
		t.Fatalf("counts changed: %#v", stored.Files[0])
	}

	// Twenty-one files at the per-file cap exceed the whole-diff bound.
	oversized := make([]tracker.AttemptDiffFile, 0, 21)
	for index := range 21 {
		oversized = append(oversized, tracker.AttemptDiffFile{
			Path: "huge" + strconv.Itoa(index) + ".go", Status: tracker.DiffStatusModified, Additions: 1,
			Patch: strings.Repeat("y", tracker.MaxDiffPatchBytes),
		})
	}
	requireNativeCode(t, f.post(t, f.request(3, oversized...)), http.StatusRequestEntityTooLarge, "diff_too_large")

	// The same list without patches is accepted, and the counts survive.
	requireNativeStatus(t, f.post(t, f.request(3, tracker.StripDiffPatches(oversized)...)), http.StatusAccepted)
	response = f.get(t, "")
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if len(stored.Files) != 21 || stored.Files[0].Patch != "" || stored.Files[0].Additions != 1 {
		t.Fatalf("stripped diff = %#v", stored.Files[0])
	}
}

// Validation refuses a malformed post before anything is stored.
func TestAttemptDiffValidation(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)
	tests := []struct {
		name    string
		mutate  func(*tracker.AttemptDiffRequest)
		status  int
		code    string
		attempt string
	}{
		{
			name: "generation source disagrees with the producer",
			mutate: func(r *tracker.AttemptDiffRequest) {
				r.Generation.Source = tracker.DiffSourceWorkspace
				r.Generation.ID = "ws_1"
			},
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name:   "zero generation",
			mutate: func(r *tracker.AttemptDiffRequest) { r.Generation.Seq = 0 },
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name:   "unknown file status",
			mutate: func(r *tracker.AttemptDiffRequest) { r.Files[0].Status = "copied" },
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name:   "oversized sha",
			mutate: func(r *tracker.AttemptDiffRequest) { r.HeadSHA = strings.Repeat("c", tracker.MaxDiffSHABytes+1) },
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name:   "missing producer lease",
			mutate: func(r *tracker.AttemptDiffRequest) { r.Producer.LeaseID = "" },
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name:    "unknown attempt id shape",
			attempt: "not-an-attempt",
			status:  http.StatusNotFound, code: "not_found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified, Patch: "@@"})
			if test.mutate != nil {
				test.mutate(&request)
			}
			attempt := f.attempt
			if test.attempt != "" {
				attempt = test.attempt
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+attempt+"/diff", f.worker, request)
			requireNativeCode(t, response, test.status, test.code)
		})
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_diffs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stored %d diffs from refused posts", count)
	}
}

// The read follows the issue's read rule: a token with no grant on the project
// cannot see the diff, and the answer is opaque.
func TestAttemptDiffReadRule(t *testing.T) {
	t.Parallel()
	f := newAttemptDiffFixture(t)
	requireNativeStatus(t, f.post(t, f.request(2, tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified, Patch: "@@"})), http.StatusAccepted)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "attempt-diff-other")
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/attempts/"+f.attempt+"/diff", other.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
}

// The diff route's body allowance must hold a diff at the contract's limit at
// the worst case of JSON string escaping, or a diff the contract accepts is
// refused by the transport instead. Each byte class the encoder escapes costs
// at most six bytes, which is the factor the allowance is sized by.
func TestAttemptDiffBodyAllowanceCoversEscaping(t *testing.T) {
	t.Parallel()
	const sample = 1 << 10
	for _, test := range []struct {
		name string
		char string
	}{
		{name: "less than", char: "<"},
		{name: "greater than", char: ">"},
		{name: "ampersand", char: "&"},
		{name: "escape", char: "\x1b"},
		{name: "quote", char: "\""},
		{name: "newline", char: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(strings.Repeat(test.char, sample))
			if err != nil {
				t.Fatal(err)
			}
			if perByte := (len(encoded) - 2) / sample; perByte > 6 {
				t.Fatalf("%q encodes to %d bytes per byte, above the allowance factor", test.char, perByte)
			}
		})
	}
	if want := int64(6*tracker.MaxDiffBytes + (1 << 20)); maxAttemptDiffRequestBytes != want {
		t.Fatalf("allowance = %d, want %d", maxAttemptDiffRequestBytes, want)
	}
}
