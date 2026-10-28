package hubserver

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeBoardResumedExecution(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	type currentWork struct {
		fixture     nativeFixture
		runner      runnerFixture
		issue       tracker.NativeIssue
		worker      string
		lease       tracker.NativeLease
		event       tracker.NativeRunEvent
		completedAt time.Time
	}
	var work []currentWork
	for _, name := range []string{"example-project", "example-library"} {
		f := newNativeFixture(t, service, "", name)
		approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
		issue := f.create(t, "Resume existing work")
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.enroll(t)
		worker := f.worker(t, name+"-worker")
		lease := claimNativeAttempt(t, f, worker, name+"-machine", name+"-previous", issue.WorkItemID)
		start := nativeStartedEvent(lease)
		path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, start), http.StatusOK)
		finish := start
		finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "previous-finish", 2, "succeeded"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, finish), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
		now = now.Add(time.Second)
		lease = claimNativeAttempt(t, f, worker, name+"-machine", name+"-current", issue.WorkItemID)
		if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)", lease.ID, r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
		work = append(work, currentWork{fixture: f, runner: r, issue: issue, worker: worker, lease: lease})
	}
	for _, test := range []struct {
		name, stage string
		live        bool
	}{
		{"implementing", "Implementing", true},
		{"typed heartbeat", "Implementing", true},
		{"unknown stage", "Running", true},
		{"stale runtime", "", false},
		{"wrong fence", "", false},
		{"wrong project", "", false},
		{"finalizing", "Finalizing", false},
		{"claim only", "", false},
		{"native running", "Implementing", true},
		{"diagnostic completion", "Finalizing", false},
		{"stale completion runtime", "", false},
		{"stale completion snapshot", "", false},
		{"provider completed", "", false},
		{"native completed", "", false},
		{"released", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = now.Add(time.Second)
			if test.name == "stale completion runtime" {
				now = now.Add(runnerauth.HeartbeatTimeout + time.Second)
			}
			for index, w := range work {
				f, r, issue, lease := w.fixture, w.runner, w.issue, w.lease
				if test.name == "native running" {
					w.event = nativeStartedEvent(lease)
					w.event.IdempotencyKey = "current-start"
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", w.worker, w.event), http.StatusOK)
					work[index].event = w.event
				}
				local := runnerauth.DiagnosticAttempt{Key: "local:42", IssueID: string(issue.WorkItemID), LocalAttemptID: 42, Generation: 3, Stage: "implementing", Membership: []string{"running", "durable_active", "claimed"}, LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt}
				claim := runnerauth.DiagnosticAttempt{Key: "native:attempt_current", IssueID: string(issue.WorkItemID), NativeAttemptID: "attempt_current_" + string(f.project.ID), Fence: uint64(lease.FencingToken), Membership: []string{"native_claim"}}
				if w.event.Data.AttemptID != "" {
					claim.NativeAttemptID = w.event.Data.AttemptID
				}
				if test.name == "diagnostic completion" {
					w.completedAt = now
					work[index].completedAt = now
				}
				if test.name == "diagnostic completion" || test.name == "stale completion runtime" || test.name == "stale completion snapshot" {
					claim.ProviderCompletedAt, claim.HostOperation, claim.OperationPending = w.completedAt, "workspace.finalize_native_work", true
				}
				if test.name == "unknown stage" {
					local.Stage = "unreported"
				}
				if test.name == "wrong fence" {
					local.Fence = uint64(lease.FencingToken) + 1
				}
				if test.name == "finalizing" {
					claim.ProviderCompletedAt, claim.HostOperation, claim.OperationPending = now, "workspace.finalize_native_work", true
				}
				records := []runnerauth.DiagnosticAttempt{local, claim}
				if test.name == "claim only" {
					records = records[1:]
				}
				view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, RuntimeObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Records: records}}
				if test.name == "stale runtime" {
					view.Diagnostics.RuntimeObservedAt = now.Add(-10 * time.Minute)
				}
				if test.name == "stale completion runtime" || test.name == "stale completion snapshot" {
					view.Diagnostics.RuntimeObservedAt = w.completedAt
				}
				if test.name == "stale completion snapshot" {
					view.Diagnostics.ObservedAt, view.ObservedAt = w.completedAt, w.completedAt
				}
				if test.name == "wrong project" {
					view.ProjectID = "prj_other"
				}
				if _, err := service.database.db.ExecContext(t.Context(), "DELETE FROM runner_project_observations WHERE runner_id = ?", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if test.name == "typed heartbeat" {
					view.ObservedAt = now.Add(-2 * runnerauth.HeartbeatTimeout)
					view.Diagnostics.ObservedAt, view.Diagnostics.RuntimeObservedAt = view.ObservedAt, view.ObservedAt
				}
				raw, err := json.Marshal(map[string]runnerauth.ProjectConfiguration{string(f.project.ID): view})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET project_configuration_json=? WHERE id=?", string(raw), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if test.name == "typed heartbeat" {
					view.Diagnostics.ObservedAt, view.Diagnostics.RuntimeObservedAt = now, now
					request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
					response := httptest.NewRecorder()
					timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
						if strings.HasPrefix(query, "UPDATE runner_identities") && strings.Contains(query, "project_configuration_json") {
							t.Errorf("unchanged legacy snapshot updated configuration: %s", query)
						}
					}}
					service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
					requireNativeStatus(t, response, http.StatusOK)
				}
				if test.name == "provider completed" || test.name == "native completed" {
					path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
					event := w.event
					if test.name == "provider completed" {
						event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "provider-completed", 2
						event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now}
					} else {
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "current-finish", 3, "succeeded"
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, w.worker, event), http.StatusOK)
					work[index].event = event
				}
				mutation := tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 600, Reason: "released"}
				endpoint, status := "/renew", http.StatusOK
				if test.name == "released" {
					endpoint, status = "/release", http.StatusNoContent
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+endpoint, w.worker, mutation), status)
				if test.name != "typed heartbeat" && test.name != "wrong project" {
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
					legacy, legacyErr := readNativeBoardExecution(t.Context(), service.database.reader, scope, string(issue.WorkItemID), nil, now)
					tx, err := service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO runner_project_observations(organization_id,runner_id,project_id,observed_at,diagnostics_observed_at,runtime_observed_at,snapshot_digest) VALUES(?,?,?,?,?,?,?)`, scope.organization, r.binding.RunnerID, scope.project, formatHubTime(view.ObservedAt), formatHubTime(view.Diagnostics.ObservedAt), formatHubTime(view.Diagnostics.RuntimeObservedAt), "board-fixture-"+string(f.project.ID)); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					if err := storeRunnerBoardProjection(t.Context(), tx, scope, r.binding.RunnerID, view); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					projected, projectedErr := readNativeBoardExecution(t.Context(), service.database.reader, scope, string(issue.WorkItemID), nil, now)
					if !reflect.DeepEqual(legacy, projected) || !errors.Is(projectedErr, legacyErr) {
						t.Fatalf("legacy execution=%+v err=%v; projected=%+v err=%v", legacy, legacyErr, projected, projectedErr)
					}
				}
				drainTestNativeBoard(t, service)
				var rawCard string
				if err := service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE work_item_id=?", issue.WorkItemID).Scan(&rawCard); err != nil {
					t.Fatal(err)
				}
				var card tracker.NativeBoardCard
				if err := json.Unmarshal([]byte(rawCard), &card); err != nil {
					t.Fatal(err)
				}
				live := card.Execution != nil && card.Execution.Running
				if live != test.live || card.StatusLine != test.stage {
					t.Fatalf("project %s: execution=%+v status=%s", f.project.ID, card.Execution, card.StatusLine)
				}
				if test.live && card.Execution.ID != claim.NativeAttemptID {
					t.Fatalf("unlinked execution=%+v", card.Execution)
				}
				wantAttemptStatus := "succeeded"
				if w.event.Data.AttemptID != "" && test.name != "native completed" && test.name != "released" {
					wantAttemptStatus = "running"
				}
				if card.Attempt == nil || card.Attempt.Status != wantAttemptStatus {
					t.Fatalf("historical attempt=%+v", card.Attempt)
				}
				summary, err := readNativeBoardSummary(t.Context(), service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}, url.Values{}, now)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, lane := range summary.Lanes {
					count += lane.Running
				}
				want := 0
				if test.live {
					want = 1
				}
				if count != want {
					t.Fatalf("project %s: live=%d want %d", f.project.ID, count, want)
				}
			}
			var live int
			if err := service.database.reader.QueryRowContext(t.Context(), "SELECT sum(running) FROM native_board_items").Scan(&live); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.live {
				want = 2
			}
			if live != want {
				t.Fatalf("combined live=%d want %d", live, want)
			}
			var occupied int
			if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM leases WHERE released_at IS NULL AND julianday(expires_at)>julianday(?)", formatHubTime(now)).Scan(&occupied); err != nil {
				t.Fatal(err)
			}
			wantOccupied := 2
			if test.name == "released" {
				wantOccupied = 0
			}
			if occupied != wantOccupied {
				t.Fatalf("occupied=%d want %d", occupied, wantOccupied)
			}
		})
	}
}

func TestNativeBoardProjectionAndReplay(t *testing.T) {
	t.Parallel()
	t.Run("dashboard change log", testDashboardChangeLog)
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Pushed card")
	other := f.create(t, "Unchanged card")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	read := func() tracker.NativeIssuePage {
		t.Helper()
		drainTestNativeBoard(t, f.service)
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=board&state=Todo&limit=2000", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		return page
	}
	initial := read()
	if len(initial.Cards) != 2 || len(initial.Items) != 2 || initial.Sequence == 0 {
		t.Fatalf("initial board = %#v", initial)
	}
	since := initial.Sequence
	expectedTitle := "Edited pushed card"
	for _, test := range []struct {
		name string
		path string
		body any
	}{
		{"edit", "", tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "board-edit"}, ExpectedRevision: issue.Revision, Title: &expectedTitle}},
		{"comment", "/comments", map[string]any{"idempotency_key": "board-comment", "body": "Activity changes only this card"}},
		{"transition", "/workflow", map[string]any{"idempotency_key": "board-transition", "expected_revision": "2", "state": "In Progress", "reason": "user_requested"}},
		{"archive", "/archive", map[string]any{"idempotency_key": "board-archive", "expected_revision": "3"}},
		{"restore", "/restore", map[string]any{"idempotency_key": "board-restore", "expected_revision": "4"}},
		{"complete", "/workflow", map[string]any{"idempotency_key": "board-complete", "expected_revision": "5", "state": "Done", "reason": "user_requested"}},
		{"reopen", "/workflow", map[string]any{"idempotency_key": "board-reopen", "expected_revision": "6", "state": "Todo", "reason": "user_requested"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := http.MethodPost
			if test.name == "edit" {
				method = http.MethodPatch
			}
			response := performHubAPIRequest(t, f.service, method, f.base+"/work-items/"+string(issue.WorkItemID)+test.path, f.token, test.body)
			if response.Code < 200 || response.Code >= 300 {
				t.Fatalf("%s: %d %s", test.name, response.Code, response.Body.String())
			}
			drainTestNativeBoard(t, f.service)
			tx, err := f.service.database.reader.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			frame, err := readNativeBoardFrame(t.Context(), tx, scope, since)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Gap || len(frame.Deltas) != 1 || frame.Deltas[0].Current == nil || frame.Deltas[0].Current.Issue.WorkItemID != issue.WorkItemID {
				t.Fatalf("frame = %#v", frame)
			}
			if frame.Deltas[0].Current.Issue.WorkItemID == other.WorkItemID {
				t.Fatal("unchanged card was emitted")
			}
			since = frame.Sequence
			summary, err := readNativeBoardSummary(t.Context(), tx, scope, url.Values{}, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			recount := map[string]tracker.NativeWorkLane{}
			rows, err := tx.QueryContext(t.Context(), "SELECT ws.detent_state,count(*),0 FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.project_id=? AND i.archived=0 GROUP BY ws.detent_state", scope.project)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var lane tracker.NativeWorkLane
				if err := rows.Scan(&lane.State, &lane.Total, &lane.Running); err != nil {
					t.Fatal(err)
				}
				recount[lane.State] = lane
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			actual := map[string]tracker.NativeWorkLane{}
			for _, lane := range summary.Lanes {
				actual[lane.State] = lane
			}
			if !reflect.DeepEqual(actual, recount) {
				t.Fatalf("counts=%v recount=%v", actual, recount)
			}
			full, err := readNativeWorkSummary(t.Context(), tx, scope, "SELECT i.native_id FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.archived=0", []any{scope.organization, scope.project}, 50, f.service.config.now(), "48h", "")
			if err != nil {
				t.Fatal(err)
			}
			if summary.Completed != full.Completed {
				t.Fatalf("completed=%d recount=%d", summary.Completed, full.Completed)
			}
		})
	}
	frame, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, initial.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Gap || len(frame.Deltas) != 7 {
		t.Fatalf("missed deltas = %#v", frame)
	}
	unchanged, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, frame.Sequence)
	if err != nil || unchanged.Gap || len(unchanged.Deltas) != 0 {
		t.Fatalf("unchanged=%#v err=%v", unchanged, err)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE retained(n) AS (
SELECT 1 UNION ALL SELECT n+1 FROM retained WHERE n<?)
INSERT INTO native_board_deltas(organization_id,project_id,previous_json,current_json)
SELECT ?,?,NULL,NULL FROM retained`, nativeBoardReplayLimit, scope.organization, scope.project); err != nil {
		t.Fatal(err)
	}
	var beforeReset int64
	if err := tx.QueryRowContext(t.Context(), "SELECT MAX(sequence) FROM native_board_deltas WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&beforeReset); err != nil {
		t.Fatal(err)
	}
	if err := resetNativeBoardPolicy(t.Context(), tx, string(scope.organization)+"/"+string(scope.project)); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	f.create(t, "Card after policy reset")
	gap, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, beforeReset)
	if err != nil || !gap.Gap {
		t.Fatalf("gap=%#v err=%v", gap, err)
	}
	history := f.create(t, "Historical workflow card")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(history.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "historical-done"}, ExpectedRevision: history.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(other.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "historical-blocker"}, ExpectedRevision: 1, RelatedWorkItemID: history.WorkItemID, Operation: "add"}), http.StatusOK)
	descriptor := hubTestPolicy()
	for _, test := range []struct {
		name     string
		states   []tracker.NativeState
		terminal bool
		wantRank int
	}{
		{"reordered workflow", []tracker.NativeState{nativeFixtureStates()[2], nativeFixtureStates()[0], nativeFixtureStates()[1]}, true, 1},
		{"historical terminal flag changes", []tracker.NativeState{{Name: "Done", Transitions: []string{"Todo"}}, nativeFixtureStates()[0], nativeFixtureStates()[1], {Name: "Closed", Terminal: true}}, false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := descriptor
			changed.Workflow = &policy.Workflow{Source: "detent.yaml", States: test.states}
			changed = changed.WithID()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusOK)
			descriptor = changed
			drainTestNativeBoard(t, f.service)
			for _, id := range []tracker.NativeWorkItemID{history.WorkItemID, other.WorkItemID} {
				var raw string
				var rank int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json,lane_rank FROM native_board_items WHERE work_item_id=?", id).Scan(&raw, &rank); err != nil {
					t.Fatal(err)
				}
				var card tracker.NativeBoardCard
				if err := json.Unmarshal([]byte(raw), &card); err != nil {
					t.Fatal(err)
				}
				if id == history.WorkItemID {
					if card.Issue.Terminal != test.terminal || rank != test.wantRank {
						t.Fatalf("historical card terminal=%v rank=%d", card.Issue.Terminal, rank)
					}
				} else if len(card.Issue.Blockers) != 1 || card.Issue.Blockers[0].Terminal != test.terminal {
					t.Fatalf("dependent card blockers=%+v", card.Issue.Blockers)
				}
			}
		})
	}

}

