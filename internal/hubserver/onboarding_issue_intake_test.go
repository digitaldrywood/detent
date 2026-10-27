package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	f.service.config.ImportBackend = nil
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE repositories SET github_owner='example-org', github_name='example-project' WHERE github_node_id='R_repo'"); err != nil {
		t.Fatal(err)
	}
	// Exercise the hosted runner-first association, without a GitHub backend.
	_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id=NULL, checkout_repository='example-org/example-project', github_intake='manual' WHERE id=?", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := append(f.project.States, tracker.NativeState{Name: "Backlog", OperatorOnly: true, Transitions: []string{"Todo"}}, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
	raw, err := marshalNative(states)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", raw, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?, 'Backlog','Backlog',0,0,?,?), (?, 'Blocked','Blocked',0,0,?,?)", f.project.ID, testTimestamp, testTimestamp, f.project.ID, testTimestamp, testTimestamp)
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
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential, map[string]any{"version": "test", "capacity": 1, "display_name": "runner", "checkout_repository": "example-org/example-project", "local_checks": runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}})
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
	return tracker.GitHubIssuePreview{Number: number, ID: fmt.Sprint("I_", number), URL: fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", number), Title: fmt.Sprint("Source ", number), Body: "Preview", Closed: closed, Labels: []string{"enhancement"}}
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
		name           string
		closed         bool
		rateLimit      bool
		mcp            bool
		importDisabled bool
		projectLane    string
		unmapped       bool
	}{{name: "partial import and native edit"}, {name: "429 after first import", rateLimit: true}, {name: "explicit mixed open and closed history", closed: true}, {name: "MCP bounded intake receipts and authority", mcp: true}, {name: "manual import disabled", importDisabled: true}, {name: "corrected project mapping for precreated issue", projectLane: "Backlog"}, {name: "corrected dispatchable project mapping requires confirmation", projectLane: "Todo"}, {name: "corrected unmapped project status uses destination", projectLane: "Backlog", unmapped: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			if test.projectLane == "" {
				f.preview(t, test.closed)
			} else {
				source := &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}
				f.command(t, tracker.GitHubBatchCommand{Action: "discover", ProjectV2: source}, http.StatusOK)
				page := tracker.GitHubDiscoveryPage{Total: 3}
				for _, number := range []int{12, 13, 14} {
					preview := previewIssue(number, false)
					preview.ProjectStatus = "Ready"
					page.Issues = append(page.Issues, preview)
				}
				f.report(t, tracker.GitHubBatchResult{Page: &page}, http.StatusOK)
			}
			if test.importDisabled {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET github_intake='disabled' WHERE id=?", f.project.ID); err != nil {
					t.Fatal(err)
				}
				f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}, Destination: "Backlog"}, http.StatusUnprocessableEntity)
				var linked int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM issues WHERE project_id=? AND github_number=12", f.project.ID).Scan(&linked); err != nil {
					t.Fatal(err)
				}
				if linked != 0 || f.heartbeat(t) != nil {
					t.Fatalf("disabled import created %d issues or scheduled runner intake", linked)
				}
				return
			}
			if test.mcp {
				captureds := make(chan context.Context, 1)
				f.service.echo.GET("/api/v2/organizations/:organization/mcp-batch-fixture", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
				path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp-batch-fixture"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
				captured := <-captureds
				connect := func(id string) context.Context {
					ctx := operatortool.BindConnection(captured, id, "batch fixture")
					if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
						t.Fatal(err)
					}
					return ctx
				}
				ctx := connect("batch-tools")
				e := hubProjectExecutor{f.service}
				f.batch.Error = "provider credential-secret"
				f.batch.Page.Issues[0].Body = strings.Repeat("preview body", 30000)
				raw, err := json.Marshal(f.batch)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE onboarding_issue_intake SET request_json=? WHERE project_id=?", raw, f.project.ID); err != nil {
					t.Fatal(err)
				}
				input := operatortool.GitHubBatchInput{Revision: f.batch.Revision, Action: "apply", Numbers: []int{12, 13, 14}, Destination: "Backlog"}
				input.Destination = "Todo"
				var refusal *operatortool.RequestError
				if _, err := e.Execute(ctx, projectCall(t, "command_git_hub_batch", string(f.project.ID), "dispatch-preview", input)); !errors.As(err, &refusal) || refusal.Code != "invalid_request" || refusal.Message != "Select a destination and explicitly confirm any dispatchable lane" {
					t.Fatalf("unconfirmed dispatch=%v", err)
				}
				input.Destination = "Backlog"
				call := projectCall(t, "command_git_hub_batch", string(f.project.ID), "batch-apply", input)
				a := projectAction(t, e, ctx, call)
				if a.Status != chatpkg.ActionSucceeded || strings.Contains(a.Result, "credential-secret") || strings.Contains(a.Result, `"body"`) {
					t.Fatalf("batch apply=%+v", a)
				}
				if replay := projectAction(t, e, connect("batch-reconnected"), call); replay.Result != a.Result {
					t.Fatalf("batch replay=%+v", replay)
				}
				input.Numbers = []int{12}
				if _, err := e.Execute(connect("batch-conflict"), projectCall(t, "command_git_hub_batch", string(f.project.ID), "batch-apply", input)); !errors.Is(err, mutation.ErrConflict) {
					t.Fatalf("changed batch replay=%v", err)
				}
				read := operatortool.Call{Name: "get_git_hub_batch", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","limit":1}`)}
				result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read)
				if err != nil || strings.Contains(string(result.Content), "credential-secret") || strings.Contains(string(result.Content), `"body"`) || !strings.Contains(string(result.Content), `"next_cursor":"1"`) {
					t.Fatalf("batch page=%s %v", result.Content, err)
				}
				var envelope struct {
					Data projectBatchPage `json:"data"`
				}
				if json.Unmarshal(result.Content, &envelope) != nil || len(envelope.Data.Batch.Items) != 1 || len(envelope.Data.Batch.Page.Issues) != 1 {
					t.Fatalf("batch bounds=%s", result.Content)
				}
				preview := envelope.Data.Batch.Page.Issues[0]
				if preview.Number != f.batch.Page.Issues[0].Number || preview.Title != f.batch.Page.Issues[0].Title || preview.Status != "open" {
					t.Fatalf("batch preview=%+v", preview)
				}
				stored, err := readGitHubBatch(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
				if err != nil || stored.Page.Issues[0].Body != strings.Repeat("preview body", 30000) {
					t.Fatalf("stored batch preview body changed: %v", err)
				}
				if _, err := e.Execute(ctx, projectCall(t, "command_git_hub_batch", string(f.project.ID), "stale-batch", input)); !errors.Is(err, mutation.ErrConflict) {
					t.Fatalf("stale batch=%v", err)
				}
				other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-batch")
				read.Arguments = json.RawMessage(`{"project_id":"` + string(other.project.ID) + `"}`)
				result, err = e.Execute(ctx, read)
				if err != nil || strings.Contains(string(result.Content), f.batch.ID) {
					t.Fatalf("cross-project batch=%s %v", result.Content, err)
				}
				return
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 12, 13, 14}, Destination: "Todo"}, http.StatusUnprocessableEntity)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 12, 13, 14}, Destination: "Backlog"}, http.StatusOK)
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
			if test.projectLane != "" {
				invalidSnapshot := batchSnapshot(13)
				invalidSnapshot.Comments = append(invalidSnapshot.Comments, invalidSnapshot.Comments[0])
				f.report(t, tracker.GitHubBatchResult{Number: 13, Snapshot: &invalidSnapshot}, http.StatusOK)
				if f.batch.Items[1].Status != "failed" || f.batch.Items[1].WorkItemID == "" {
					t.Fatalf("expected failed precreated import: %+v", f.batch.Items[1])
				}
			} else {
				f.report(t, tracker.GitHubBatchResult{Number: 13, Error: "Source context incomplete: private access or partial page", RetryAt: deadline}, http.StatusOK)
			}
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
			if test.projectLane == "Todo" {
				body += issueContractTestSections
			}
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
			retry := tracker.GitHubBatchCommand{Action: "retry"}
			if test.projectLane != "" {
				retry.ProjectV2 = &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": test.projectLane}}
				if test.unmapped {
					retry.ProjectV2.StatusLanes = map[string]string{"Other": "Blocked"}
				}
				if test.projectLane == "Todo" {
					f.command(t, retry, http.StatusUnprocessableEntity)
					response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
					decodeHubResponse(t, response, &native)
					if native.State != "Blocked" || f.batch.Items[1].Status != "failed" || f.heartbeat(t) != nil {
						t.Fatalf("unconfirmed retry changed routing: issue=%+v item=%+v", native, f.batch.Items[1])
					}
					retry.AllowDispatch = true
				}
			}
			f.command(t, retry, http.StatusOK)
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
			wantLane := "Backlog"
			if test.projectLane != "" {
				wantLane = test.projectLane
				for _, index := range []int{0, 2} {
					completed, _, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.batch.Items[index].WorkItemID))
					if err != nil || completed.State != "Blocked" || f.batch.Items[index].Status != "completed" {
						t.Fatalf("corrected mapping changed completed issue: %+v error=%v", completed, err)
					}
				}
				unmapped := f.batch.Items[1].UnmappedProjectStatus
				if (unmapped != nil) != test.unmapped || test.unmapped && *unmapped != "Ready" {
					t.Fatalf("corrected mapping report=%+v", f.batch.Items[1])
				}
			}
			if native.WorkItemID != id || native.Title != title || native.Body != body || native.State != wantLane || native.LinkedSource.Status != "complete" {
				t.Fatalf("native ownership = %#v item=%+v", native, f.batch.Items[1])
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

func TestGitHubBatchRunnerOperations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		known      any
		operations string
		heartbeat  bool
		status     int
	}{
		{"typed grant overrides payload refusal", true, `[]`, true, http.StatusOK},
		{"typed refusal overrides payload grant", true, `["heartbeat"]`, false, http.StatusUnprocessableEntity},
		{"unknown operations evaluate legacy grant", nil, `["heartbeat"]`, false, http.StatusOK},
		{"unknown operations evaluate legacy refusal", nil, `[]`, false, http.StatusUnprocessableEntity},
		{"unknown operations ignore incomplete typed grant", nil, `[]`, true, http.StatusUnprocessableEntity},
		{"unknown malformed operations refuse", nil, `[1]`, true, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			d := f.service.database
			if _, err := d.db.ExecContext(t.Context(), `DELETE FROM runner_operations WHERE runner_id=? AND operation='heartbeat'`, f.runner.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if test.heartbeat {
				if _, err := d.db.ExecContext(t.Context(), `INSERT INTO runner_operations(runner_id,operation) VALUES(?,'heartbeat')`, f.runner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.db.ExecContext(t.Context(), `UPDATE runner_identities SET operations_known=?,operations_json=? WHERE id=?`, test.known, test.operations, f.runner.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "discover"}, test.status)
			batch, err := readGitHubBatch(t.Context(), d.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil {
				t.Fatal(err)
			}
			if test.status == http.StatusOK {
				if batch == nil || batch.RunnerID != f.runner.binding.RunnerID || batch.Status != "discovering" {
					t.Fatalf("authorized runner did not receive discovery: %+v", batch)
				}
			} else if batch != nil {
				t.Fatalf("refused runner created a batch: %+v", batch)
			}
		})
	}
}

func TestGitHubBatchSkipsLinkedAndLegacyImportedIdentity(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"linked URL", "legacy source key", "stable provenance identity", "unknown provenance identity"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			var existing tracker.NativeIssue
			if mode == "linked URL" {
				f.service.config.ImportBackend = linkedTestImporter{snapshot: batchSnapshot(12)}
				r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "prior-linked"}, GitHubIssueURL: previewIssue(12, false).URL})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &existing)
				f.service.config.ImportBackend = nil
			} else {
				snapshot := batchSnapshot(12)
				r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "prior-import"}, Title: "Native edits on old import", Body: "Preserved", State: "Backlog", Provenance: &snapshot.Provenance})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &existing)
				if mode == "stable provenance identity" || mode == "unknown provenance identity" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_source_key='github:old-url' WHERE native_id=?", existing.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "unknown provenance identity" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET provenance_provider=NULL,provenance_external_id=NULL WHERE native_id=?`, existing.WorkItemID); err != nil {
					t.Fatal(err)
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

