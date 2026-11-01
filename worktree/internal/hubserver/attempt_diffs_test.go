package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// attemptDiffFixture is a claimed issue with a running attempt, which is the
// only producer the hub honours today.
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

type convertingDiffQuery struct {
	nativeQueryer
	convert func() error
}

func (q *convertingDiffQuery) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	if q.convert != nil && strings.Contains(statement, "attempt_diff_file_archives") {
		convert := q.convert
		q.convert = nil
		if err := convert(); err != nil {
			return nil, err
		}
	}
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
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
	return claimedAttemptDiffFixture(t, false, Config{})
}

func claimedAttemptDiffFixture(t *testing.T, enrolled bool, cfg Config) *attemptDiffFixture {
	t.Helper()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "hub.db")
	service := openTestService(t, cfg)
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
			f := claimedAttemptDiffFixture(t, true, Config{})
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
		name     string
		mutate   func(*tracker.AttemptDiffRequest)
		status   int
		code     string
		attempt  string
		manifest bool
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
		{
			name:     "shared manifest inline patch",
			manifest: true,
			status:   http.StatusUnprocessableEntity, code: "invalid_request",
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
			if test.manifest {
				var logs bytes.Buffer
				objects := &changeObjectFixture{}
				service := &Service{database: f.service.database, config: Config{
					Hosted: &HostedConfig{SharedEntry: &HostedSharedEntry{}},
					Logger: slog.New(hostedLogHandler{output: slog.NewJSONHandler(&logs, nil)}),
					now:    f.service.config.now, Content: objects.call,
				}}
				credential, _, err := f.service.authenticateAPIToken(t.Context(), f.worker, "", "")
				if err != nil {
					t.Fatal(err)
				}
				request.Files[0].Path, request.Files[0].Patch = "private-path.go", "private-token"
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/diff", bytes.NewReader(raw)).WithContext(service.contentContext(t.Context())), response)
				c.SetPath(nativeBase + "/attempts/:attempt/diff")
				c.SetParamNames("attempt")
				c.SetParamValues(attempt)
				c.Set("native_scope", nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential})
				if err := service.postAttemptDiff(c); err != nil {
					t.Fatal(err)
				}
				failure := requireNativeCode(t, response, test.status, test.code)
				var entry map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
					t.Fatalf("expected exactly one JSON log entry: %v: %s", err, &logs)
				}
				const reason = "Shared diff bodies must arrive through object storage"
				if failure.Message != reason || entry["level"] != "WARN" || entry["refusal_code"] != test.code || entry["reason"] != reason || entry["error"] != reason || entry["route"] != nativeBase+"/attempts/:attempt/diff" {
					t.Fatalf("refusal = %#v, log = %#v", failure, entry)
				}
				for _, private := range []string{"private-path.go", "private-token"} {
					if strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
						t.Fatalf("private diff content escaped: %s", private)
					}
				}
				return
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

