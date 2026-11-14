package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeBoardResumedExecution(t *testing.T) {
	t.Parallel()
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
		{"truncated diagnostics", "", false},
		{"native running", "Implementing", true},
		{"diagnostic completion", "Finalizing", false},
		{"stale completion runtime", "", false},
		{"stale completion snapshot", "", false},
		{"provider completed", "", false},
		{"native completed", "", false},
		{"released", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := &leaseTestClock{value: time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)}
			service := newClockedPostgresNativeService(t, Config{Clock: clock.Now})
			if err := service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			type currentWork struct {
				fixture nativeFixture
				runner  runnerFixture
				issue   tracker.NativeIssue
				worker  string
				lease   tracker.NativeLease
				event   tracker.NativeRunEvent
			}
			var work []currentWork
			for _, name := range []string{"example-project", "example-library"} {
				f := newNativeFixture(t, service, "", name)
				approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
				issue := f.create(t, "Resume existing work")
				r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
				r.enroll(t)
				worker := r.redemption.Credential
				lease := claimNativeAttempt(t, f, worker, name+"-machine", name+"-previous", issue.WorkItemID)
				start := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, start), http.StatusOK)
				finish := start
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "previous-finish", 2, "succeeded"
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, finish), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				clock.Advance(time.Second)
				lease = claimNativeAttempt(t, f, worker, name+"-machine", name+"-current", issue.WorkItemID)
				renewed := performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 600})
				requireNativeStatus(t, renewed, http.StatusOK)
				decodeHubResponse(t, renewed, &lease)
				w := currentWork{fixture: f, runner: r, issue: issue, worker: worker, lease: lease}
				switch test.name {
				case "native running", "diagnostic completion", "stale completion runtime", "stale completion snapshot", "provider completed", "native completed", "released":
					w.event = nativeStartedEvent(lease)
					w.event.IdempotencyKey = "current-start"
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, w.event), http.StatusOK)
				}
				work = append(work, w)
			}
			clock.Advance(time.Second)
			completedAt := clock.Now()
			if test.name == "stale completion runtime" || test.name == "stale completion snapshot" {
				clock.Advance(runnerauth.HeartbeatTimeout + time.Second)
			}
			now := clock.Now()
			for _, w := range work {
				f, r, issue, lease := w.fixture, w.runner, w.issue, w.lease
				local := runnerauth.DiagnosticAttempt{Key: "local:42", IssueID: string(issue.WorkItemID), LocalAttemptID: 42, Generation: 3, Stage: "implementing", Membership: []string{"running", "durable_active", "claimed"}, LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt}
				claim := runnerauth.DiagnosticAttempt{Key: "native:attempt_current", IssueID: string(issue.WorkItemID), NativeAttemptID: "attempt_current_" + string(f.project.ID), Fence: uint64(lease.FencingToken), Membership: []string{"native_claim"}}
				if w.event.Data.AttemptID != "" {
					claim.NativeAttemptID = w.event.Data.AttemptID
				}
				switch test.name {
				case "diagnostic completion", "stale completion runtime", "stale completion snapshot":
					claim.ProviderCompletedAt, claim.HostOperation, claim.OperationPending = completedAt, "workspace.finalize_native_work", true
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
				if test.name == "truncated diagnostics" {
					view.Diagnostics.Truncated = true
				}
				if test.name == "stale completion runtime" || test.name == "stale completion snapshot" {
					view.Diagnostics.RuntimeObservedAt = completedAt
				}
				if test.name == "stale completion snapshot" {
					view.Diagnostics.ObservedAt, view.ObservedAt = completedAt, completedAt
				}
				if test.name == "typed heartbeat" {
					view.ObservedAt = now.Add(-2 * runnerauth.HeartbeatTimeout)
				}
				if test.name == "wrong project" {
					view.ProjectID = "prj_other"
				}
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
				response := httptest.NewRecorder()
				statements := 0
				traced := postgres.WithTransactionTiming(request.Context(), nil, func(_ time.Time, query string) {
					statements++
					if strings.HasPrefix(query, "UPDATE runner_identities") && strings.Contains(query, "project_configuration_json") {
						t.Errorf("unchanged legacy snapshot updated configuration: %s", query)
					}
				})
				service.Handler().ServeHTTP(response, request.WithContext(traced))
				if test.name == "wrong project" {
					requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
				} else {
					requireNativeStatus(t, response, http.StatusOK)
					if statements == 0 {
						t.Fatal("heartbeat did not capture actual PostgreSQL statements")
					}
				}
				if test.name == "wrong project" {
					if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						_, _, err := reader.RunnerProjectObservation(t.Context(), r.binding.RunnerID, f.project.ID, string(issue.WorkItemID))
						if err == nil {
							return errors.New("wrong-project heartbeat stored selected-project observations")
						}
						if !errors.Is(err, corestore.ErrNotFound) {
							return err
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.name != "wrong project" {
					if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						observation, _, err := reader.RunnerProjectObservation(t.Context(), r.binding.RunnerID, f.project.ID, string(issue.WorkItemID))
						if err != nil {
							return err
						}
						if observation.Diagnostics == nil || !observation.Diagnostics.ObservedAt.Equal(view.Diagnostics.ObservedAt) || !observation.Diagnostics.RuntimeObservedAt.Equal(view.Diagnostics.RuntimeObservedAt) || !reflect.DeepEqual(observation.Diagnostics.Records, records) {
							t.Fatalf("typed diagnostic projection changed: %+v", observation)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.name == "provider completed" || test.name == "native completed" || test.name == "released" {
					event := w.event
					event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "provider-completed", 2
					event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now}
					path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, w.worker, event), http.StatusOK)
					if test.name != "provider completed" {
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "current-finish", 3, "succeeded"
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, w.worker, event), http.StatusOK)
					}
				}
				mutation := tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 600, Reason: "released"}
				endpoint, status := "/renew", http.StatusOK
				if test.name == "released" {
					endpoint, status = "/release", http.StatusNoContent
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+endpoint, w.worker, mutation), status)
				if err := service.maintainBoardRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				drainTestNativeBoard(t, service)
				var stored viewstore.BoardRecord
				if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					stored, err = reader.BoardItem(t.Context(), f.project.ID, issue.WorkItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				card, err := viewstore.NewContent(service.runDetailStore()).BoardRecord(t.Context(), stored)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(card.Execution, stored.Display.Execution) || card.StatusLine != stored.Display.StatusLine {
					t.Fatalf("canonical and typed execution differ: card=%+v display=%+v", card, stored.Display)
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
				credential, status, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
				if err != nil || status != http.StatusOK {
					t.Fatalf("fixture credential=%d %v", status, err)
				}
				page, err := service.readWorkBoardProject(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}, savedWorkState{View: "board", CompletedWindow: "all"})
				if err != nil {
					t.Fatal(err)
				}
				summary := page.Work
				if summary == nil {
					t.Fatal("board omitted running summary")
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
			admin := nativeAttemptTestDatabase(t, work[0].fixture)
			var live int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_items WHERE organization_id=$1 AND running", work[0].fixture.project.OrganizationID).Scan(&live); err != nil {
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
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM leases WHERE organization_id=$1 AND released_at IS NULL AND expires_at>$2", work[0].fixture.project.OrganizationID, now).Scan(&occupied); err != nil {
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
	t.Run("source snapshot cannot suppress a later projection", testDashboardProjectionAfterSourceSnapshot)
	t.Run("dashboard change log", testDashboardChangeLog)
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Pushed card")
	other := f.create(t, "Unchanged card")
	scope := nativeBoardFixtureScope(t, f)
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
			frame := readTestBoardRecordFrame(t, f.service, scope, since)
			if frame.Gap || len(frame.Deltas) != 1 || frame.Deltas[0].Current == nil || frame.Deltas[0].Current.Issue.WorkItemID != issue.WorkItemID {
				t.Fatalf("frame = %#v", frame)
			}
			if frame.Deltas[0].Current.Issue.WorkItemID == other.WorkItemID {
				t.Fatal("unchanged card was emitted")
			}
			since = frame.Sequence
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/board/counts", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var summary viewstore.BoardSummaryPage
			decodeHubResponse(t, response, &summary)
			if summary.ProjectionPending {
				t.Fatal("settled board summary is pending")
			}
			recount := map[string]tracker.NativeWorkLane{}
			completed := int64(0)
			for _, id := range []tracker.NativeWorkItemID{issue.WorkItemID, other.WorkItemID} {
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var current tracker.NativeIssue
				decodeHubResponse(t, response, &current)
				if current.Archived {
					continue
				}
				lane := recount[current.State]
				lane.State = current.State
				lane.Total++
				recount[current.State] = lane
				if current.Terminal && current.ClosedAt != nil {
					completed++
				}
			}
			actual := map[string]tracker.NativeWorkLane{}
			var shipped int64
			for _, row := range summary.Rows {
				switch row.Kind {
				case "lane":
					if row.Total != 0 || row.Running != 0 {
						actual[row.State] = tracker.NativeWorkLane{State: row.State, Total: int(row.Total), Running: int(row.Running)}
					}
				case "total":
					shipped = row.Shipped24h
				}
			}
			if !reflect.DeepEqual(actual, recount) {
				t.Fatalf("counts=%v recount=%v", actual, recount)
			}
			if shipped != completed {
				t.Fatalf("completed=%d recount=%d", shipped, completed)
			}
		})
	}
	frame := readTestBoardRecordFrame(t, f.service, scope, initial.Sequence)
	if frame.Gap || len(frame.Deltas) != 7 {
		t.Fatalf("missed deltas = %#v", frame)
	}
	unchanged := readTestBoardRecordFrame(t, f.service, scope, frame.Sequence)
	if unchanged.Gap || len(unchanged.Deltas) != 0 || unchanged.Sequence != frame.Sequence {
		t.Fatalf("unchanged=%#v", unchanged)
	}
	descriptor := hubTestPolicy()
	t.Run("policy reset", func(t *testing.T) {
		beforeReset := frame.Sequence
		changed := descriptor
		changed.SourceDigest = policy.Digest([]byte("updated board policy instructions"))
		changed = changed.WithID()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusOK)
		descriptor = changed
		f.create(t, "Card after policy reset")
		drainTestNativeBoard(t, f.service)
		gap := readTestBoardRecordFrame(t, f.service, scope, beforeReset)
		if !gap.Gap {
			t.Fatalf("gap=%#v", gap)
		}
	})
	history := f.create(t, "Historical workflow card")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(history.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "historical-done"}, ExpectedRevision: history.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(other.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "historical-blocker"}, ExpectedRevision: 1, RelatedWorkItemID: history.WorkItemID, Operation: "add"}), http.StatusOK)
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
				var row viewstore.BoardRecord
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.BoardItem(t.Context(), scope.Project, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if id == history.WorkItemID {
					if row.Terminal != test.terminal || row.Position.Lane != test.wantRank {
						t.Fatalf("historical card terminal=%v rank=%d", row.Terminal, row.Position.Lane)
					}
				} else if row.PageIssue == nil || len(row.PageIssue.Blockers) != 1 || row.PageIssue.Blockers[0].Terminal != test.terminal {
					t.Fatalf("dependent card projection=%+v", row.PageIssue)
				}
			}
		})
	}

}

