package hubserver

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type batchFixture struct {
	nativeFixture
	runner   runnerFixture
	batch    *tracker.GitHubBatch
	sequence int
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	f := linkedFixture(t)
	// Exercise the hosted runner-first association, without a GitHub backend.
	_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id=NULL, checkout_repository='acme/orders' WHERE id=?", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := append(f.project.States, tracker.NativeState{Name: "Backlog", OperatorOnly: true, Transitions: []string{"Todo"}})
	raw, err := marshalNative(states)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", raw, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?, 'Backlog','Backlog',0,0,?,?)", f.project.ID, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	runner.enroll(t)
	fixture := &batchFixture{nativeFixture: f, runner: runner}
	fixture.heartbeat(t)
	return fixture
}
func (f *batchFixture) heartbeat(t *testing.T) *tracker.GitHubBatchTask {
	t.Helper()
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential, map[string]any{"version": "test", "capacity": 1, "display_name": "runner", "checkout_repository": "acme/orders", "local_checks": runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}})
	requireNativeStatus(t, r, http.StatusOK)
	var response runnerauth.RoutingSnapshot
	decodeHubResponse(t, r, &response)
	return response.GitHubIntake
}
func (f *batchFixture) command(t *testing.T, request tracker.GitHubBatchCommand, status int) {
	t.Helper()
	f.sequence++
	request.IdempotencyKey = fmt.Sprint("command-", f.sequence)
	if f.batch != nil {
		request.Revision = f.batch.Revision
	}
	if request.RunnerID == "" {
		request.RunnerID = f.runner.binding.RunnerID
	}
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake", testHubAdminToken, request)
	requireNativeStatus(t, r, status)
	if status == http.StatusOK {
		var view githubBatchView
		decodeHubResponse(t, r, &view)
		f.batch = view.Batch
	}
}
func (f *batchFixture) report(t *testing.T, request tracker.GitHubBatchResult, status int) {
	t.Helper()
	f.sequence++
	request.IdempotencyKey = fmt.Sprint("result-", f.sequence)
	request.BatchID = f.batch.ID
	request.Revision = f.batch.Revision
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", f.runner.redemption.Credential, request)
	requireNativeStatus(t, r, status)
	if status == http.StatusOK {
		var view githubBatchView
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding/issue-intake", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &view)
		f.batch = view.Batch
	}
}
func previewIssue(number int, closed bool) tracker.GitHubIssuePreview {
	return tracker.GitHubIssuePreview{Number: number, ID: fmt.Sprint("I_", number), URL: fmt.Sprintf("https://github.com/acme/orders/issues/%d", number), Title: fmt.Sprint("Source ", number), Body: "Preview", Closed: closed, Labels: []string{"enhancement"}}
}
func (f *batchFixture) preview(t *testing.T, closed bool) {
	t.Helper()
	f.command(t, tracker.GitHubBatchCommand{Action: "discover", IncludeClosed: closed}, http.StatusOK)
	if task := f.heartbeat(t); task == nil || task.Discovery == nil || task.Discovery.IncludeClosed != closed {
		t.Fatalf("discovery task = %#v", task)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var setup onboarding.Project
	decodeHubResponse(t, response, &setup)
	if len(setup.Runners) != 1 || setup.Runners[0].LocalChecks == nil || !setup.Runners[0].LocalChecks.Passed() || setup.Runners[0].LocalChecks.ObservedAt.IsZero() {
		t.Fatalf("heartbeat lost local checks while delivering intake: %+v", setup.Runners)
	}
	issues := []tracker.GitHubIssuePreview{previewIssue(12, false), previewIssue(13, closed), previewIssue(14, false)}
	f.report(t, tracker.GitHubBatchResult{Page: &tracker.GitHubDiscoveryPage{Issues: issues, Total: 3}}, http.StatusOK)
}
func batchSnapshot(number int) tracker.GitHubIssueSnapshot {
	snapshot := linkedSnapshot()
	snapshot.URL = previewIssue(number, false).URL
	snapshot.Provenance.ExternalID = previewIssue(number, false).ID
	snapshot.Comments[0].Provenance.ExternalID = fmt.Sprint("C_", number)
	return snapshot
}

func TestGitHubBatchIntakeRetryAndNativeOwnership(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		closed    bool
		rateLimit bool
	}{{name: "partial import and native edit"}, {name: "429 after first import", rateLimit: true}, {name: "explicit mixed open and closed history", closed: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			f.preview(t, test.closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 12, 13, 14}, Destination: "Todo"}, http.StatusUnprocessableEntity)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 12, 13, 14}}, http.StatusOK)
			if len(f.batch.Items) != 3 || f.batch.Destination != "Backlog" {
				t.Fatalf("selection = %#v", f.batch)
			}
			snapshot := batchSnapshot(12)
			f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: &snapshot}, http.StatusOK)
			if task := f.heartbeat(t); task == nil || task.Item.Number != 13 {
				t.Fatalf("next intake = %#v", task)
			}
			deadline := ""
			if test.rateLimit {
				deadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			}
			f.report(t, tracker.GitHubBatchResult{Number: 13, Error: "Source context incomplete: private access or partial page", RetryAt: deadline}, http.StatusOK)
			if !test.rateLimit {
				snapshot = batchSnapshot(14)
				f.report(t, tracker.GitHubBatchResult{Number: 14, Snapshot: &snapshot}, http.StatusOK)
			}
			if task := f.heartbeat(t); task != nil {
				t.Fatalf("failed intake retried without operator: %#v", task)
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, Config{DatabasePath: f.service.config.DatabasePath})
			f.runner.service = f.service
			responseAfterRestart := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding/issue-intake", testHubAdminToken, nil)
			requireNativeStatus(t, responseAfterRestart, http.StatusOK)
			var durable githubBatchView
			decodeHubResponse(t, responseAfterRestart, &durable)
			if durable.Batch.Revision != f.batch.Revision || durable.Batch.Items[0].Status != "completed" {
				t.Fatalf("lost intake checkpoints after restart: %#v", durable.Batch)
			}
			f.batch = durable.Batch
			id := f.batch.Items[1].WorkItemID
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var native tracker.NativeIssue
			decodeHubResponse(t, response, &native)
			title, body := "Native task title", "Native body during retry"
			edit := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "native-edit"}, ExpectedRevision: native.Revision, Title: &title, Body: &body}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(id), f.token, edit), http.StatusOK)
			if test.rateLimit {
				f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusUnprocessableEntity)
				// Move the recorded source deadline into the past without sleeping.
				for i := range f.batch.Items {
					f.batch.Items[i].RetryAt = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
				}
				raw, err := marshalNative(f.batch)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.service.database.db.ExecContext(t.Context(), "UPDATE onboarding_issue_intake SET request_json=? WHERE project_id=?", raw, f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
			snapshot = batchSnapshot(13)
			f.report(t, tracker.GitHubBatchResult{Number: 13, Snapshot: &snapshot}, http.StatusOK)
			if test.rateLimit {
				snapshot = batchSnapshot(14)
				f.report(t, tracker.GitHubBatchResult{Number: 14, Snapshot: &snapshot}, http.StatusOK)
			}
			for range 3 {
				if task := f.heartbeat(t); task != nil {
					t.Fatalf("completed intake still requesting GitHub: %#v", task)
				}
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
			decodeHubResponse(t, response, &native)
			if native.Title != title || native.Body != body || native.State != "Backlog" || native.LinkedSource.Status != "complete" {
				t.Fatalf("native ownership = %#v", native)
			}
			var comments, count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", id).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if comments != 1 || count != 3 {
				t.Fatalf("comments=%d issues=%d", comments, count)
			}
			// A subsequent selected batch reuses both URL and stable identities.
			f.preview(t, test.closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13, 14}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
			for _, item := range f.batch.Items {
				if item.Status != "skipped" {
					t.Fatalf("duplicate = %#v", item)
				}
			}
		})
	}
}