func TestNativeBoardAttemptAndChangeDeltas(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	drainTestNativeBoard(t, f.service)
	var since int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_board_sequences WHERE project_id=?", scope.project).Scan(&since); err != nil {
		t.Fatal(err)
	}
	var version tracker.ChangeVersion
	var lease tracker.NativeLease
	var start tracker.NativeRunEvent
	effort := ""
	model := "test-model"
	worker := f.worker(t, "board-worker")
	assertDelta := func(t *testing.T, status string, count int, reset ...bool) {
		t.Helper()
		drainTestNativeBoard(t, f.service)
		frame, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, since)
		if err != nil {
			t.Fatal(err)
		}
		var card *tracker.NativeBoardCard
		if len(reset) > 0 && reset[0] {
			if !frame.Gap || len(frame.Deltas) != 0 {
				t.Fatalf("policy reset frame=%#v", frame)
			}
			page, err := f.service.readBoardIssues(t.Context(), scope, url.Values{"include": {"board"}})
			if err != nil || len(page.Cards) != 1 {
				t.Fatalf("reset page=%#v, error=%v", page, err)
			}
			card = &page.Cards[0]
		} else {
			if frame.Gap || len(frame.Deltas) == 0 {
				t.Fatalf("frame=%#v", frame)
			}
			for _, delta := range frame.Deltas {
				if delta.Current == nil || delta.Current.Issue.WorkItemID != f.issue.WorkItemID {
					t.Fatalf("wrong delta=%#v", delta)
				}
			}
			card = frame.Deltas[len(frame.Deltas)-1].Current
		}
		if card.Issue.WorkItemID != f.issue.WorkItemID {
			t.Fatalf("wrong card: %s", card.Issue.WorkItemID)
		}
		if card.Change == nil || card.Change.Status != f.detail(t).Summary.Status {
			t.Fatalf("change=%#v", card.Change)
		}
		if status != "" && (card.Attempt == nil || card.Attempt.Status != status || card.Attempt.Count != count) {
			t.Fatalf("attempt=%#v want status %s count %d", card.Attempt, status, count)
		}
		if status != "" {
			if card.Attempt.Identity == nil || card.Attempt.Identity.ReasoningEffort != effort || card.Attempt.Identity.Model != model {
				t.Fatalf("board identity=%#v want model %s effort %s", card.Attempt.Identity, model, effort)
			}
			var attempts tracker.Page[tracker.NativeAttempt]
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/attempts", worker, nil), &attempts)
			latest := attempts.Items[len(attempts.Items)-1]
			if latest.Identity == nil || *latest.Identity != *card.Attempt.Identity {
				t.Fatalf("attempt identity=%#v board identity=%#v", latest.Identity, card.Attempt.Identity)
			}
			if effort != "" && (latest.Runtime == nil || latest.Runtime.Identity.ReasoningEffort.Value != effort) {
				t.Fatalf("runtime identity differs from projection: %#v", latest.Runtime)
			}
			record, err := readNativeAttemptRecord(t.Context(), f.service.database.reader, scope, string(f.issue.WorkItemID), latest.AttemptID, now)
			if err != nil || record.Identity == nil || record.Identity.Model != "test-model" || record.Identity.ReasoningEffort != "" {
				t.Fatalf("display projection changed execution identity=%#v, error=%v", record.Identity, err)
			}
		}
		summary, err := readNativeBoardSummary(t.Context(), f.service.database.reader, scope, url.Values{}, f.service.config.now())
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, lane := range summary.Lanes {
			running += lane.Running
		}
		expected := 0
		if status == "running" {
			expected = 1
		}
		if running != expected {
			t.Fatalf("running=%d want %d", running, expected)
		}
		since = frame.Sequence
	}
	for _, test := range []struct {
		name, status string
		count        int
		mutate       func(t *testing.T)
	}{
		{"attempt", "running", 1, func(t *testing.T) {
			lease = claimNativeAttempt(t, f.nativeFixture, worker, "board-machine", "board-session", f.issue.WorkItemID)
			start = nativeStartedEvent(lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		}},
		{"runtime effort", "running", 1, func(t *testing.T) {
			effort, model = "high", "resolved-model"
			event := start
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "runtime-effort", 2
			event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now, Identity: agentidentity.RuntimeUpdate(model, "", effort, "", now)}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, event), http.StatusOK)
		}},
		{"release", "interrupted", 1, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
		}},
		{"new attempt", "running", 2, func(t *testing.T) {
			effort, model = "", "test-model"
			lease = claimNativeAttempt(t, f.nativeFixture, worker, "board-machine", "board-session-2", f.issue.WorkItemID)
			start := nativeStartedEvent(lease)
			start.IdempotencyKey = "board-second-start"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		}},
		{"expiry", "interrupted", 2, func(t *testing.T) {
			now = now.Add(91 * time.Second)
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			f.service.maintainNativeBoard(t.Context())
		}},
		{"version", "", 2, func(t *testing.T) { version = f.publish(t, "board-version", "") }},
		{"review", "", 2, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "board-review"}, Decision: "approved"}), http.StatusOK)
		}},
		{"check", "", 2, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
		}},
		{"review policy requirement", "", 2, func(t *testing.T) {
			rules := f.rules
			rules.RequireReview = false
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "changed-review-rule"}, ExpectedID: f.rules.ID, Policy: rules})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &f.rules)
		}},
		{"restored review requirement", "", 2, func(t *testing.T) {
			rules := f.rules
			rules.RequireReview = true
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "restored-review-rule"}, ExpectedID: f.rules.ID, Policy: rules})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &f.rules)
		}},
		{"historical retired operator action", "", 2, func(t *testing.T) {
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerHubNative
			descriptor, err := workflowconfig.ResolvePolicy(workflowconfig.Workflow{
				Config:   cfg,
				Authored: &policy.Authored{Version: workflowconfig.NativePolicyCanonicalizationVersion},
				DefinitionSources: &workflowconfig.ProjectDefinitionSources{
					Workflow: []byte("---\ntracker:\n  kind: hub_native\noperator:\n  actions: [clear_closed_dependencies, return_retired_parks]\n---\nHistorical instructions."),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := workflowconfig.ResolveSharedPolicy(descriptor); err == nil || !strings.Contains(err.Error(), "operator.actions") {
				t.Fatalf("new policy admission error = %v", err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor})
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO policy_revisions(scope,policy_id,metadata_json,approved_by,approved_at) VALUES(?,?,?,?,?)", string(scope.organization)+"/"+string(scope.project), descriptor.ID, string(raw), "historical-owner", formatHubTime(now)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER change_versions_immutable"); err != nil {
				t.Fatal(err)
			}
			version = f.detail(t).Versions[0]
			original, err := compactChangeVersion(version)
			if err != nil {
				t.Fatal(err)
			}
			originalPolicyID := version.PolicyID
			ctx := t.Context()
			t.Cleanup(func() {
				version.PolicyID = originalPolicyID
				if _, err := f.service.database.db.ExecContext(context.WithoutCancel(ctx), "UPDATE change_versions SET policy_revision_id=?,record_json=? WHERE id=?", originalPolicyID, original, version.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(context.WithoutCancel(ctx), "INSERT OR IGNORE INTO native_board_dirty(work_item_id) VALUES(?)", f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			})
			version.PolicyID = descriptor.ID
			record, err := compactChangeVersion(version)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_versions SET policy_revision_id=?,record_json=? WHERE id=?", descriptor.ID, record, version.ID); err != nil {
				t.Fatal(err)
			}
			if got := f.detail(t).Versions[0].Policy; !reflect.DeepEqual(got, descriptor) {
				t.Fatal("historical policy changed during hydration")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT OR IGNORE INTO native_board_dirty(work_item_id) VALUES(?)", f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = now.Add(time.Second)
			test.mutate(t)
			assertDelta(t, test.status, test.count, test.name == "review policy requirement" || test.name == "restored review requirement")
		})
	}
	unaffected := f.create(t, "Historical card without Change")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(unaffected.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "unaffected-done"}, ExpectedRevision: unaffected.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	historical := f.create(t, "Historical Change card")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(historical.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "change-done"}, ExpectedRevision: historical.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	path := f.base + "/work-items/" + string(historical.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "historical-change"}, Title: "Historical Change"})
	requireNativeStatus(t, response, http.StatusOK)
	var historicalChange tracker.ChangeRequest
	decodeHubResponse(t, response, &historicalChange)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+historicalChange.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "historical-version"}, ChangeVersionInput: changeTestInput()}), http.StatusOK)
	drainTestNativeBoard(t, f.service)
	for _, statement := range []string{
		"CREATE TABLE policy_dirty_cards(work_item_id TEXT PRIMARY KEY)",
		"CREATE TRIGGER count_change_policy_cards AFTER INSERT ON native_board_dirty BEGIN INSERT OR IGNORE INTO policy_dirty_cards VALUES(NEW.work_item_id); END",
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	instructions := hubTestPolicy()
	instructions.SourceDigest = policy.Digest([]byte("changed instructions"))
	instructions = instructions.WithID()
	changed := hubTestPolicy()
	changed.Gates.SecurityAudit = true
	changed = changed.WithID()
	for _, test := range []struct {
		name   string
		method string
		body   any
	}{
		{"instruction-only custom policy", http.MethodPut, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: instructions}},
		{"changed policy", http.MethodPut, policy.Change{ExpectedID: instructions.ID, Policy: changed}},
		{"restored policy", http.MethodPut, policy.Change{ExpectedID: changed.ID, Policy: hubTestPolicy()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM policy_dirty_cards"); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, test.method, f.base+"/policy", testHubAdminToken, test.body), map[string]int{http.MethodPut: http.StatusOK, http.MethodDelete: http.StatusNoContent}[test.method])
			ids, err := nativePageIDs(t.Context(), f.service.database.reader, "SELECT work_item_id FROM policy_dirty_cards ORDER BY work_item_id")
			want := []string{string(f.issue.WorkItemID), string(historical.WorkItemID)}
			slices.Sort(want)
			if err != nil || len(ids) != 0 {
				t.Fatalf("approval rebuilt historical cards=%v err=%v", ids, err)
			}
			frame, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, since)
			if err != nil || !frame.Gap {
				t.Fatalf("policy did not invalidate old replay=%+v, error=%v", frame, err)
			}
			since = frame.Sequence
			page, err := f.service.readBoardIssues(t.Context(), scope, url.Values{"include": {"board"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range want {
				var card tracker.NativeBoardCard
				for _, candidate := range page.Cards {
					if string(candidate.Issue.WorkItemID) == id {
						card = candidate
						break
					}
				}
				if card.Change == nil {
					t.Fatalf("missing returned historical/current card %s", id)
				}
				wantStatus := "stale_policy"
				if test.name == "restored policy" || test.name == "instruction-only custom policy" {
					wantStatus = "needs_evidence"
					if id == string(f.issue.WorkItemID) {
						wantStatus = "reviewed"
					}
				}
				if card.Change == nil || card.Change.Status != wantStatus {
					t.Fatalf("card %s Change=%+v, want %s", id, card.Change, wantStatus)
				}
			}
		})
	}

}

func TestHostedBoardStreamReplay(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "board-viewer", "member", "board@example.test", "write", "")
	item := f.seedIssue(t, 1)
	private := f
	private.project = "prj_board_blocker"
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) SELECT ?,organization_id,'Board blocker',profile,states_json,created_at,0 FROM projects WHERE id=?", private.project, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT ?,source_name,detent_state,terminal,dispatchable,created_at,updated_at FROM workflow_states WHERE project_id=?", private.project, f.project); err != nil {
		t.Fatal(err)
	}
	blocker := private.seedIssue(t, 1)
	related := f.seedIssue(t, 2)
	for _, id := range []tracker.NativeWorkItemID{related, blocker} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_relationships(first_issue_id,second_issue_id,relationship_type,created_at) SELECT min(a.id,b.id),max(a.id,b.id),'related',? FROM issues a,issues b WHERE a.native_id=? AND b.native_id=?", formatHubTime(time.Now()), item, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies(blocker_issue_id,dependent_issue_id,provenance,created_at,updated_at) SELECT b.id,d.id,'native',?,? FROM issues b,issues d WHERE b.native_id=? AND d.native_id=?", formatHubTime(time.Now()), formatHubTime(time.Now()), blocker, item); err != nil {
		t.Fatal(err)
	}
	if err := f.service.hubTransact(t.Context(), func(*sql.Tx, time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertRelated := func(t *testing.T, issue tracker.NativeIssue, granted bool) {
		t.Helper()
		ids := []tracker.NativeWorkItemID{}
		for _, link := range issue.Related {
			if link.Title == "" || link.Number == 0 || link.WebURL == "" {
				t.Fatalf("missing related metadata: %#v", link)
			}
			ids = append(ids, link.ID)
		}
		want := []tracker.NativeWorkItemID{related}
		if granted {
			want = append(want, blocker)
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("related IDs=%v, want %v (granted=%v)", ids, want, granted)
		}
	}
	drainTestNativeBoard(t, f.service)
	var stored tracker.NativeBoardCard
	var raw string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE work_item_id=?", item).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	assertRelated(t, stored.Issue, true)
	var page tracker.NativeIssuePage
	response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board", nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	for _, card := range page.Cards {
		if card.Issue.WorkItemID == item {
			assertRelated(t, card.Issue, false)
		}
	}
	stream := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(page.Sequence, 10))
	readFrame := func(scanner *bufio.Reader) tracker.NativeBoardFrame { return readHostedBoardFrame(t, scanner) }
	initial := readFrame(stream)
	if initial.Gap || len(initial.Deltas) != 0 || initial.Work == nil || initial.GrantVersion == "" || initial.GrantVersion != page.GrantVersion {
		t.Fatalf("initial=%#v", initial)
	}
	for index := range 2 {
		response := f.request(t, user, http.MethodPost, f.base+"/work-items/"+string(item)+"/comments", map[string]any{"idempotency_key": fmt.Sprintf("board-stream-%d", index), "body": "Pushed activity"})
		requireNativeStatus(t, response, http.StatusOK)
		drainTestNativeBoard(t, f.service)
	}
	replay := readFrame(openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(initial.Sequence, 10)))
	if replay.Gap || len(replay.Deltas) != 2 || replay.Sequence <= initial.Sequence {
		t.Fatalf("replay=%#v", replay)
	}
	for _, delta := range replay.Deltas {
		if delta.Current == nil || delta.Current.Issue.WorkItemID != item {
			t.Fatalf("delta=%#v", delta)
		}
		assertRelated(t, delta.Current.Issue, false)
		if delta.Previous == nil {
			t.Fatalf("missing previous card: %#v", delta)
		}
		assertRelated(t, delta.Previous.Issue, false)
	}
	for readFrame(stream).Sequence != replay.Sequence {
	}
	grantVersion := initial.GrantVersion
	for _, test := range []struct {
		name  string
		grant bool
	}{{"gain", true}, {"lose", false}, {"regain", true}} {
		t.Run(test.name, func(t *testing.T) {
			if test.grant {
				private.grant(t, user, false, false)
			} else if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", user.identity.Subject, private.project); err != nil {
				t.Fatal(err)
			}
			var changed tracker.NativeBoardFrame
			for {
				changed = readFrame(stream)
				if changed.GrantVersion != grantVersion {
					break
				}
			}
			if changed.GrantVersion == "" || changed.Sequence != replay.Sequence || len(changed.Deltas) != 0 {
				t.Fatalf("grant-only frame=%#v", changed)
			}
			grantVersion = changed.GrantVersion
			response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board", nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &page)
			if page.GrantVersion != grantVersion || len(page.Cards) != 2 {
				t.Fatalf("grant snapshot=%#v", page)
			}
			var card tracker.NativeBoardCard
			for _, candidate := range page.Cards {
				if candidate.Issue.WorkItemID == item {
					card = candidate
					break
				}
			}
			assertRelated(t, card.Issue, test.grant)
			if test.grant {
				if len(card.Issue.Blockers) != 1 || card.Issue.Blockers[0].ID != blocker || card.StatusLine != "Waiting on #1" {
					t.Fatalf("granted blocker=%#v", card)
				}
			} else if len(card.Issue.Blockers) != 0 || card.StatusLine != "" {
				t.Fatalf("revoked blocker=%#v", card)
			}
			resumed := readFrame(openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(replay.Sequence, 10)))
			if resumed.GrantVersion != grantVersion || len(resumed.Deltas) != 0 || resumed.Gap {
				t.Fatalf("grant reconnect=%#v", resumed)
			}
			replayed := readFrame(openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(initial.Sequence, 10)))
			if replayed.Gap || len(replayed.Deltas) != 2 {
				t.Fatalf("grant replay=%#v", replayed)
			}
			for _, delta := range replayed.Deltas {
				if delta.Previous == nil || delta.Current == nil {
					t.Fatalf("missing replay cards: %#v", delta)
				}
				assertRelated(t, delta.Previous.Issue, test.grant)
				assertRelated(t, delta.Current.Issue, test.grant)
			}
		})
	}
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/prj_ungranted/events?board=true&since=0", nil), http.StatusForbidden)
}

