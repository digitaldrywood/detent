package hubserver

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Action runs end to end (decisions section 18.12): the row that exists before
// the process, the refusals that stop a command reaching a worktree it must
// not touch, the reads a person comes back to, and the runner report that
// records a run nobody asked for.

// execWorkspace opens a workspace and binds it with the exec surface, which is
// the only kind of workspace a run may be queued on.
func (f actionFixture) execWorkspace(t *testing.T) string {
	t.Helper()
	session, lease := f.requested(t)
	requireNativeStatus(t, f.workerPost(t, session.ID, "bind", workspaceBindRequest{
		workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: execCapabilities,
		Isolation: workspacesession.IsolationContainer}), http.StatusOK)
	return session.ID
}

// postRun sends one POST /actions/:action/runs with its own idempotency key.
func (f actionFixture) postRun(t *testing.T, actionID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	filled := map[string]any{"idempotency_key": newNativeID("runkey")}
	maps.Copy(filled, body)
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions/"+actionID+"/runs", f.token, filled)
}

func (f actionFixture) queueRun(t *testing.T, actionID, workspaceID string) string {
	t.Helper()
	response := f.postRun(t, actionID, map[string]any{"workspace_id": workspaceID})
	requireNativeStatus(t, response, http.StatusAccepted)
	var receipt projectActionRunReceipt
	decodeHubResponse(t, response, &receipt)
	return receipt.RunID
}

func (f actionFixture) readRun(t *testing.T, actionID, runID string) workspacesession.Run {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/actions/"+actionID+"/runs/"+runID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var run workspacesession.Run
	decodeHubResponse(t, response, &run)
	return run
}

func (f actionFixture) runListing(t *testing.T, actionID, query string) projectActionRunList {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/actions/"+actionID+"/runs"+query, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var listing projectActionRunList
	decodeHubResponse(t, response, &listing)
	return listing
}

// A queued run is a row before it is a process, and the row is on the project
// stream in the same commit that wrote it.
func TestProjectActionRunQueuesARowAndAnEvent(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Run tests", "command": "go test ./..."})
	workspace := f.execWorkspace(t)

	response := f.postRun(t, action.ID, map[string]any{"workspace_id": workspace})
	requireNativeStatus(t, response, http.StatusAccepted)
	var receipt projectActionRunReceipt
	decodeHubResponse(t, response, &receipt)
	if !strings.HasPrefix(receipt.RunID, "actionrun_") {
		t.Fatalf("run id = %q, want a typed identifier", receipt.RunID)
	}

	run := f.readRun(t, action.ID, receipt.RunID)
	if run.Status != workspacesession.RunQueued || run.Revision != 1 {
		t.Fatalf("run = %#v, want queued at revision 1", run)
	}
	// The command is snapshotted, so an action edited afterwards does not
	// rewrite the history of what already ran.
	if run.Command != "go test ./..." || run.WorkspaceID != workspace || run.ActionID != action.ID {
		t.Fatalf("run = %#v", run)
	}
	if run.ExitCode != nil || run.StartedAt != nil || run.FinishedAt != nil || run.OutputArtifact != "" {
		t.Fatalf("run = %#v, want nothing recorded yet", run)
	}
	if run.CreatedBy != f.ownerID {
		t.Fatalf("created_by = %q, want the actor %q", run.CreatedBy, f.ownerID)
	}
	if types := f.eventTypes(t, receipt.RunID); len(types) != 1 || types[0] != "action_run.queued" {
		t.Fatalf("events = %v, want one action_run.queued", types)
	}

	t.Run("editing the action afterwards does not rewrite the queued run", func(t *testing.T) {
		requireNativeStatus(t, f.patchAction(t, action.ID, map[string]any{
			"expected_revision": 1, "command": "go test -race ./..."}), http.StatusOK)
		if again := f.readRun(t, action.ID, receipt.RunID); again.Command != "go test ./..." {
			t.Fatalf("command = %q, want the command as it was when the run was queued", again.Command)
		}
	})
}