func TestGitHubBatchDiscoveryFailureAndSourceValidation(t *testing.T) {
	t.Parallel()
	f := newBatchFixture(t)
	f.command(t, tracker.GitHubBatchCommand{Action: "discover", Labels: []string{"enhancement"}}, http.StatusOK)
	f.report(t, tracker.GitHubBatchResult{Error: "Private repository inaccessible; verify runner read access"}, http.StatusOK)
	if task := f.heartbeat(t); task != nil {
		t.Fatal("private access failure caused idle polling")
	}
	f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
	f.report(t, tracker.GitHubBatchResult{Page: &tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{previewIssue(12, true)}}}, http.StatusUnprocessableEntity)
	f.report(t, tracker.GitHubBatchResult{Page: &tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{previewIssue(12, false)}}}, http.StatusOK)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{999}}, http.StatusUnprocessableEntity)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
	snapshot := batchSnapshot(12)
	snapshot.Provenance.ExternalID = "I_wrong"
	f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: &snapshot}, http.StatusUnprocessableEntity)
	other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
	other.enroll(t)
	request := tracker.GitHubBatchResult{Mutation: tracker.Mutation{IdempotencyKey: "wrong-runner"}, BatchID: f.batch.ID, Revision: f.batch.Revision, Number: 12, Snapshot: &snapshot}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/issue-intake/result", other.redemption.Credential, request), http.StatusNotFound)
}

