package orchestrator

import (
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestLocalIntakeCohortLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	queued := dispatchTestIssue("queued", "Todo")
	backlog := dispatchTestIssue("backlog", "Backlog")
	admitted := dispatchTestIssue("admitted", "In Progress")
	foreign := dispatchTestIssue("cloud", "Rework")
	blocked := dispatchTestIssue("held", "Blocked")
	tracker := &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{queued, backlog, admitted, foreign, blocked}}, states: []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Human Review", "Merging", "Blocked"}},
		{Name: "Human Review", Transitions: []string{"Rework", "Merging"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Merging", "Blocked"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Rework", "Human Review"}},
		{Name: "Done", Terminal: true}, {Name: "Blocked"}, {Name: "Backlog"},
	}, reviewed: new(false)}
	dbPath := filepath.Join(t.TempDir(), "runtime.db")
	attempts := openCompletionDeferralStoreWithoutCleanup(t, dbPath)
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "local"}, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{SourceState: "Human Review", ReworkState: "Rework"}})
	orch := &Orchestrator{cfg: cfg, projectID: "local", connector: tracker, workAttempts: attempts}
	state := newState(cfg)
	id, ok := orch.startDurableWorkAttempt(t.Context(), &state, admitted, 1, now, "", runpkg.RunModeImplement, dispatchLoopStartRecord{})
	if !ok {
		t.Fatal("initial local admission failed")
	}
	cancelled := false
	state.Running[admitted.ID] = Running{Issue: admitted, WorkAttemptID: id, StartedAt: now, Mode: runpkg.RunModeImplement, cancel: func() { cancelled = true }}
	orch.setLocalIntakeDisabled(true)
	if cancelled || state.Draining {
		t.Fatal("intake off stopped the worker or entered shutdown drain")
	}
	if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "local", IssueID: foreign.ID, WorkerType: "runner", StartedAt: now, WorkerMetadataJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "other", IssueID: queued.ID, WorkerType: "agent", StartedAt: now, WorkerMetadataJSON: `{"run_mode":"implement"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "local", IssueID: blocked.ID, WorkerType: "agent", StartedAt: now, WorkerMetadataJSON: `{"run_mode":"implement"}`}); err != nil {
		t.Fatal(err)
	}
	change := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: "head", Files: 1}
	complete := func(mode string, result runpkg.RunResult) {
		t.Helper()
		orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: admitted.ID, CompletedAt: now.Add(time.Minute), Request: runpkg.RunRequest{Mode: mode}, Result: result})
	}
	complete(runpkg.RunModeImplement, runpkg.RunResult{FinalState: FinalStateCompleted, NativeChange: change})
	if got := tracker.stateIssues[2].State; got != "Human Review" {
		t.Fatalf("implementation completion = %s", got)
	}
	for i := range 60 {
		id, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "local", IssueID: "old-other-" + strconv.Itoa(i), WorkerType: "agent", StartedAt: now.Add(time.Minute), WorkerMetadataJSON: `{"run_mode":"implement"}`})
		if err != nil {
			t.Fatal(err)
		}
		if err := attempts.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(2 * time.Minute), TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	if err := attempts.Close(); err != nil {
		t.Fatal(err)
	}
	attempts = openCompletionDeferralStore(t, dbPath)
	orch = &Orchestrator{cfg: cfg, projectID: "local", connector: tracker, workAttempts: attempts}
	orch.setLocalIntakeDisabled(true)
	state = newState(cfg)
	orch.recoverLocalAdmissions(t.Context(), &state)
	state.BoardIssues = cloneIssues(tracker.stateIssues)
	orch.snapshotLocalIntake(&state)
	if !reflect.DeepEqual(state.LocalIntake.Remaining, []string{admitted.ID}) || !reflect.DeepEqual(state.LocalIntake.Blocked, []string{blocked.ID}) {
		t.Fatalf("restarted cohort = %+v", state.LocalIntake)
	}
	for _, issue := range []connector.Issue{queued, backlog, foreign} {
		if orch.localIntakeAllows(&state, issue) {
			t.Fatalf("unadmitted issue %s eligible after restart", issue.ID)
		}
	}
	for _, turn := range []struct {
		lane, mode, want string
		landing          *runpkg.NativeLanding
	}{
		{lane: "Rework", mode: runpkg.RunModeImplement, want: "Merging"},
		{lane: "Merging", mode: runpkg.RunModeMerge, want: "Rework", landing: &runpkg.NativeLanding{RefusalKind: workspace.LandRefusalConflict, Refusal: "source conflict"}},
		{lane: "Rework", mode: runpkg.RunModeImplement, want: "Merging"},
		{lane: "Merging", mode: runpkg.RunModeMerge, want: "Done", landing: &runpkg.NativeLanding{Landed: true, ChangeID: "change_1", VersionID: "version_2", HeadSHA: "head", MergeSHA: "merged"}},
	} {
		t.Run(turn.lane+" to "+turn.want, func(t *testing.T) {
			tracker.stateIssues[2].State = turn.lane
			issue := cloneIssue(tracker.stateIssues[2])
			if !orch.localIntakeAllows(&state, issue) {
				t.Fatal("admitted continuation suppressed")
			}
			id, ok := orch.startDurableWorkAttempt(t.Context(), &state, issue, 1, now, "", turn.mode, dispatchLoopStartRecord{})
			if !ok {
				t.Fatal("continuation attempt failed")
			}
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: id, StartedAt: now, Mode: turn.mode}
			tracker.reviewed = new(true)
			result := runpkg.RunResult{FinalState: FinalStateCompleted, NativeChange: change, NativeLanding: turn.landing}
			if turn.landing != nil && turn.landing.Landed {
				tracker.stateIssues[2].State = "Done"
			}
			complete(turn.mode, result)
			if got := tracker.stateIssues[2].State; got != turn.want {
				t.Fatalf("completion lane = %s, want %s", got, turn.want)
			}
		})
	}
	state.BoardIssues = cloneIssues(tracker.stateIssues)
	orch.snapshotLocalIntake(&state)
	if len(state.LocalIntake.Remaining) != 0 || len(state.LocalIntake.Blocked) != 1 {
		t.Fatalf("finished cohort = %+v", state.LocalIntake)
	}
	if tracker.stateIssues[0].State != "Todo" || tracker.stateIssues[1].State != "Backlog" {
		t.Fatal("queued neighbor changed lane")
	}
	for _, update := range tracker.updates {
		if update.issueID != admitted.ID {
			t.Fatalf("foreign lane write: %+v", update)
		}
	}
	orch.setLocalIntakeDisabled(false)
	if !orch.localIntakeAllows(&state, queued) {
		t.Fatal("explicit resume did not restore admission")
	}
}

func TestLocalIntakeDispatchesOnlyDurableContinuations(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	for _, lane := range []string{"In Progress", "Rework", "Merging"} {
		t.Run(lane, func(t *testing.T) {
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "local"}, LocalIntakeDisabled: true, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done"}})
			admitted := dispatchTestIssue("admitted", lane)
			queued := dispatchTestIssue("queued", "Todo")
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{admitted, queued}}
			attempts := openCompletionDeferralStore(t, filepath.Join(t.TempDir(), "runtime.db"))
			if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "local", IssueID: admitted.ID, WorkerType: "agent", StartedAt: time.Now().Add(-time.Hour), WorkerMetadataJSON: `{"run_mode":"implement"}`}); err != nil {
				t.Fatal(err)
			}
			backend := newWorkerHostRunner()
			orch, err := New(cfg, Dependencies{Connector: tracker, Runner: backend, WorkAttempts: attempts})
			if err != nil {
				t.Fatal(err)
			}
			state := newState(cfg)
			orch.recoverLocalAdmissions(t.Context(), &state)
			if outcome := orch.dispatchIssueWithAdmission(t.Context(), &state, queued, 1, time.Now(), "", false, nil); outcome.dispatched {
				t.Fatal("unstarted Todo acquired a local worker")
			}
			outcome := orch.dispatchIssueWithAdmission(t.Context(), &state, admitted, 1, time.Now(), "", false, nil)
			if !outcome.dispatched {
				t.Fatalf("continuation suppressed: %+v", outcome)
			}
			run := receiveWorkerHostRunRequest(t, backend.started)
			if run.Issue.ID != admitted.ID {
				t.Fatalf("started %s instead of admitted issue", run.Issue.ID)
			}
			state.Running[admitted.ID].cancel()
			if tracker.stateIssues[1].State != "Todo" {
				t.Fatal("queued Todo moved lane")
			}
		})
	}
}