func TestNativeBoardPolicyRefreshIsolatesProjects(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	type projectCards struct {
		fixture nativeFixture
		policy  policy.Descriptor
		items   []tracker.NativeIssue
		changes []tracker.ChangeRequest
		want    string
	}
	projects := []projectCards{}
	for index, name := range []string{"changed-audit", "unchanged-gates"} {
		f := newNativeFixture(t, service, "", name)
		descriptor := hubTestPolicy()
		approveHubTestPolicy(t, service, f.base+"/policy", descriptor)
		cards := projectCards{fixture: f, policy: descriptor, want: "reviewed"}
		for _, state := range []string{"Todo", "Done"} {
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: state}, Title: name + " " + state, Body: issueContractTestSections, State: state})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: name + " " + state})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			response = performHubAPIRequest(t, service, http.MethodPost, path+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: changeTestInput()})
			requireNativeStatus(t, response, http.StatusOK)
			cards.items = append(cards.items, issue)
			cards.changes = append(cards.changes, change)
		}
		cards.policy.SourceDigest = policy.Digest([]byte(name))
		if index == 0 {
			cards.policy.Gates.SecurityAudit = true
			cards.want = "stale_policy"
		}
		cards.policy = cards.policy.WithID()
		projects = append(projects, cards)
	}
	tx, err := service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, project := range projects {
		scope := string(project.fixture.project.OrganizationID) + "/" + string(project.fixture.project.ID)
		if _, err := service.database.approvePolicyInTx(t.Context(), tx, scope, "test-owner", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: project.policy}, policyLanding{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, service)
	for _, project := range projects {
		scope := nativeScope{organization: project.fixture.project.OrganizationID, project: project.fixture.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
		page, err := service.readBoardIssues(t.Context(), scope, url.Values{"include": {"board"}})
		if err != nil {
			t.Fatal(err)
		}
		for index, issue := range project.items {
			var card tracker.NativeBoardCard
			for _, candidate := range page.Cards {
				if candidate.Issue.WorkItemID == issue.WorkItemID {
					card = candidate
					break
				}
			}
			if card.Issue.State != issue.State || card.Change == nil || card.Change.Status != project.want {
				t.Fatalf("project %s card lane = %s, Change = %+v, want unchanged lane and %s", project.fixture.project.ID, card.Issue.State, card.Change, project.want)
			}
			detail, err := readChangeDetail(t.Context(), service.database.reader, scope, string(issue.WorkItemID), project.changes[index].ID, service.config.now())
			if err != nil || detail.Summary.Status != card.Change.Status {
				t.Fatalf("card status differs from durable detail: %+v, error %v", detail.Summary, err)
			}
		}
	}
}

func TestNativeBoardDependentChangesReachBlockerStream(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "dependency-viewer", "member", "viewer@example.test", "write", "")
	created := f.request(t, user, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "dependent"}, Title: "Dependent", Body: issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	dependent, blocker := issue.WorkItemID, f.seedIssue(t, 2)
	drainTestNativeBoard(t, f.service)
	response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board", nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.NativeIssuePage
	decodeHubResponse(t, response, &page)
	stream := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(page.Sequence, 10))
	readHostedBoardFrame(t, stream)
	for _, test := range []struct {
		name, path string
		body       any
		state      string
		links      int
	}{
		{name: "add", path: "/dependencies", body: tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "add"}, ExpectedRevision: 1, RelatedWorkItemID: blocker, Operation: "add"}, state: "Todo", links: 1},
		{name: "complete", path: "/workflow", body: map[string]any{"idempotency_key": "complete", "expected_revision": "2", "state": "Done", "reason": "user_requested"}, state: "Done", links: 1},
		{name: "reopen", path: "/workflow", body: map[string]any{"idempotency_key": "reopen", "expected_revision": "3", "state": "Todo", "reason": "user_requested"}, state: "Todo", links: 1},
		{name: "remove", path: "/dependencies", body: tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "remove"}, ExpectedRevision: 4, RelatedWorkItemID: blocker, Operation: "remove"}, links: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/work-items/"+string(dependent)+test.path, test.body), http.StatusOK)
			drainTestNativeBoard(t, f.service)
			for {
				frame := readHostedBoardFrame(t, stream)
				if frame.Gap {
					t.Fatal("dependency change invalidated replay")
				}
				for _, delta := range frame.Deltas {
					if delta.Current == nil || delta.Current.Issue.WorkItemID != blocker {
						continue
					}
					links := delta.Current.Issue.Dependents
					if len(links) != test.links {
						t.Fatalf("blocker retained stale dependent chips: %+v", links)
					}
					if len(links) > 0 && (links[0].ID != dependent || links[0].State != test.state || links[0].Terminal != (test.state == "Done")) {
						t.Fatalf("blocker retained stale dependent state: %+v", links)
					}
					return
				}
			}
		})
	}
}