func TestGitHubBatchAppPagesAndPartialRetry(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		fail    bool
		restart bool
	}{{name: "100 issue page"}, {name: "invalid issue rolls back and retries", fail: true}, {name: "restart leaves import for background maintenance", restart: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			reads := 0
			f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
				reads++
				if request.Stage == "issue" {
					snapshot := batchSnapshot(request.IssueNumber)
					return GitHubImportPage{Issue: &IssueSource{URL: snapshot.URL, NodeID: snapshot.Provenance.ExternalID, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
				}
				if request.Stage == "comments" {
					return GitHubImportPage{}, nil
				}
				if request.Stage != "batch" || request.Discovery == nil || !request.Discovery.Snapshots {
					t.Fatalf("source request=%+v", request)
				}
				page := tracker.GitHubDiscoveryPage{Total: 101}
				first, last := 1, 100
				if request.Discovery.Cursor != "" {
					first, last = 101, 101
				} else {
					page.NextCursor = "page2"
				}
				for number := first; number <= last; number++ {
					preview := previewIssue(number, false)
					snapshot := batchSnapshot(number)
					snapshot.Body = strings.Repeat("complete-source-body", 100) + issueContractTestSections
					preview.Snapshot = &snapshot
					preview.Labels = []string{"bug", "phase:one"}
					if number%2 == 0 {
						preview.Labels = append(preview.Labels, "detent:backlog")
					} else {
						preview.Labels = append(preview.Labels, "detent:todo")
					}
					if test.fail && number == 50 {
						snapshot.Body = strings.Repeat("x", (256<<10)+1)
					}
					page.Issues = append(page.Issues, preview)
				}
				return GitHubImportPage{BatchPage: &page}, nil
			})
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", AllowDispatch: true}, http.StatusOK)
			if test.restart {
				cfg := f.service.config
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				cfg.WebhookMaintenanceInterval = time.Hour
				f.service = openTestService(t, cfg)
				if reads != 0 || !f.service.ready.Load() {
					t.Fatalf("startup performed import work before readiness: reads=%d ready=%t", reads, f.service.ready.Load())
				}
			}
			if task := f.heartbeat(t); task != nil {
				t.Fatal("App-owned import assigned to runner")
			}
			if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			batch, err := readGitHubBatch(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			f.batch = batch
			if batch.Status != "finished" || len(batch.Items) != 101 || reads != 2 {
				t.Fatalf("progress=%s items=%d source reads=%d", batch.Status, len(batch.Items), reads)
			}
			for _, item := range batch.Items {
				if test.fail && item.Number == 50 {
					if item.Status != "failed" || item.WorkItemID != "" || item.Error == "" {
						t.Fatalf("failed item=%+v", item)
					}
					continue
				}
				issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(item.WorkItemID))
				if err != nil {
					t.Fatalf("item=%+v: %v", item, err)
				}
				lane := "Todo"
				if item.Number%2 == 0 {
					lane = "Backlog"
				}
				if item.Status != "completed" || issue.State != lane || len(issue.Labels) != 2 || issue.Labels[0] != "bug" || issue.Labels[1] != "phase:one" || !strings.Contains(issue.Body, "complete-source-body") {
					t.Fatalf("item=%+v issue=%+v", item, issue)
				}
			}
			response, err := f.service.projectGitHubBatch(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "complete-source-body") || strings.Contains(string(raw), `"snapshot":`) {
				t.Fatal("batch read echoed issue bodies")
			}
			if test.fail {
				f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusOK)
				if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
					t.Fatal(err)
				}
				batch, err = readGitHubBatch(t.Context(), f.service.database.db, scope)
				if err != nil {
					t.Fatal(err)
				}
				if batch.Items[49].Status != "completed" || reads != 4 {
					t.Fatalf("retry item=%+v reads=%d", batch.Items[49], reads)
				}
				f.batch = batch
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", AllowDispatch: true}, http.StatusOK)
			if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			batch, err = readGitHubBatch(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range batch.Items {
				if item.Status != "skipped" {
					t.Fatalf("rerun duplicated item=%+v", item)
				}
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 101 {
				t.Fatalf("duplicate issues=%d", count)
			}
		})
	}
}