func TestNativeBoardAttemptAndChangeDeltas(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	now := clock.Now()
	f := newChangeFixture(t, newClockedPostgresNativeService(t, Config{Clock: clock.Now}))
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	scope := nativeBoardFixtureScope(t, f.nativeFixture)
	drainTestNativeBoard(t, f.service)
	var sequence viewstore.BoardSequence
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		sequence, err = r.BoardSequence(t.Context(), scope.Project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	since := sequence.Sequence
	var version tracker.ChangeVersion
	var lease tracker.NativeLease
	var start tracker.NativeRunEvent
	effort := ""
	model := "test-model"
	worker := f.worker(t, "board-worker")
	assertDelta := func(t *testing.T, status string, count int, reset ...bool) {
		t.Helper()
		drainTestNativeBoard(t, f.service)
		frame := readTestBoardRecordFrame(t, f.service, scope, since)
		var card *tracker.NativeBoardCard
		if len(reset) > 0 && reset[0] {
			if !frame.Gap || len(frame.Deltas) != 0 {
				t.Fatalf("policy reset frame=%#v", frame)
			}
			page, err := f.service.readBoardRecords(f.service.runDetailContext(t.Context()), scope, url.Values{"include": {"board"}})
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
			record, err := f.service.readNativeAttemptTenant(t.Context(), scope, string(f.issue.WorkItemID), latest.AttemptID)
			if err != nil || record.Identity == nil || record.Identity.Model != "test-model" || record.Identity.ReasoningEffort != "" {
				t.Fatalf("display projection changed execution identity=%#v, error=%v", record.Identity, err)
			}
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/board/counts", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var summary viewstore.BoardSummaryPage
		decodeHubResponse(t, response, &summary)
		if summary.ProjectionPending {
			t.Fatal("settled board summary is pending")
		}
		running := 0
		for _, row := range summary.Rows {
			if row.Kind == "lane" {
				running += int(row.Running)
			}
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
		{"release", "cancelled", 1, func(t *testing.T) {
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
			clock.Advance(91 * time.Second)
			now = clock.Now()
			if err := f.service.maintainNativeRetention(f.service.runDetailContext(t.Context()), now); err != nil {
				t.Fatal(err)
			}
			f.service.maintainNativeBoard(f.service.runDetailContext(t.Context()))
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
			content := eventstore.NewContent(f.service.runDetailStore())
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: scope.Organization}, raw)
			if err != nil {
				t.Fatal(err)
			}
			policyRow := policystore.Revision{
				OrganizationID: scope.Organization, ProjectID: scope.Project, PolicyID: descriptor.ID,
				ApprovedBy: "historical-owner", ApprovedAt: now, GitHubPullRequest: new(descriptor.Gates.GitHubPullRequest),
				PayloadObjectKey: payload.PayloadObjectKey, PayloadSHA256: payload.PayloadSHA256,
				PayloadBytes: payload.PayloadBytes, PayloadContentBytes: payload.PayloadContentBytes,
			}
			originalVersion := f.detail(t).Change.CurrentVersion
			version = f.detail(t).Versions[0]
			version.ID, version.Number = hubapi.NewID("version"), version.Number+1
			version.PolicyID, version.Policy = descriptor.ID, descriptor
			raw, err = json.Marshal(version)
			if err != nil {
				t.Fatal(err)
			}
			versionRow, err := content.PrepareVersion(t.Context(), scope.Organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.ImportProjectPolicyRevision(t.Context(), policyRow, false); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return err
				}
				change, err := w.LockChange(t.Context(), f.change.ID)
				if err != nil {
					return err
				}
				change.CurrentVersionID, change.UpdatedAt = new(version.ID), now
				_, err = w.UpdateChange(t.Context(), change)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
				defer cancel()
				if err := f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					change, err := w.LockChange(ctx, f.change.ID)
					if err != nil {
						return err
					}
					change.CurrentVersionID = new(originalVersion)
					_, err = w.UpdateChange(ctx, change)
					return err
				}); err != nil {
					t.Error(err)
				}
			})
			if versions := f.detail(t).Versions; versions[len(versions)-1].ID != version.ID || !reflect.DeepEqual(versions[len(versions)-1].Policy, descriptor) {
				t.Fatal("historical policy changed during hydration")
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock.Advance(time.Second)
			now = clock.Now()
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
	readStoredCards := func(t *testing.T) map[tracker.NativeWorkItemID]viewstore.BoardRecord {
		t.Helper()
		cards := make(map[tracker.NativeWorkItemID]viewstore.BoardRecord)
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, id := range []tracker.NativeWorkItemID{f.issue.WorkItemID, unaffected.WorkItemID, historical.WorkItemID} {
				row, err := r.BoardItem(t.Context(), scope.Project, id)
				if err != nil {
					return err
				}
				cards[id] = row
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return cards
	}
	readCandidates := func(t *testing.T) []viewstore.BoardCandidate {
		t.Helper()
		remaining := map[tracker.NativeWorkItemID]bool{f.issue.WorkItemID: true, unaffected.WorkItemID: true, historical.WorkItemID: true}
		var candidates []viewstore.BoardCandidate
		var seen []tracker.NativeWorkItemID
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			for range 4 {
				candidate, err := r.NextBoardCandidate(t.Context(), "", seen...)
				if errors.Is(err, corestore.ErrNotFound) {
					if len(remaining) != 0 {
						t.Fatalf("candidate walk omitted known items: %v", remaining)
					}
					return nil
				}
				if err != nil {
					return err
				}
				if !remaining[candidate.WorkItemID] || candidate.ProjectID != scope.Project || candidate.Required || candidate.QueuedAt != nil {
					t.Fatalf("unexpected/queued board candidate: %+v", candidate)
				}
				delete(remaining, candidate.WorkItemID)
				seen = append(seen, candidate.WorkItemID)
				candidates = append(candidates, candidate)
			}
			return errors.New("candidate walk did not end after the three known items")
		}); err != nil {
			t.Fatal(err)
		}
		return candidates
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
			before := readStoredCards(t)
			beforeCandidates := readCandidates(t)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, test.method, f.base+"/policy", testHubAdminToken, test.body), map[string]int{http.MethodPut: http.StatusOK, http.MethodDelete: http.StatusNoContent}[test.method])
			want := []string{string(f.issue.WorkItemID), string(historical.WorkItemID)}
			slices.Sort(want)
			if after := readStoredCards(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("approval rebuilt historical cards: before=%+v after=%+v", before, after)
			}
			if after := readCandidates(t); !reflect.DeepEqual(beforeCandidates, after) {
				t.Fatalf("approval changed candidate tuples: before=%+v after=%+v", beforeCandidates, after)
			}
			frame := readTestBoardRecordFrame(t, f.service, scope, since)
			if !frame.Gap {
				t.Fatalf("policy did not invalidate old replay=%+v", frame)
			}
			since = frame.Sequence
			page, err := f.service.readBoardRecords(f.service.runDetailContext(t.Context()), scope, url.Values{"include": {"board"}})
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
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		source, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		source.Core.ID, source.Core.Name, source.Core.Slug = private.project, "Board blocker", ""
		source.Settings.ProjectID = private.project
		source.Settings.RepositoryEnabled = false
		_, err = w.CreateProjectRecord(t.Context(), source)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	blocker := private.seedIssue(t, 1)
	related := f.seedIssue(t, 2)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		for _, target := range []struct {
			project tracker.ProjectID
			item    tracker.NativeWorkItemID
		}{{f.project, related}, {private.project, blocker}} {
			if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.service.accessOrganization(), ProjectID: f.project, WorkItemID: item, RelatedProjectID: target.project, RelatedWorkItemID: target.item, Type: "related"}, "add", f.service.config.Clock()); err != nil {
				return err
			}
		}
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.service.accessOrganization(), ProjectID: f.project, WorkItemID: item, RelatedProjectID: private.project, RelatedWorkItemID: blocker, Type: "dependency", Provenance: "native"}, "add", f.service.config.Clock())
	}); err != nil {
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
	var stored tracker.NativeIssue
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.BoardItem(t.Context(), f.project, item)
		if err != nil {
			return err
		}
		if row.PageIssue == nil {
			return errors.New("stored board issue is unavailable")
		}
		stored = *row.PageIssue
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRelated(t, stored, true)
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
	if initial.Gap || len(initial.Deltas) != 0 || !initial.CountsIndependent || initial.Work != nil || initial.GrantVersion == "" || initial.GrantVersion != page.GrantVersion {
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
			} else if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, private.project)
			}); err != nil {
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
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/prj_ungranted/events?board=true&since=0", nil), http.StatusNotFound)
}