// A run is refused before anything is written when the command could not
// safely reach the worktree it names.
func TestProjectActionRunRefusals(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Run tests", "command": "go test ./..."})

	t.Run("an action this project does not hold is not found", func(t *testing.T) {
		workspace := f.execWorkspace(t)
		requireNativeCode(t, f.postRun(t, newNativeID("action"), map[string]any{"workspace_id": workspace}),
			http.StatusNotFound, "not_found")
	})

	t.Run("a workspace of another project is not found", func(t *testing.T) {
		// The id is real; it belongs to another hub's project. The scope is in
		// the read's predicate rather than a filter applied afterwards, so the
		// answer is the same one a workspace that never existed gets.
		other := newActionFixture(t)
		requireNativeCode(t, f.postRun(t, action.ID, map[string]any{"workspace_id": other.execWorkspace(t)}),
			http.StatusNotFound, "not_found")
	})

	t.Run("a workspace no runner has bound is stale rather than queued", func(t *testing.T) {
		// Queueing against a workspace with no runner would leave a row no
		// frame could ever start.
		session, _ := f.requested(t)
		requireNativeCode(t, f.postRun(t, action.ID, map[string]any{"workspace_id": session.ID}),
			http.StatusConflict, "stale_execution")
	})

	t.Run("a read-only workspace refuses a run", func(t *testing.T) {
		// A read-only workspace is one whose attempt is still running, and a
		// command writes into the worktree the model is editing.
		workspace := f.execWorkspace(t)
		if _, err := f.service.database.db.ExecContext(t.Context(),
			"UPDATE workspace_sessions SET read_only = 1 WHERE id = ?", workspace); err != nil {
			t.Fatal(err)
		}
		requireNativeCode(t, f.postRun(t, action.ID, map[string]any{"workspace_id": workspace}),
			http.StatusConflict, "read_only")
	})

	t.Run("a workspace whose runner does not serve exec refuses a run", func(t *testing.T) {
		session, lease := f.requested(t)
		requireNativeStatus(t, f.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities,
			Isolation: workspacesession.IsolationContainer}), http.StatusOK)
		requireNativeCode(t, f.postRun(t, action.ID, map[string]any{"workspace_id": session.ID}),
			http.StatusConflict, "capability_missing")
	})

	t.Run("a body naming no workspace is not found", func(t *testing.T) {
		requireNativeCode(t, f.postRun(t, action.ID, map[string]any{}), http.StatusNotFound, "not_found")
	})
}

// The output endpoint is what Run.OutputArtifact names, and it answers the
// bytes as text.
func TestProjectActionRunOutputEndpoint(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Run tests", "command": "go test ./..."})
	workspace := f.execWorkspace(t)
	run := f.queueRun(t, action.ID, workspace)
	path := f.base + "/actions/" + action.ID + "/runs/" + run + "/output"

	t.Run("a run with no output answers an empty body rather than not found", func(t *testing.T) {
		// A command that printed nothing and exited zero has no output, and
		// 404 would read as "no such run".
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if body := response.Body.String(); body != "" {
			t.Fatalf("body = %q, want empty", body)
		}
	})

	t.Run("the stored bytes come back as text and the receipt names this path", func(t *testing.T) {
		if _, err := f.service.database.db.ExecContext(t.Context(),
			`UPDATE project_action_runs SET output = ?, output_bytes = ?, output_artifact = ? WHERE id = ?`,
			"ok\nPASS\n", len("ok\nPASS\n"), path, run); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("content type = %q, want text/plain; charset=utf-8", got)
		}
		if body := response.Body.String(); body != "ok\nPASS\n" {
			t.Fatalf("body = %q", body)
		}
		if artifact := f.readRun(t, action.ID, run).OutputArtifact; artifact != path {
			t.Fatalf("output_artifact = %q, want %q", artifact, path)
		}
	})

	t.Run("a run of another action is not found on this action's path", func(t *testing.T) {
		other := f.authored(t, map[string]any{"name": "Build", "command": "make build"})
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet,
			f.base+"/actions/"+other.ID+"/runs/"+run, f.token, nil), http.StatusNotFound)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet,
			f.base+"/actions/"+other.ID+"/runs/"+run+"/output", f.token, nil), http.StatusNotFound)
	})
}