func TestAttemptDiffManifest(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	f := claimedAttemptDiffFixture(t, false, Config{now: func() time.Time { return now }})
	f.start(t)
	legacy := *f
	legacy.service = legacyChangeService(t, f.service)
	objects := &changeObjectFixture{}
	f.service.config.Content = objects.call
	files := make([]tracker.AttemptDiffFile, 300)
	for i := range files {
		files[i] = tracker.AttemptDiffFile{Path: fmt.Sprintf("file-%03d.go", i), Status: tracker.DiffStatusModified, Additions: i, Deletions: i / 2, Patch: "@@ preserved\n+change\n"}
	}
	files[1].OldPath = "previous.go"
	files[1].Status = tracker.DiffStatusRenamed
	files[2].Binary = true
	files[3].Truncated = true
	files[3].Patch = ""
	files[4].Path = ".env"
	files[4].Patch = "+private value"
	expected := append([]tracker.AttemptDiffFile(nil), files...)
	expected[2].Patch = ""
	expected[4].Patch, expected[4].Denied = "", true
	response := f.post(t, f.request(1, files...))
	requireNativeStatus(t, response, http.StatusAccepted)
	var receipt tracker.AttemptDiffReceipt
	decodeHubResponse(t, response, &receipt)
	var count int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_diff_files WHERE diff_id=?", receipt.DiffID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("per-file SQL rows=%d err=%v", count, err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_diffs WHERE object_key<>'' AND content_bytes>0").Scan(&count); err != nil || count != 1 {
		t.Fatalf("diff manifests=%d err=%v", count, err)
	}
	var maximum int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT max(length(id)+length(attempt_id)+length(organization_id)+length(project_id)+length(work_item_id)+length(source)+length(source_id)+length(base_sha)+length(head_sha)+length(producer_kind)+length(producer_id)+length(producer_runner_id)+length(producer_lease_id)+length(created_at)+length(object_key)+length(object_sha256)+9*8) FROM attempt_diffs`).Scan(&maximum); err != nil {
		t.Fatal(err)
	}
	if maximum > 2048 {
		t.Fatalf("attempt_diffs row uses %d bytes", maximum)
	}
	objects.mu.Lock()
	objectCount := len(objects.objects)
	objects.mu.Unlock()
	if objectCount != 1 {
		t.Fatalf("stored %d objects, want one manifest for 300 files", objectCount)
	}
	response = f.get(t, "")
	requireNativeStatus(t, response, http.StatusOK)
	var diff tracker.AttemptDiff
	decodeHubResponse(t, response, &diff)
	want := tracker.AttemptDiff{ID: receipt.DiffID, AttemptID: f.attempt, Producer: f.producer(), Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Files: expected, FileCount: 300, PatchBytes: 297 * int64(len("@@ preserved\n+change\n")), Truncated: true, CreatedAt: diff.CreatedAt}
	if !reflect.DeepEqual(diff, want) {
		t.Fatal("manifest changed legacy files or flags")
	}
	err := legacy.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_diffs(id,attempt_id,organization_id,project_id,work_item_id,source,source_id,seq,base_sha,head_sha,producer_kind,producer_id,producer_runner_id,producer_lease_id,producer_fencing_token,file_count,patch_bytes,posted_bytes,truncated,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, want.ID, want.AttemptID, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, want.Generation.Source, want.Generation.ID, want.Generation.Seq, want.BaseSHA, want.HeadSHA, want.Producer.Kind, want.Producer.ID, want.Producer.RunnerID, want.Producer.LeaseID, want.Producer.FencingToken, want.FileCount, want.PatchBytes, want.PatchBytes, want.Truncated, formatHubTime(want.CreatedAt)); err != nil {
			return err
		}
		for position, file := range expected {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_diff_files(diff_id,position,path,old_path,status,additions,deletions,binary,patch,truncated,denied) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, want.ID, position, file.Path, file.OldPath, file.Status, file.Additions, file.Deletions, file.Binary, file.Patch, file.Truncated, file.Denied); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy.service.config.Content = objects.call
	if _, err := legacy.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET status='succeeded' WHERE id=?", f.attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.service.database.db.ExecContext(t.Context(), "UPDATE leases SET released_at=? WHERE lease_id=?", formatHubTime(now), f.lease.ID); err != nil {
		t.Fatal(err)
	}
	convertFiles := func() error {
		ctx := legacy.service.contentContext(t.Context())
		scope := nativeScope{organization: legacy.project.OrganizationID, project: legacy.project.ID}
		for _, family := range storageConversionFamilies {
			if family.name != "diff_files" {
				continue
			}
			if err := legacy.service.discoverStorageFamily(ctx, family, now); err != nil {
				return err
			}
			return legacy.service.convertStorageFamily(ctx, scope, family, now)
		}
		return errors.New("diff file conversion family is missing")
	}
	for pass, want := range []int{200, 100, 0} {
		if pass == 0 {
			query := &convertingDiffQuery{nativeQueryer: legacy.service.database.reader, convert: convertFiles}
			files, err := readAttemptDiffFiles(legacy.service.contentContext(t.Context()), query, receipt.DiffID)
			if err != nil {
				t.Fatal(err)
			}
			if query.convert != nil {
				t.Fatal("diff read did not interleave with conversion")
			}
			if !reflect.DeepEqual(files, expected) {
				t.Fatalf("conversion during diff read returned %d files, want %d unchanged files", len(files), len(expected))
			}
		} else if err := convertFiles(); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := legacy.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_diff_files WHERE diff_id=?", receipt.DiffID).Scan(&remaining); err != nil || remaining != want {
			t.Fatalf("pass %d remaining=%d want=%d err=%v", pass, remaining, want, err)
		}
		mixed := legacy.get(t, "")
		requireNativeStatus(t, mixed, http.StatusOK)
		if response.Body.String() != mixed.Body.String() {
			t.Fatal("partial legacy conversion changed diff output")
		}
	}
	legacyResponse := legacy.get(t, "")
	requireNativeStatus(t, legacyResponse, http.StatusOK)
	if response.Body.String() != legacyResponse.Body.String() {
		t.Fatal("attempt diff API differs from per-file legacy rows")
	}
	itemResponse := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/diff", f.token, nil)
	requireNativeStatus(t, itemResponse, http.StatusOK)
	var item tracker.WorkItemDiff
	decodeHubResponse(t, itemResponse, &item)
	if item.Diff == nil || !reflect.DeepEqual(*item.Diff, diff) {
		t.Fatal("work-item diff differs from attempt diff")
	}
	legacyItem := performHubAPIRequest(t, legacy.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/diff", f.token, nil)
	requireNativeStatus(t, legacyItem, http.StatusOK)
	if itemResponse.Body.String() != legacyItem.Body.String() {
		t.Fatal("work-item diff API differs from per-file legacy rows")
	}
	operatorCtx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	page, err := (hubOperatorExecutor{f.service}).Execute(operatorCtx, changeToolCall(operatortool.GetAttemptDiff, operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), AttemptID: f.attempt, Limit: 10}))
	if err != nil {
		t.Fatal(err)
	}
	var paged operatortool.ChangeResult
	if err := json.Unmarshal(page.Content, &paged); err != nil {
		t.Fatal(err)
	}
	if paged.Diff == nil || !reflect.DeepEqual(paged.Diff.Files, expected[:10]) {
		t.Fatal("MCP manifest page changed diff files")
	}
	legacyOperatorCtx := changeOperatorContext(t, legacy.service, f.token, string(f.project.OrganizationID))
	for _, tool := range []string{operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff} {
		args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), Limit: 10}
		if tool == operatortool.GetAttemptDiff {
			args.AttemptID = f.attempt
		}
		actual, err := (hubOperatorExecutor{f.service}).Execute(operatorCtx, changeToolCall(tool, args))
		if err != nil {
			t.Fatal(err)
		}
		legacyPage, err := (hubOperatorExecutor{legacy.service}).Execute(legacyOperatorCtx, changeToolCall(tool, args))
		if err != nil {
			t.Fatal(err)
		}
		if string(actual.Content) != string(legacyPage.Content) {
			t.Fatalf("%s differs from per-file legacy rows", tool)
		}
	}
	ctx := f.service.contentContext(t.Context())
	body := diffbody.New(string(f.project.OrganizationID), string(f.project.ID), newNativeID("diff"), 0, "@@ object body")
	referenced := []tracker.AttemptDiffFile{{Path: "object.go", Status: tracker.DiffStatusModified, PatchBody: &body}}
	data, err := json.Marshal(referenced)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareChangeContent(ctx, string(f.project.OrganizationID), data); err != nil {
		t.Fatal(err)
	}
	err = f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		receipt, err = storeAttemptDiff(ctx, tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, f.attempt, string(f.issue.WorkItemID), f.request(2, referenced...), now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := f.service.manifestDiffBodyExists(ctx, body.Key)
	if err != nil || !exists {
		t.Fatalf("manifest body lookup lost live object: %v", err)
	}
	if _, err := f.service.database.db.ExecContext(ctx, "UPDATE attempt_diffs SET manifest_expired=1 WHERE id=?", receipt.DiffID); err != nil {
		t.Fatal(err)
	}
	files, err = readAttemptDiffFiles(ctx, f.service.database.reader, receipt.DiffID)
	if err != nil || len(files) != 1 || !files[0].PatchExpired || files[0].PatchBody != nil {
		t.Fatalf("manifest expiry lost: %#v %v", files, err)
	}
	exists, err = f.service.manifestDiffBodyExists(ctx, body.Key)
	if err != nil || exists {
		t.Fatalf("expired manifest retained body: %v", err)
	}
}