func TestNativeBoardPolicyRefreshIsolatesProjects(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
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
	for _, project := range projects {
		approveHubTestPolicy(t, service, project.fixture.base+"/policy", project.policy)
	}
	drainTestNativeBoard(t, service)
	for _, project := range projects {
		response := performHubAPIRequest(t, service, http.MethodGet, project.fixture.base+"/work-items?include=board", project.fixture.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
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
			response := performHubAPIRequest(t, service, http.MethodGet, project.fixture.base+"/work-items/"+string(issue.WorkItemID)+"/changes/"+project.changes[index].ID, project.fixture.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var detail tracker.ChangeDetail
			decodeHubResponse(t, response, &detail)
			if detail.Summary.Status != card.Change.Status {
				t.Fatalf("card status differs from durable detail: %+v", detail.Summary)
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
	since := page.Sequence
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
			var published []viewstore.BoardDelta
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(f.service.appOrganizationID()), corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				published, err = r.BoardDeltas(t.Context(), f.project, since, 200)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(published, func(delta viewstore.BoardDelta) bool { return delta.WorkItemID == blocker }) {
				t.Fatal("dependency mutation did not publish a blocker board delta")
			}
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
					since = frame.Sequence
					return
				}
			}
		})
	}
}

func TestHostedBoardStreamHidesHiddenNeighborRefresh(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	viewer := f.user(t, "partial-viewer", "member", "partial@example.test", "write", "")
	full := f.user(t, "full-viewer", "member", "full@example.test", "write", "")
	linked, unlinked := f.seedIssue(t, 1), f.seedIssue(t, 2)
	private := f
	private.project = "prj_hidden_blocker"
	private.base = strings.Replace(f.base, string(f.project), string(private.project), 1)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		source, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		source.Core.ID, source.Core.Name, source.Core.Slug = private.project, "Hidden blocker", ""
		source.Settings.ProjectID = private.project
		source.Settings.RepositoryEnabled = false
		_, err = w.CreateProjectRecord(t.Context(), source)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	private.grant(t, full, true, false)
	hidden := private.seedIssue(t, 1)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.service.accessOrganization(), ProjectID: f.project, WorkItemID: linked, RelatedProjectID: private.project, RelatedWorkItemID: hidden, Type: "dependency", Provenance: "native"}, "add", f.service.config.Clock())
	}); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	since := map[string]int64{}
	for name, user := range map[string]hostedSecurityUser{"partial": viewer, "full": full} {
		var page tracker.NativeIssuePage
		response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board", nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &page)
		since[name] = page.Sequence
	}
	title := "Renamed hidden blocker"
	for _, test := range []struct {
		name, method, path string
		body               func(tracker.Revision) any
		want               func(tracker.NativeDependency) bool
	}{
		{"done", http.MethodPost, "/workflow", func(revision tracker.Revision) any {
			return tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "hidden-done"}, ExpectedRevision: revision, State: "Done", Reason: "user_requested"}
		}, func(link tracker.NativeDependency) bool { return link.State == "Done" && link.Terminal }},
		{"title", http.MethodPatch, "", func(revision tracker.Revision) any {
			return tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "hidden-title"}, ExpectedRevision: revision, Title: &title}
		}, func(link tracker.NativeDependency) bool { return link.Title == title }},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := f.request(t, full, http.MethodGet, private.base+"/work-items/"+string(hidden), nil)
			requireNativeStatus(t, read, http.StatusOK)
			var current tracker.NativeIssue
			decodeHubResponse(t, read, &current)
			requireNativeStatus(t, f.request(t, full, test.method, private.base+"/work-items/"+string(hidden)+test.path, test.body(current.Revision)), http.StatusOK)
			drainTestNativeBoard(t, f.service)
			partial := readHostedBoardFrame(t, openHostedTestStream(t, f, viewer, "?board=true&since="+strconv.FormatInt(since["partial"], 10)))
			if partial.Gap || partial.Sequence <= since["partial"] {
				t.Fatalf("partial viewer frame did not advance: %#v", partial)
			}
			for _, delta := range partial.Deltas {
				for _, card := range []*tracker.NativeBoardCard{delta.Previous, delta.Current} {
					if card != nil && (card.Issue.WorkItemID == linked || card.Issue.WorkItemID == unlinked) {
						t.Fatalf("partial viewer received a card delta for %s when only the hidden blocker changed", card.Issue.WorkItemID)
					}
				}
			}
			since["partial"] = partial.Sequence
			visible := readHostedBoardFrame(t, openHostedTestStream(t, f, full, "?board=true&since="+strconv.FormatInt(since["full"], 10)))
			refreshed := slices.ContainsFunc(visible.Deltas, func(delta tracker.NativeBoardDelta) bool {
				return delta.Current != nil && delta.Current.Issue.WorkItemID == linked && len(delta.Current.Issue.Blockers) == 1 && delta.Current.Issue.Blockers[0].ID == hidden && test.want(delta.Current.Issue.Blockers[0])
			})
			if visible.Gap || !refreshed {
				t.Fatalf("full viewer did not receive the refreshed blocker chip: %#v", visible)
			}
			since["full"] = visible.Sequence
		})
	}
}