// The run listing is how a run is found without having seen its 202: a
// run-on-worktree-creation run has no client that asked for it.
func TestProjectActionRunListing(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Run tests", "command": "go test ./..."})
	other := f.authored(t, map[string]any{"name": "Build", "command": "make build"})
	workspace := f.execWorkspace(t)

	runs := make([]string, 0, 3)
	for range 3 {
		runs = append(runs, f.queueRun(t, action.ID, workspace))
	}
	otherRun := f.queueRun(t, other.ID, workspace)

	t.Run("the listing is newest first and scoped to its action", func(t *testing.T) {
		listing := f.runListing(t, action.ID, "")
		if len(listing.Items) != 3 {
			t.Fatalf("listing = %#v, want this action's three runs", listing.Items)
		}
		for index, item := range listing.Items {
			if item.ID != runs[len(runs)-1-index] {
				t.Fatalf("listing[%d] = %q, want %q", index, item.ID, runs[len(runs)-1-index])
			}
			if item.ActionID != action.ID {
				t.Fatalf("listing[%d] belongs to %q", index, item.ActionID)
			}
		}
		if single := f.runListing(t, other.ID, ""); len(single.Items) != 1 || single.Items[0].ID != otherRun {
			t.Fatalf("the other action's listing = %#v, want only %s", single.Items, otherRun)
		}
	})

	t.Run("limit bounds the page", func(t *testing.T) {
		listing := f.runListing(t, action.ID, "?limit=1")
		if len(listing.Items) != 1 || listing.Items[0].ID != runs[2] {
			t.Fatalf("listing = %#v, want the newest run only", listing.Items)
		}
	})

	t.Run("an invalid limit is refused", func(t *testing.T) {
		requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodGet,
			f.base+"/actions/"+action.ID+"/runs?limit=0", f.token, nil),
			http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("an action this project does not hold has no listing", func(t *testing.T) {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet,
			f.base+"/actions/"+newNativeID("action")+"/runs", f.token, nil), http.StatusNotFound)
	})
}