func normalizeTestDashboardWatermark(t *testing.T, before, after map[string]any) {
	t.Helper()
	if before == nil || after == nil {
		t.Fatal("dashboard snapshot is not an object")
	}
	previous, existed := before["change_sequence"]
	current, exists := after["change_sequence"]
	if existed != exists {
		t.Fatal("dashboard change sequence availability changed")
	}
	if !existed {
		return
	}
	previousSequence, previousOK := previous.(float64)
	currentSequence, currentOK := current.(float64)
	if !previousOK || !currentOK || currentSequence < previousSequence {
		t.Fatalf("dashboard change sequence regressed: %v -> %v", previous, current)
	}
	after["change_sequence"] = previous
}

func drainTestNativeBoard(t *testing.T, service *Service) {
	t.Helper()
	var cursor string
	for {
		more, id, err := service.database.drainNativeBoardDirtyAfter(service.runDetailContext(t.Context()), service.config.now(), cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
		cursor = id
	}
}

func readHostedBoardFrame(t *testing.T, scanner *bufio.Reader) tracker.NativeBoardFrame {
	t.Helper()
	for {
		kind, data := readHostedEvent(t, scanner)
		if kind != "activity" {
			continue
		}
		var frame tracker.NativeBoardFrame
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
}

func TestNativeBoardRefreshSkipsFailingCard(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		failures        int
		nonexpired      int
		attemptsPerTick int
		source          string
	}{
		{"65 failing cards", 65, 0, 100, "dirty"},
		{"dirty across ticks", 7, 0, 3, "dirty"},
		{"expired across ticks", 7, 0, 3, "expired"},
		{"mixed across ticks", 7, 0, 3, "mixed"},
		{"nonexpired ahead of dirty", 0, 128, 3, "dirty"},
		{"nonexpired ahead of expired", 0, 128, 3, "expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			issues := []tracker.NativeWorkItemID{f.create(t, "First").WorkItemID, f.create(t, "Second").WorkItemID, f.create(t, "Third").WorkItemID}
			slices.Sort(issues)
			drainTestNativeBoard(t, f.service)
			broken, healthy := issues[0], issues[1:]
			db := f.service.database.db
			now := f.service.config.now()
			expired := now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")
			if _, err := db.ExecContext(t.Context(), "UPDATE native_board_items SET record_json='[1]' WHERE work_item_id=?", broken); err != nil {
				t.Fatal(err)
			}
			for i := range test.nonexpired {
				expiry := ""
				if i%2 != 0 {
					expiry = now.Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO native_board_items(work_item_id,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,record_json) SELECT ?,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,?,record_json FROM native_board_items WHERE work_item_id=?`, fmt.Sprintf("wi_!%06d", i), expiry, broken); err != nil {
					t.Fatal(err)
				}
			}
			for i := range test.failures {
				id := fmt.Sprintf("wi_00000000000000000000000000000%03d", i)
				if _, err := db.ExecContext(t.Context(), `INSERT INTO native_board_items(work_item_id,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,record_json) SELECT ?,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,record_json FROM native_board_items WHERE work_item_id=?`, id, broken); err != nil {
					t.Fatal(err)
				}
				if test.source == "expired" || (test.source == "mixed" && i%2 == 0) {
					if _, err := db.ExecContext(t.Context(), "UPDATE native_board_items SET expires_at=? WHERE work_item_id=?", expired, id); err != nil {
						t.Fatal(err)
					}
				} else if _, err := db.ExecContext(t.Context(), "INSERT INTO native_board_dirty(work_item_id) VALUES(?)", id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(t.Context(), "UPDATE native_board_items SET record_json='null' WHERE work_item_id=?", broken); err != nil {
				t.Fatal(err)
			}
			healthyExpiry := expired
			if test.nonexpired > 0 && test.source == "dirty" {
				healthyExpiry = ""
			}
			for _, id := range healthy {
				if _, err := db.ExecContext(t.Context(), "UPDATE native_board_items SET record_json='null',expires_at=? WHERE work_item_id=?", healthyExpiry, id); err != nil {
					t.Fatal(err)
				}
				if test.source != "expired" {
					if _, err := db.ExecContext(t.Context(), "INSERT INTO native_board_dirty(work_item_id) VALUES(?)", id); err != nil {
						t.Fatal(err)
					}
				}
			}
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			step := analyticsBackfillBudget / time.Duration(test.attemptsPerTick+1)
			f.service.config.now = func() time.Time {
				result := now
				now = now.Add(step)
				return result
			}
			var statements []string
			ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
				if query != nativeBoardRetentionCandidateQuery {
					statements = append(statements, query)
				}
			}})
			ticks := (test.failures+test.nonexpired+len(issues)+test.attemptsPerTick-1)/test.attemptsPerTick + 2
			for tick := range ticks {
				logs.Reset()
				statements = nil
				f.service.maintainNativeBoard(ctx)
				if tick == 0 && test.nonexpired > 0 {
					if f.service.boardRefreshCursor != "wi_!000002" || len(statements) != test.attemptsPerTick+1 {
						t.Fatalf("nonexpired discovery did not resume after three read-only steps: cursor=%q statements=%v", f.service.boardRefreshCursor, statements)
					}
					for _, statement := range statements {
						if statement != nativeBoardCandidateQuery && statement != "SELECT id,organization_id,principal_id,requested_revision,scan_revision,after_thread,through_thread,partial_count FROM team_thread_unread_summaries WHERE requested_revision>completed_revision ORDER BY turn,id LIMIT 1" {
							t.Fatalf("nonexpired discovery performed refresh work: %s", statement)
						}
					}
				}
				seen := map[string]bool{}
				for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					var record struct {
						Message string `json:"msg"`
						ID      string `json:"work_item_id"`
						Error   string `json:"error"`
					}
					if err := json.Unmarshal(line, &record); err != nil {
						t.Fatal(err)
					}
					if record.Message != "refresh native board card" || record.ID == "" || record.Error == "" || seen[record.ID] {
						t.Fatalf("failure was not logged once with its identity and error: %s", line)
					}
					seen[record.ID] = true
				}
				if (tick == 0 || tick == ticks-1) && len(seen) != min(test.failures, test.attemptsPerTick) {
					t.Fatalf("tick %d attempted %d failures, want %d after resuming or wrapping", tick, len(seen), min(test.failures, test.attemptsPerTick))
				}
			}
			for _, id := range healthy {
				var raw, expiry string
				var dirty bool
				if err := db.QueryRowContext(t.Context(), `SELECT record_json,expires_at,EXISTS(SELECT 1 FROM native_board_dirty WHERE work_item_id=?) FROM native_board_items WHERE work_item_id=?`, id, id).Scan(&raw, &expiry, &dirty); err != nil {
					t.Fatal(err)
				}
				var card *tracker.NativeBoardCard
				if err := json.Unmarshal([]byte(raw), &card); err != nil || card == nil || card.Issue.WorkItemID != id || expiry != "" || dirty {
					t.Errorf("healthy card %s starved after %d ticks: card=%+v expiry=%q dirty=%v err=%v", id, ticks, card, expiry, dirty, err)
				}
			}
		})
	}
}

func TestNativeBoardCandidateQueryBounded(t *testing.T) {
	t.Parallel()
	type candidate struct {
		id      string
		refresh bool
	}
	for _, count := range []int{2, 65, 256} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			issue := f.create(t, "Candidate")
			drainTestNativeBoard(t, f.service)
			now := f.service.config.now()
			expired := now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")
			future := now.Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for i := range count * 2 {
				id := fmt.Sprintf("wi_%06d", i)
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_board_items(work_item_id,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,record_json) SELECT ?,organization_id,project_id,state,archived,terminal,running,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,record_json FROM native_board_items WHERE work_item_id=?`, id, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if i%2 == 0 {
					if _, err := tx.ExecContext(t.Context(), "INSERT INTO native_board_dirty(work_item_id) VALUES(?)", id); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := tx.ExecContext(t.Context(), "DELETE FROM native_board_items WHERE work_item_id=?", issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			last := fmt.Sprintf("wi_%06d", count*2-1)
			for _, expiry := range []struct {
				name, value  string
				due, tailDue bool
			}{
				{"absent", "", false, false},
				{"future", future, false, false},
				{"sparse", "", false, true},
				{"expired", expired, true, true},
			} {
				t.Run(expiry.name, func(t *testing.T) {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET expires_at=?", expiry.value); err != nil {
						t.Fatal(err)
					}
					if expiry.tailDue {
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET expires_at=? WHERE work_item_id=?", expired, last); err != nil {
							t.Fatal(err)
						}
					}
					for _, test := range []struct {
						cursor string
						want   []candidate
						next   candidate
					}{
						{"", []candidate{{"wi_000000", true}, {"wi_000000", expiry.due}}, candidate{"wi_000000", true}},
						{"wi_000000", []candidate{{"wi_000002", true}, {"wi_000001", expiry.due}}, candidate{"wi_000001", expiry.due}},
						{"wi_000001", []candidate{{"wi_000002", true}, {"wi_000002", expiry.due}}, candidate{"wi_000002", true}},
						{fmt.Sprintf("wi_%06d", count*2-2), []candidate{{last, expiry.tailDue}}, candidate{last, expiry.tailDue}},
						{"wi_999999", nil, candidate{}},
					} {
						rows, err := f.service.database.reader.QueryContext(t.Context(), nativeBoardCandidateQuery, test.cursor, now.UTC().Format("2006-01-02T15:04:05.000000000Z"), test.cursor)
						if err != nil {
							t.Fatal(err)
						}
						defer rows.Close()
						var got []candidate
						for rows.Next() {
							var row candidate
							if err := rows.Scan(&row.id, &row.refresh); err != nil {
								t.Fatal(err)
							}
							got = append(got, row)
						}
						if err := rows.Err(); err != nil {
							t.Fatal(err)
						}
						if !slices.Equal(got, test.want) {
							t.Fatalf("cursor %q: candidates=%v want=%v", test.cursor, got, test.want)
						}
						id, refresh, err := f.service.database.nextNativeBoardCandidate(t.Context(), now, test.cursor)
						if err != nil || id != test.next.id || refresh != test.next.refresh {
							t.Fatalf("cursor %q: next candidate=%q refresh=%v want=%+v err=%v", test.cursor, id, refresh, test.next, err)
						}
					}
				})
			}
		})
	}
}

func TestNativeBoardDirtyWriterBound(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		f.create(t, fmt.Sprintf("Bounded board %d", i))
	}
	d := f.service.database
	if _, err := d.db.ExecContext(t.Context(), "UPDATE issues SET title=title||' refreshed' WHERE organization_id=? AND project_id=?", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	var reads int
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		if strings.Contains(query, "SELECT record_json FROM native_board_items") {
			reads++
			if d.db.Stats().InUse != 0 {
				t.Error("board projection reads while holding the writer")
			}
		}
	}})
	var cursor string
	for remaining := 3; remaining > 0; remaining-- {
		var before, after int
		if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_deltas").Scan(&before); err != nil {
			t.Fatal(err)
		}
		timingCtx, timing := withWriterTiming(ctx)
		more, id, err := d.drainNativeBoardDirtyAfter(timingCtx, f.service.config.now(), cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !more || id == "" || timing.txs.Load() != 1 {
			t.Fatalf("drain more=%t id=%q transactions=%d", more, id, timing.txs.Load())
		}
		var dirty int
		if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_dirty").Scan(&dirty); err != nil {
			t.Fatal(err)
		}
		if dirty != remaining-1 {
			t.Fatalf("dirty=%d want=%d: each transaction must refresh one card", dirty, remaining-1)
		}
		if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_deltas").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after-before != 1 {
			t.Fatalf("one board transaction published %d deltas; want one", after-before)
		}
		cursor = id
	}
	if reads != 3 {
		t.Fatalf("projection reads=%d want=3", reads)
	}
}