func TestHostedProjectStreamOmitsNeighborRefresh(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	viewer := f.user(t, "partial-viewer", "member", "partial@example.test", "write", "")
	full := f.user(t, "full-viewer", "member", "full@example.test", "write", "")
	hiddenDependent, visibleDependent, visibleBlocker := f.seedIssue(t, 1), f.seedIssue(t, 2), f.seedIssue(t, 3)
	private := f
	private.project = "prj_hidden_blocker"
	private.base = strings.Replace(f.base, string(f.project), string(private.project), 1)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		source, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		source.Core.ID, source.Core.Name, source.Core.Slug = private.project, "Hidden blocker", ""
		source.Settings.ProjectID = private.project
		source.Settings.RepositoryEnabled = false
		_, err = w.CreateProjectRecord(t.Context(), source)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	private.grant(t, full, true, false)
	hiddenBlocker := private.seedIssue(t, 1)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		for _, edge := range []struct {
			dependent tracker.NativeWorkItemID
			project   tracker.ProjectID
			blocker   tracker.NativeWorkItemID
		}{{hiddenDependent, private.project, hiddenBlocker}, {visibleDependent, f.project, visibleBlocker}} {
			if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.service.accessOrganization(), ProjectID: f.project, WorkItemID: edge.dependent, RelatedProjectID: edge.project, RelatedWorkItemID: edge.blocker, Type: "dependency", Provenance: "native"}, "add", f.service.config.Clock()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	head := func(t *testing.T) int64 {
		t.Helper()
		var sequence int64
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
			watermark, err := r.BoardSequence(t.Context(), f.project)
			sequence = watermark.Sequence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return sequence
	}
	workItemEvents := func(t *testing.T, query string, since, until int64) []string {
		t.Helper()
		stream := openHostedTestStream(t, f, viewer, query+"&since="+strconv.FormatInt(since, 10))
		var named []string
		for {
			frame, err := readSSEFrame(stream)
			if err != nil {
				t.Fatalf("read project stream: %v", err)
			}
			if frame.ID == "" {
				continue
			}
			if frame.Event == "work_item" {
				named = append(named, projectEventEntity(t, frame.Data))
			}
			sequence, err := strconv.ParseInt(frame.ID, 10, 64)
			if err != nil {
				t.Fatalf("frame id %q: %v", frame.ID, err)
			}
			if sequence >= until {
				return named
			}
		}
	}
	since := head(t)
	for _, test := range []struct {
		name      string
		base      string
		blocker   tracker.NativeWorkItemID
		dependent tracker.NativeWorkItemID
		own       []string
	}{
		{"hidden neighbor", private.base, hiddenBlocker, hiddenDependent, nil},
		{"visible neighbor", f.base, visibleBlocker, visibleDependent, []string{string(visibleBlocker)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := f.request(t, full, http.MethodGet, test.base+"/work-items/"+string(test.blocker), nil)
			requireNativeStatus(t, read, http.StatusOK)
			var current tracker.NativeIssue
			decodeHubResponse(t, read, &current)
			transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "done-" + test.name}, ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"}
			requireNativeStatus(t, f.request(t, full, http.MethodPost, test.base+"/work-items/"+string(test.blocker)+"/workflow", transition), http.StatusOK)
			drainTestNativeBoard(t, f.service)
			until := head(t)
			if until <= since {
				t.Fatalf("neighbor refresh did not advance the project sequence: %d -> %d", since, until)
			}
			got := workItemEvents(t, "?", since, until)
			if slices.Contains(got, string(test.dependent)) {
				t.Errorf("project stream named the dependent after only its neighbor changed: %v", got)
			}
			for _, own := range test.own {
				if !slices.Contains(got, own) {
					t.Errorf("project stream lost the changed item's own work_item event: %v, want %s", got, own)
				}
			}
			if got := workItemEvents(t, "?work_item="+string(test.dependent), since, until); len(got) != 0 {
				t.Errorf("dependent stream work_item events=%v, want none", got)
			}
			since = until
		})
	}
}