func TestGitHubBatchSkipsLinkedAndLegacyImportedIdentity(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"linked URL", "legacy source key", "stable provenance identity"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			var existing tracker.NativeIssue
			if mode == "linked URL" {
				existing = f.link(t, "prior-linked")
			} else {
				snapshot := batchSnapshot(12)
				r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "prior-import"}, Title: "Native edits on old import", Body: "Preserved", State: "Backlog", Provenance: &snapshot.Provenance})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &existing)
				if mode == "stable provenance identity" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_source_key='github:old-url' WHERE native_id=?", existing.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.preview(t, false)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
			if len(f.batch.Items) != 1 || f.batch.Items[0].Status != "skipped" || f.batch.Items[0].WorkItemID != existing.WorkItemID {
				t.Fatalf("duplicate result = %#v", f.batch.Items)
			}
			if task := f.heartbeat(t); task != nil {
				t.Fatal("existing source was fetched again")
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("issues=%d err=%v", count, err)
			}
		})
	}
}

func TestGitHubBatchHonorsFirstRunIntakeWithoutAnotherSourceRead(t *testing.T) {
	t.Parallel()
	f := newBatchFixture(t)
	f.preview(t, false)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Todo", AllowDispatch: true}, http.StatusOK)
	item := f.batch.Items[0]
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/source-intake", testHubAdminToken, tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "first-run-intake"}, Snapshot: batchSnapshot(12)})
	requireNativeStatus(t, r, http.StatusOK)
	if task := f.heartbeat(t); task != nil {
		t.Fatalf("one-time hydration already complete; task=%#v", task)
	}
	r = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding/issue-intake", testHubAdminToken, nil)
	var view githubBatchView
	decodeHubResponse(t, r, &view)
	if view.Batch.Status != "finished" || view.Batch.Items[0].Status != "completed" {
		t.Fatalf("batch did not retain existing completion: %#v", view)
	}
}

func TestGitHubBatchExplicitRunnerReassignmentKeepsPendingSelection(t *testing.T) {
	t.Parallel()
	f := newBatchFixture(t)
	f.preview(t, false)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
	original := f.runner
	other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
	other.enroll(t)
	f.runner = other
	f.heartbeat(t)
	f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
	if task := f.heartbeat(t); task == nil || task.Item.Number != 12 {
		t.Fatalf("reassigned pending selection: %#v", task)
	}
	f.runner = original
	if task := f.heartbeat(t); task != nil {
		t.Fatal("original runner still receiving intake after reassignment")
	}
}

func TestGitHubBatchImportsDiscussionBeyondDefaultAPILimit(t *testing.T) {
	t.Parallel()
	f := newBatchFixture(t)
	f.preview(t, false)
	f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
	snapshot := batchSnapshot(12)
	comment := snapshot.Comments[0]
	snapshot.Comments = nil
	for i := range 40 {
		comment.Provenance.ExternalID = fmt.Sprint("C_large_", i)
		comment.Body = string(make([]byte, 32<<10))
		snapshot.Comments = append(snapshot.Comments, comment)
	}
	f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: &snapshot}, http.StatusOK)
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", f.batch.Items[0].WorkItemID).Scan(&count); err != nil || count != 40 {
		t.Fatalf("discussion count=%d error=%v", count, err)
	}
}