// The worker report is how a run the runner started itself is recorded: a
// run-on-worktree-creation action has no person watching and no exec stream.
func TestWorkspaceWorkerActionRunReport(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Install", "command": "npm install",
		"run_on_worktree_creation": true})

	t.Run("a report with no run id creates the run and leaves created_by empty", func(t *testing.T) {
		session, lease := f.requested(t)
		requireNativeStatus(t, f.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: execCapabilities,
			Isolation: workspacesession.IsolationContainer}), http.StatusOK)
		started := f.at(t)
		response := f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
			WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID,
			Status: workspacesession.RunRunning, StartedAt: &started})
		requireNativeStatus(t, response, http.StatusOK)
		var run workspacesession.Run
		decodeHubResponse(t, response, &run)
		if run.Status != workspacesession.RunRunning || run.Command != "npm install" || run.WorkspaceID != session.ID {
			t.Fatalf("run = %#v", run)
		}
		// Nobody asked for this run: the action carries
		// run_on_worktree_creation and the runner started it.
		if run.CreatedBy != "" {
			t.Fatalf("created_by = %q, want empty for a run the runner started", run.CreatedBy)
		}
		// But it is claimed, by the runner that started it. A run that is not
		// queued and names no executor would read as one the hub's own dispatch
		// could still hand out (section 18.12).
		row, err := readProjectActionRunByID(t.Context(), f.service.database.db, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(row.ClaimedBy, "runner:") {
			t.Fatalf("claimed_by = %q, want the runner that started it", row.ClaimedBy)
		}
		if types := f.eventTypes(t, run.ID); len(types) != 1 || types[0] != "action_run.running" {
			t.Fatalf("events = %v", types)
		}

		t.Run("a second report moves the same run and records its output", func(t *testing.T) {
			exit := 0
			finished := f.at(t)
			updated := f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
				WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID, RunID: run.ID,
				Status: workspacesession.RunSucceeded, ExitCode: &exit, FinishedAt: &finished,
				Output: "added 42 packages\n"})
			requireNativeStatus(t, updated, http.StatusOK)
			var moved workspacesession.Run
			decodeHubResponse(t, updated, &moved)
			if moved.ID != run.ID || moved.Status != workspacesession.RunSucceeded || moved.Revision != 2 {
				t.Fatalf("moved = %#v", moved)
			}
			if moved.ExitCode == nil || *moved.ExitCode != 0 || moved.OutputBytes != int64(len("added 42 packages\n")) {
				t.Fatalf("moved = %#v", moved)
			}
			if moved.OutputArtifact == "" {
				t.Fatalf("moved = %#v, want the output receipt once there is output", moved)
			}
			body := performHubAPIRequest(t, f.service, http.MethodGet, moved.OutputArtifact, f.token, nil)
			requireNativeStatus(t, body, http.StatusOK)
			if body.Body.String() != "added 42 packages\n" {
				t.Fatalf("output = %q", body.Body.String())
			}
		})

		t.Run("a move off a terminal status is refused", func(t *testing.T) {
			// A run that already finished must not be rewritten by a report
			// that was in flight when it did.
			requireNativeCode(t, f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
				WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID, RunID: run.ID,
				Status: workspacesession.RunRunning}), http.StatusConflict, "stale_execution")
		})

		t.Run("a stale tuple is refused before the run is read", func(t *testing.T) {
			stale := workspaceIdentity(lease)
			stale.FencingToken++
			requireNativeCode(t, f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
				WorkspaceIdentity: stale, ActionID: action.ID,
				Status: workspacesession.RunRunning}), http.StatusConflict, "stale_execution")
		})

		t.Run("an escape-dense output that fills the cap is not refused by the transport", func(t *testing.T) {
			// ESC is the byte that makes this real: every ANSI colour sequence
			// carries it, coloured build output is exactly what a project
			// action produces, and JSON escaping turns one byte into six. At a
			// smaller body limit this report is answered 413 and the run that
			// actually finished is never recorded -- it sits running, which is
			// the one outcome section 18.12 exists to prevent.
			report := f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
				WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID,
				Status: workspacesession.RunSucceeded,
				Output: strings.Repeat("\x1b", workspacesession.MaxExecOutputBytes)})
			requireNativeStatus(t, report, http.StatusOK)
			var dense workspacesession.Run
			decodeHubResponse(t, report, &dense)
			if dense.OutputBytes != workspacesession.MaxExecOutputBytes || dense.Truncated {
				t.Fatalf("dense = %#v, want the whole cap stored untruncated", dense)
			}
		})

		t.Run("output past the cap is stored truncated", func(t *testing.T) {
			report := f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
				WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID,
				Status: workspacesession.RunFailed,
				Output: strings.Repeat("x", workspacesession.MaxExecOutputBytes+256)})
			requireNativeStatus(t, report, http.StatusOK)
			var capped workspacesession.Run
			decodeHubResponse(t, report, &capped)
			if capped.OutputBytes != workspacesession.MaxExecOutputBytes || !capped.Truncated {
				t.Fatalf("capped = %#v, want exactly %d bytes and truncated",
					capped, workspacesession.MaxExecOutputBytes)
			}
		})
	})

	t.Run("a report naming an action of another project is not found", func(t *testing.T) {
		session, lease := f.requested(t)
		requireNativeStatus(t, f.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: execCapabilities,
			Isolation: workspacesession.IsolationContainer}), http.StatusOK)
		requireNativeCode(t, f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
			WorkspaceIdentity: workspaceIdentity(lease), ActionID: newNativeID("action"),
			Status: workspacesession.RunRunning}), http.StatusNotFound, "not_found")
	})

	t.Run("a run of another workspace is not found on this workspace's path", func(t *testing.T) {
		first, firstLease := f.requested(t)
		requireNativeStatus(t, f.workerPost(t, first.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(firstLease), Capabilities: execCapabilities,
			Isolation: workspacesession.IsolationContainer}), http.StatusOK)
		run := f.queueRun(t, action.ID, first.ID)
		second, secondLease := f.requested(t)
		requireNativeStatus(t, f.workerPost(t, second.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(secondLease), Capabilities: execCapabilities,
			Isolation: workspacesession.IsolationContainer}), http.StatusOK)
		requireNativeCode(t, f.workerPost(t, second.ID, "action-runs", workspaceActionRunReport{
			WorkspaceIdentity: workspaceIdentity(secondLease), ActionID: action.ID, RunID: run,
			Status: workspacesession.RunRunning}), http.StatusNotFound, "not_found")
		if unchanged := f.readRun(t, action.ID, run); unchanged.Status != workspacesession.RunQueued {
			t.Fatalf("run = %#v, want it untouched", unchanged)
		}
	})

	t.Run("an unknown status or reason is refused", func(t *testing.T) {
		session, lease := f.requested(t)
		requireNativeStatus(t, f.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: execCapabilities,
			Isolation: workspacesession.IsolationContainer}), http.StatusOK)
		requireNativeCode(t, f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
			WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID, Status: "started"}),
			http.StatusUnprocessableEntity, "invalid_request")
		requireNativeCode(t, f.workerPost(t, session.ID, "action-runs", workspaceActionRunReport{
			WorkspaceIdentity: workspaceIdentity(lease), ActionID: action.ID,
			Status: workspacesession.RunFailed, Reason: "bored"}),
			http.StatusUnprocessableEntity, "invalid_request")
	})
}