func projectEventEntity(t *testing.T, data string) string {
	t.Helper()
	var event struct {
		EntityID string `json:"entity_id"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatalf("decode work_item event %q: %v", data, err)
	}
	return event.EntityID
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
	organization := tracker.OrganizationID(service.appOrganizationID())
	var current []tracker.NativeWorkItemID
	for {
		var candidate viewstore.BoardCandidate
		var pending bool
		err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			candidate, err = r.NextBoardCandidate(t.Context(), "", current...)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			item, err := r.WorkItem(t.Context(), candidate.ProjectID, candidate.WorkItemID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			expected := err == nil
			project, err := r.ProjectRecord(t.Context(), candidate.ProjectID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			expected = expected && err == nil && project.Core.DeletedAt == nil && project.Core.Profile == "native"
			row, err := r.BoardItem(t.Context(), candidate.ProjectID, candidate.WorkItemID)
			if errors.Is(err, corestore.ErrNotFound) {
				pending = expected || candidate.Required
				return nil
			}
			if err != nil {
				return err
			}
			state := slices.IndexFunc(project.States, func(state tracker.NativeState) bool { return state.Name == row.Position.State })
			pending = !expected || candidate.Required || !row.Display.Ready || row.PageIssue == nil || row.ListIssue == nil ||
				row.SourceRevision != item.Revision || row.SourceEventSequence != item.EventSequence || state >= 0 && (project.States[state].Terminal != row.Terminal || state != row.Position.Lane) ||
				row.SourceProjectRevision != project.Core.ContentSettingsRevision || row.RefreshAfter != nil && !row.RefreshAfter.After(service.config.Clock())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if candidate.WorkItemID == "" {
			return
		}
		if !pending {
			current = append(current, candidate.WorkItemID)
			continue
		}
		if err := service.maintainBoardRecords(t.Context()); err != nil {
			t.Fatal(err)
		}
		current = current[:0]
	}
}

func nativeBoardFixtureScope(t *testing.T, f nativeFixture) hubscope.NativeScope {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatalf("board fixture credential status=%d: %v", status, err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func readTestBoardRecordFrame(t *testing.T, service *Service, scope hubscope.NativeScope, since int64) tracker.NativeBoardFrame {
	t.Helper()
	sub := &dashboardSubscription{initial: true, sequence: since, frames: make(chan dashboardFrame, 1), done: make(chan struct{})}
	delivery := &dashboardDelivery{config: hubdeps.DashboardDelivery{PageSize: corestore.MaxPageSize}}
	more, err := service.dashboardBoardRecordPage(t.Context(), delivery, sub, scope)
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Fatal("board fixture exceeded one replay page")
	}
	select {
	case queued := <-sub.frames:
		var frame tracker.NativeBoardFrame
		if err := json.Unmarshal([]byte(queued.data), &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	default:
		t.Fatal("missing board frame")
		return tracker.NativeBoardFrame{}
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