func TestGitHubBatchLabelLaneConfirmation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		project       *tracker.GitHubProjectV2Source
		projectStatus string
		fallback      bool
		selective     bool
		rejected      bool
		unmapped      bool
		labels        []string
		mapping       map[string]string
		allow         bool
		ignore        bool
		lane          string
	}{
		{name: "mapped project status", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Ready", lane: "Blocked"},
		{name: "unmapped project status", labels: []string{"bug", "detent:blocked"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Unknown", unmapped: true, lane: "Backlog"},
		{name: "missing project status", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, unmapped: true, lane: "Backlog"},
		{name: "project status overrides lane label", labels: []string{"bug", "detent:blocked"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Todo"}}, projectStatus: "Ready", allow: true, lane: "Todo"},
		{name: "dispatchable project mapping requires authorization", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Todo"}}, projectStatus: "Ready", rejected: true},
		{name: "runner fallback project mapping", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Ready", fallback: true, lane: "Blocked"},
		{name: "selected project mapping", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Ready", fallback: true, selective: true, lane: "Blocked"},
		{name: "selected unmapped project status", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}, projectStatus: "Unknown", unmapped: true, fallback: true, selective: true, lane: "Backlog"},
		{name: "unknown project lane refused", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Unknown"}}, rejected: true},
		{name: "missing project identifier refused", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{StatusLanes: map[string]string{"Ready": "Blocked"}}, rejected: true},
		{name: "missing project mappings refused", labels: []string{"bug"}, project: &tracker.GitHubProjectV2Source{ID: "PVT_source"}, rejected: true},
		{name: "default destination", labels: []string{"bug"}, lane: "Backlog"},
		{name: "lane label", labels: []string{"bug", "detent:todo"}, allow: true, lane: "Todo"},
		{name: "hyphenated lane label", labels: []string{"bug", "detent:in-progress"}, allow: true, lane: "In Progress"},
		{name: "dispatchable lane label without authorization stays in destination", labels: []string{"bug", "detent:todo"}, lane: "Backlog"},
		{name: "holding lane label without authorization", labels: []string{"bug", "detent:blocked"}, lane: "Blocked"},
		{name: "ignored lane labels", labels: []string{"bug", "detent:blocked"}, mapping: map[string]string{"detent:blocked": "Blocked"}, ignore: true, lane: "Backlog"},
		{name: "custom mapping", labels: []string{"incoming", "bug"}, mapping: map[string]string{"incoming": "Todo"}, allow: true, lane: "Todo"},
		{name: "conflicting lane labels stay in destination", labels: []string{"bug", "detent:todo", "detent:blocked"}, allow: true, lane: "Backlog"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			preview := previewIssue(12, false)
			snapshot := batchSnapshot(12)
			preview.Labels, preview.Snapshot, preview.ProjectStatus = test.labels, &snapshot, test.projectStatus
			page := tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{preview}}
			if !test.fallback {
				f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
					if request.Discovery == nil || (request.Discovery.ProjectV2 == nil) != (test.project == nil) {
						t.Fatalf("discovery lost project source: %+v", request.Discovery)
					}
					return GitHubImportPage{BatchPage: &page}, nil
				})
			}
			command := tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog", AllowDispatch: test.allow, LabelLanes: test.mapping, IgnoreLanes: test.ignore, ProjectV2: test.project}
			status := http.StatusOK
			if test.rejected {
				status = http.StatusUnprocessableEntity
			}
			if test.selective {
				command.Action = "discover"
			}
			f.command(t, command, status)
			if test.rejected {
				var imported int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM issues WHERE project_id=? AND github_number=12", f.project.ID).Scan(&imported); err != nil {
					t.Fatal(err)
				}
				if imported != 0 || f.heartbeat(t) != nil {
					t.Fatalf("refused mapping imported %d issues or scheduled work", imported)
				}
				return
			}
			if test.fallback {
				task := f.heartbeat(t)
				if task == nil || task.Discovery == nil || task.Discovery.ProjectV2 == nil || task.Discovery.ProjectV2.ID != "PVT_source" || task.Discovery.ProjectV2.StatusLanes["Ready"] != "Blocked" {
					t.Fatalf("runner discovery lost mapping: %+v", task)
				}
				f.report(t, tracker.GitHubBatchResult{Page: &page}, http.StatusOK)
				if test.selective {
					f.command(t, tracker.GitHubBatchCommand{Action: "apply", Destination: "Backlog", Numbers: []int{12}}, http.StatusOK)
				}
				f.report(t, tracker.GitHubBatchResult{Results: []tracker.GitHubBatchItemResult{{Number: 12, Snapshot: &snapshot}}}, http.StatusOK)
			} else if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			batch, err := readGitHubBatch(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			item := batch.Items[0]
			if (item.UnmappedProjectStatus != nil) != test.unmapped || test.unmapped && *item.UnmappedProjectStatus != test.projectStatus {
				t.Fatalf("unmapped Status report: %+v", item)
			}
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(item.WorkItemID))
			if err != nil {
				t.Fatalf("item=%+v: %v", item, err)
			}
			wantLabels := []string{"bug"}
			if test.labels[0] == "incoming" {
				wantLabels = []string{"incoming", "bug"}
			}
			if issue.State != test.lane || !slices.Equal(issue.Labels, wantLabels) {
				t.Fatalf("mapped issue=%+v", issue)
			}
		})
	}
}

func TestGitHubBatchAppFallbackAndThrottle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		sourceErr      error
		fallback       bool
		projection     bool
		owned          any
		status         any
		payloadUnowned bool
		payloadStatus  string
		skipped        bool
		malformed      bool
	}{
		{name: "App lacks repository access", sourceErr: githubconnector.ErrNotFound, fallback: true},
		{name: "App throttle retains deadline", sourceErr: &githubconnector.StatusError{StatusCode: 429, RetryAfter: time.Hour, Err: githubconnector.ErrRateLimited}},
		{name: "typed ownership refusal overrides active payload", projection: true, owned: false, status: "discovering", skipped: true},
		{name: "typed status refusal overrides active payload", projection: true, owned: true, status: "finished", skipped: true},
		{name: "unknown status preserves typed ownership refusal", projection: true, owned: false, skipped: true},
		{name: "unknown ownership preserves typed status refusal", projection: true, status: "finished", skipped: true},
		{name: "unknown projections evaluate active payload", projection: true, sourceErr: githubconnector.ErrNotFound, fallback: true},
		{name: "unknown ownership evaluates active payload", projection: true, status: "discovering", sourceErr: githubconnector.ErrNotFound, fallback: true},
		{name: "unknown status evaluates active payload", projection: true, owned: true, sourceErr: githubconnector.ErrNotFound, fallback: true},
		{name: "unknown ownership evaluates payload refusal", projection: true, status: "discovering", payloadUnowned: true, skipped: true},
		{name: "unknown status evaluates payload refusal", projection: true, owned: true, payloadStatus: "finished", skipped: true},
		{name: "typed importing status selects batch", projection: true, owned: true, status: "importing", sourceErr: githubconnector.ErrNotFound, fallback: true},
		{name: "unknown projections evaluate ownership refusal", projection: true, payloadUnowned: true, skipped: true},
		{name: "unknown projections evaluate status refusal", projection: true, payloadStatus: "finished", skipped: true},
		{name: "unknown malformed projections refuse evaluation", projection: true, malformed: true, skipped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			reads := 0
			f.service.config.ImportBackend = scheduledContractImporter(func(context.Context, GitHubImportRequest) (GitHubImportPage, error) {
				reads++
				return GitHubImportPage{}, test.sourceErr
			})
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog"}, http.StatusOK)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			batch, err := readGitHubBatch(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			batch.RunnerID = ""
			if test.payloadUnowned {
				batch.HubOwned = false
			}
			if test.payloadStatus != "" {
				batch.Status = test.payloadStatus
			}
			raw, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			if test.malformed {
				raw = []byte(`{"hub_owned":"unknown","status":"discovering"}`)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE onboarding_issue_intake SET request_json=? WHERE project_id=?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if test.projection {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE onboarding_issue_intake SET hub_owned=?,batch_status=? WHERE project_id=?", test.owned, test.status, f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.maintainGitHubBatches(t.Context()); (err != nil) != test.malformed {
				t.Fatalf("maintenance error=%v, want malformed refusal=%t", err, test.malformed)
			}
			if test.skipped {
				var stored string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT request_json FROM onboarding_issue_intake WHERE project_id=?", f.project.ID).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if reads != 0 || stored != string(raw) {
					t.Fatalf("ineligible batch advanced: reads=%d batch=%s", reads, stored)
				}
				return
			}
			batch, err = readGitHubBatch(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			f.batch = batch
			task := f.heartbeat(t)
			if test.fallback {
				if batch.HubOwned || batch.RunnerID != f.runner.binding.RunnerID || task == nil || task.Discovery == nil {
					t.Fatalf("fallback=%+v task=%+v", batch, task)
				}
			} else {
				if !batch.HubOwned || batch.Status != "discovery_failed" || batch.RetryAt == "" || task != nil {
					t.Fatalf("throttle=%+v task=%+v", batch, task)
				}
				f.command(t, tracker.GitHubBatchCommand{Action: "retry"}, http.StatusUnprocessableEntity)
				if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if reads != 1 {
				t.Fatalf("unexpected source retries=%d", reads)
			}
		})
	}
}

func TestGitHubBatchMaintenanceTraversal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, intake, payload             string
		legacy, malformed, invalidProject bool
	}{
		{name: "disabled intake", intake: "disabled"},
		{name: "legacy ownership refusal", intake: "manual", payload: `{"hub_owned":false,"status":"discovering"}`, legacy: true},
		{name: "legacy status refusal", intake: "manual", payload: `{"hub_owned":true,"status":"finished"}`, legacy: true},
		{name: "malformed legacy batch", intake: "manual", payload: `{"hub_owned":"unknown","status":"discovering"}`, legacy: true, malformed: true},
		{name: "invalid project integration", intake: "manual", invalidProject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			reads := 0
			f.service.config.ImportBackend = scheduledContractImporter(func(context.Context, GitHubImportRequest) (GitHubImportPage, error) {
				reads++
				return GitHubImportPage{BatchPage: &tracker.GitHubDiscoveryPage{}}, nil
			})
			f.command(t, tracker.GitHubBatchCommand{Action: "discover"}, http.StatusOK)
			d := f.service.database
			exec := func(statement string, args ...any) {
				t.Helper()
				if _, err := d.db.ExecContext(t.Context(), statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(f.batch)
			if err != nil {
				t.Fatal(err)
			}
			payload := string(raw)
			var owned, status any = true, "discovering"
			if test.legacy {
				payload, owned, status = test.payload, nil, nil
			}
			for index := range githubBatchMaintenanceLimit {
				project := fmt.Sprintf("batch-prefix-%03d", index)
				exec(`INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_intake,github_repository_enabled) SELECT ?,organization_id,?,profile,states_json,created_at,?,0 FROM projects WHERE id=?`, project, project, test.intake, f.project.ID)
				exec(`INSERT INTO onboarding_issue_intake(project_id,request_json,hub_owned,batch_status) VALUES(?,?,?,?)`, project, payload, owned, status)
			}
			if test.invalidProject {
				exec(`UPDATE projects SET states_json='{}' WHERE id LIKE 'batch-prefix-%'`)
			}
			wantError := test.malformed || test.invalidProject
			if err := f.service.maintainGitHubBatches(t.Context()); (err != nil) != wantError {
				t.Fatalf("first tick error=%v, want refusal=%t", err, wantError)
			}
			if reads != 0 {
				t.Fatalf("first tick exceeded the project budget: source reads=%d", reads)
			}
			if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			batch, err := readGitHubBatch(t.Context(), d.reader, scope)
			if err != nil || batch == nil || batch.Status != "preview" || reads != 1 {
				t.Fatalf("later project did not progress: batch=%+v reads=%d error=%v", batch, reads, err)
			}
			exec(`UPDATE projects SET github_intake='manual',states_json=(SELECT states_json FROM projects WHERE id=?) WHERE id='batch-prefix-000'`, f.project.ID)
			exec(`UPDATE onboarding_issue_intake SET request_json=?,hub_owned=1,batch_status='discovering' WHERE project_id='batch-prefix-000'`, string(raw))
			if err := f.service.maintainGitHubBatches(t.Context()); (err != nil) != wantError {
				t.Fatalf("wrapped tick error=%v, want refusal=%t", err, wantError)
			}
			scope.project = "batch-prefix-000"
			batch, err = readGitHubBatch(t.Context(), d.reader, scope)
			if err != nil || batch == nil || batch.Status != "preview" || reads != 2 {
				t.Fatalf("re-enabled project did not progress after wrap: batch=%+v reads=%d error=%v", batch, reads, err)
			}
		})
	}
}