// A run the runner started itself has no stream, so nothing in the relay ends
// it: the sweep is its whole recovery path, and it must fail exactly the runs
// whose workspace is gone.
func TestProjectActionRunSweepEndsRunsOfEndedWorkspaces(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	action := f.authored(t, map[string]any{"name": "Install", "command": "npm install",
		"run_on_worktree_creation": true})

	// Two workspaces, one run reported running on each. One workspace then
	// ends; the other stays ready.
	ended, endedLease := f.requested(t)
	requireNativeStatus(t, f.workerPost(t, ended.ID, "bind", workspaceBindRequest{
		workspaceWorkerIdentity: workspaceIdentity(endedLease), Capabilities: execCapabilities,
		Isolation: workspacesession.IsolationContainer}), http.StatusOK)
	alive, aliveLease := f.requested(t)
	requireNativeStatus(t, f.workerPost(t, alive.ID, "bind", workspaceBindRequest{
		workspaceWorkerIdentity: workspaceIdentity(aliveLease), Capabilities: execCapabilities,
		Isolation: workspacesession.IsolationContainer}), http.StatusOK)

	orphan := f.reportRun(t, ended.ID, endedLease, action.ID)
	live := f.reportRun(t, alive.ID, aliveLease, action.ID)

	// The runner's next report never arrives -- it crashed, or the hub
	// restarted, or the report was refused and not retried -- and the
	// workspace ends. Nothing but the sweep can end this run.
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete,
		f.base+"/workspaces/"+ended.ID, f.token, nil), http.StatusNoContent)
	if before := f.readRun(t, action.ID, orphan); before.Status != workspacesession.RunRunning {
		t.Fatalf("run = %#v, want it still running before the sweep", before)
	}

	f.service.workspaces.sweepActionRuns(t.Context())

	swept := f.readRun(t, action.ID, orphan)
	if swept.Status != workspacesession.RunFailed || swept.Reason != workspacesession.RunReasonWorkspaceEnd {
		t.Fatalf("run = %#v, want failed with workspace_closed", swept)
	}
	if swept.FinishedAt == nil || swept.ExitCode != nil {
		t.Fatalf("run = %#v, want a finish time and no exit code", swept)
	}
	requireLastRunEvent(t, f.eventTypes(t, orphan), "action_run.failed")

	// The negative half, and the reason the predicate is the workspace's state
	// rather than a run's age: a long test run on a healthy workspace is
	// indistinguishable from a stuck row by age alone.
	if untouched := f.readRun(t, action.ID, live); untouched.Status != workspacesession.RunRunning {
		t.Fatalf("run = %#v, want a run on a ready workspace left alone", untouched)
	}

	t.Run("a second sweep leaves the failed run alone", func(t *testing.T) {
		before := f.readRun(t, action.ID, orphan)
		f.service.workspaces.sweepActionRuns(t.Context())
		after := f.readRun(t, action.ID, orphan)
		if after.Revision != before.Revision || after.UpdatedAt != before.UpdatedAt {
			t.Fatalf("run = %#v, want the terminal row untouched at revision %d", after, before.Revision)
		}
	})

	t.Run("a run still queued when its workspace ended is failed too", func(t *testing.T) {
		// A queued run of an ended workspace can never start: the row would
		// otherwise sit queued for ever, which reads as "waiting".
		workspace := f.execWorkspace(t)
		run := f.queueRun(t, action.ID, workspace)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete,
			f.base+"/workspaces/"+workspace, f.token, nil), http.StatusNoContent)
		f.service.workspaces.sweepActionRuns(t.Context())
		failed := f.readRun(t, action.ID, run)
		if failed.Status != workspacesession.RunFailed || failed.Reason != workspacesession.RunReasonWorkspaceEnd {
			t.Fatalf("run = %#v, want failed with workspace_closed", failed)
		}
		// A finished_at with no started_at would read as a run of negative
		// length, so the sweep stamps both.
		if failed.StartedAt == nil || failed.FinishedAt == nil {
			t.Fatalf("run = %#v, want both timestamps stamped", failed)
		}
	})
}

// reportRun records one running run through the worker endpoint, the way a
// runner reports a run it started itself, and returns its id.
func (f actionFixture) reportRun(t *testing.T, workspaceID string, lease tracker.NativeLease, actionID string) string {
	t.Helper()
	started := f.at(t)
	response := f.workerPost(t, workspaceID, "action-runs", workspaceActionRunReport{
		WorkspaceIdentity: workspaceIdentity(lease), ActionID: actionID,
		Status: workspacesession.RunRunning, StartedAt: &started})
	requireNativeStatus(t, response, http.StatusOK)
	var run workspacesession.Run
	decodeHubResponse(t, response, &run)
	return run.ID
}

// at reads the hub's own clock, so a timestamp a test sends is one the hub
// would accept rather than whatever the wall clock says.
func (f actionFixture) at(t *testing.T) time.Time {
	t.Helper()
	now, err := f.service.database.currentTime()
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// The bind hands the runner the run-on-worktree-creation set and nothing else,
// in authoring order.
func TestWorkspaceBindCarriesRunOnCreationActions(t *testing.T) {
	t.Parallel()
	f := newActionFixture(t)
	install := f.authored(t, map[string]any{"name": "Install", "command": "npm install",
		"run_on_worktree_creation": true})
	build := f.authored(t, map[string]any{"name": "Build", "command": "npm run build",
		"run_on_worktree_creation": true})
	f.authored(t, map[string]any{"name": "Run tests", "command": "npm test"})

	session, lease := f.requested(t)
	response := f.workerPost(t, session.ID, "bind", workspaceBindRequest{
		workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: execCapabilities,
		Isolation: workspacesession.IsolationContainer})
	requireNativeStatus(t, response, http.StatusOK)
	var bind workspaceBindResponse
	decodeHubResponse(t, response, &bind)
	if len(bind.Actions) != 2 {
		t.Fatalf("actions = %#v, want only the run-on-creation set", bind.Actions)
	}
	// Authoring order is the contract: an author who wants install before
	// build writes install first.
	if bind.Actions[0].ID != install.ID || bind.Actions[1].ID != build.ID {
		t.Fatalf("actions = %#v, want %s then %s", bind.Actions, install.ID, build.ID)
	}
	if bind.Actions[0].Command != "npm install" || bind.Actions[1].Command != "npm run build" {
		t.Fatalf("actions = %#v, want the commands carried", bind.Actions)
	}

	t.Run("a project with no run-on-creation action carries none", func(t *testing.T) {
		local := newActionFixture(t)
		local.authored(t, map[string]any{"name": "Run tests", "command": "npm test"})
		session, lease := local.requested(t)
		answer := local.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: execCapabilities,
			Isolation: workspacesession.IsolationContainer})
		requireNativeStatus(t, answer, http.StatusOK)
		var empty workspaceBindResponse
		decodeHubResponse(t, answer, &empty)
		if len(empty.Actions) != 0 {
			t.Fatalf("actions = %#v, want none", empty.Actions)
		}
	})
}

// forwardRunStatus is the whole lifecycle rule, so it is a table rather than a
// sequence of endpoint calls: a run that already failed because its stream
// closed must never be rewritten as succeeded.
func TestForwardRunStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		from, to string
		want     bool
	}{
		{workspacesession.RunQueued, workspacesession.RunRunning, true},
		{workspacesession.RunQueued, workspacesession.RunSucceeded, true},
		{workspacesession.RunQueued, workspacesession.RunFailed, true},
		{workspacesession.RunQueued, workspacesession.RunQueued, true},
		{workspacesession.RunRunning, workspacesession.RunRunning, true},
		{workspacesession.RunRunning, workspacesession.RunSucceeded, true},
		{workspacesession.RunRunning, workspacesession.RunFailed, true},
		{workspacesession.RunRunning, workspacesession.RunQueued, false},
		{workspacesession.RunSucceeded, workspacesession.RunFailed, false},
		{workspacesession.RunSucceeded, workspacesession.RunSucceeded, false},
		{workspacesession.RunFailed, workspacesession.RunSucceeded, false},
		{workspacesession.RunRunning, "started", false},
	} {
		t.Run(test.from+" to "+test.to+" is "+strconv.FormatBool(test.want), func(t *testing.T) {
			if got := forwardRunStatus(test.from, test.to); got != test.want {
				t.Fatalf("forwardRunStatus(%q, %q) = %v, want %v", test.from, test.to, got, test.want)
			}
		})
	}
}