func TestGitHubBatchUsesCatalogJobCap(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		plan    string
		cap     int
		refused bool
	}{{plan: "free", cap: 10000}, {plan: "starter", cap: 10000}} {
		t.Run(test.plan, func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			d := f.service.database
			d.hostedOrganization = f.project.OrganizationID
			plans := capacityHostedPlans()
			if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
				t.Fatal(err)
			}
			if test.plan != "free" {
				if err := d.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "paid-plan", Action: "base", ExpectedRevision: 1, Plan: PlanReference{ID: test.plan, Version: 1}, Reason: "fixture paid plan"}); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
				reads++
				offset := 0
				if request.Discovery.Cursor != "" {
					var err error
					offset, err = strconv.Atoi(request.Discovery.Cursor)
					if err != nil {
						t.Fatal(err)
					}
				}
				page := tracker.GitHubDiscoveryPage{Total: 201}
				for number := offset + 1; number <= min(offset+100, 201); number++ {
					preview := previewIssue(number, false)
					snapshot := batchSnapshot(number)
					preview.Snapshot = &snapshot
					page.Issues = append(page.Issues, preview)
				}
				if offset+100 < 201 {
					page.NextCursor = strconv.Itoa(offset + 100)
				}
				return GitHubImportPage{BatchPage: &page}, nil
			})
			f.command(t, tracker.GitHubBatchCommand{Action: "start", Destination: "Backlog"}, http.StatusOK)
			if f.batch.Limit != test.cap {
				t.Fatalf("job cap=%d want=%d", f.batch.Limit, test.cap)
			}
			if err := f.service.maintainGitHubBatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			batch, err := readGitHubBatch(t.Context(), d.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := d.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.refused {
				if count != 0 || batch.Status != "discovery_failed" || reads != 1 {
					t.Fatalf("free cap: count=%d status=%s reads=%d", count, batch.Status, reads)
				}
			} else if count != 201 || batch.Status != "finished" || reads != 3 {
				t.Fatalf("paid cap: count=%d status=%s reads=%d", count, batch.Status, reads)
			}
		})
	}
}

func TestGitHubBatchSourceCloseOptIn(t *testing.T) {
	t.Parallel()
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("source_closed=%t", closed), func(t *testing.T) {
			t.Parallel()
			f := newBatchFixture(t)
			f.preview(t, closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusUnprocessableEntity)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12}}, http.StatusOK)
			snapshot := batchSnapshot(12)
			f.report(t, tracker.GitHubBatchResult{Number: 12, Snapshot: &snapshot}, http.StatusOK)
			f.preview(t, closed)
			f.command(t, tracker.GitHubBatchCommand{Action: "apply", Numbers: []int{12, 13, 14}}, http.StatusOK)
			snapshot = batchSnapshot(13)
			f.report(t, tracker.GitHubBatchResult{Number: 13, Snapshot: &snapshot}, http.StatusOK)
			f.report(t, tracker.GitHubBatchResult{Number: 14, Error: "Source context incomplete"}, http.StatusOK)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox").Scan(&count); err != nil || count != 0 {
				t.Fatalf("source write without opt-in: count=%d error=%v", count, err)
			}
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusUnprocessableEntity)
			f.service.database.linkedSourceBase = "https://cloud.example"
			f.service.config.OutboxBackend = &recordingOutboxBackend{}
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusOK)
			f.command(t, tracker.GitHubBatchCommand{Action: "close_source"}, http.StatusOK)
			expected := 1
			if closed {
				expected = 0
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox").Scan(&count); err != nil || count != expected {
				t.Fatalf("close requests=%d want=%d error=%v", count, expected, err)
			}
			if f.batch.Items[0].SourceCloseRequested || f.batch.Items[2].SourceCloseRequested || f.batch.Items[1].SourceCloseRequested == closed {
				t.Fatalf("incorrect close selection: %+v", f.batch.Items)
			}
			if !closed {
				var raw string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT desired_json FROM github_outbox WHERE json_extract(desired_json, '$.close_source')=1").Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var desired WorkpadDesired
				if err := json.Unmarshal([]byte(raw), &desired); err != nil {
					t.Fatal(err)
				}
				if desired.Body != "Tracked in Detent: https://cloud.example/local-organization/linked/issues/2" || !desired.CloseSource || desired.StateReason != "" || desired.IssueNumber != 13 {
					t.Fatalf("close payload=%+v", desired)
				}
			}
		})
	}
}
