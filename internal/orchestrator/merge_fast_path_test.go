package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestMergingFastPathBehindCheckedHeadMergesWithoutRebaseOrAgentDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 20, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		MergeFastPathEnabled:  true,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-clean-fast-path", []string{"enhancement"}, &connector.PullRequest{
		Number:         860,
		URL:            "https://github.test/digitaldrywood/detent/pull/860",
		BranchName:     "detent/detent-digitaldrywood_detent_860-030a2359de53",
		State:          "OPEN",
		MergeableState: "behind",
		CIStatus:       "success",
		HeadSHA:        "head-fast-path",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/detent#860"
	issue.PRRepository = "digitaldrywood/detent"
	issue.BranchName = "detent/detent-digitaldrywood_detent_860-030a2359de53"

	workspaceBackend := &mergeFastPathWorkspace{
		info: workspace.Info{
			Path:   t.TempDir(),
			Key:    "digitaldrywood_detent_860",
			Branch: issue.BranchName,
		},
		result: workspace.MergePrepareResult{
			Status:   workspace.MergePrepareStatusClean,
			DiffStat: workspace.DiffStat{},
		},
	}
	agentBackend := &mergeFastPathAgentBackend{}
	runner, err := runpkg.NewRunner(runpkg.Dependencies{
		Workflow:     workflowconfig.Workflow{},
		Workspace:    workspaceBackend,
		AgentBackend: agentBackend,
		Now: func() time.Time {
			return now
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
	}
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{issue}, now)

	completion := receiveMergeFastPathCompletion(t, orch.runResults)
	if completion.Err != nil {
		t.Fatalf("completion.Err = %v", completion.Err)
	}
	if completion.Request.Mode != runpkg.RunModeMerge {
		t.Fatalf("completion.Request.Mode = %q, want %q", completion.Request.Mode, runpkg.RunModeMerge)
	}
	orch.handleRunResult(context.Background(), &state, completion)

	if got := workspaceBackend.prepareCalls.Load(); got != 0 {
		t.Fatalf("PrepareMerge() calls = %d, want 0", got)
	}
	if got := workspaceBackend.afterRunCalls.Load(); got != 1 {
		t.Fatalf("AfterRun() calls = %d, want 1", got)
	}
	if got := agentBackend.calls.Load(); got != 0 {
		t.Fatalf("AgentBackend.RunTurn() calls = %d, want 0", got)
	}
	if got := tracker.merges; len(got) != 1 {
		t.Fatalf("merges = %#v, want one programmatic merge", got)
	}
	if got := tracker.merges[0]; got.repository != "digitaldrywood/detent" || got.number != 860 || got.headSHA != "head-fast-path" {
		t.Fatalf("merge request = %#v, want repository digitaldrywood/detent PR 860 head-fast-path", got)
	}
	if got := tracker.hydrations; !reflect.DeepEqual(got, []autoPromoteTickHydration{{
		issueID:    issue.ID,
		repository: "digitaldrywood/detent",
		number:     860,
	}}) {
		t.Fatalf("hydrations = %#v, want fresh PR hydration", got)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Done"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after clean merge fast-path", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after clean merge fast-path", issue.ID)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after clean merge fast-path", issue.ID)
	}
	if completed.FinalState != "Done" {
		t.Fatalf("Completed[%q].FinalState = %q, want Done", issue.ID, completed.FinalState)
	}
	if completed.Issue.PullRequest == nil || completed.Issue.PullRequest.State != "MERGED" {
		t.Fatalf("Completed[%q].Issue.PullRequest = %#v, want merged PR", issue.ID, completed.Issue.PullRequest)
	}
}

func TestMergingFastPathMissingRequiredChecksRoutesToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 14, 13, 25, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    1,
		FailureRetryBaseDelay:  time.Minute,
		MaxRetryBackoff:        time.Hour,
		ContinuationRetryDelay: 5 * time.Second,
		MergeFastPathEnabled:   true,
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Merging"},
		TerminalStates:         []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-missing-required-checks", []string{"bug"}, &connector.PullRequest{
		Number:         862,
		URL:            "https://github.test/digitaldrywood/detent/pull/862",
		BranchName:     "detent/merge-fast-path-missing-checks",
		State:          "OPEN",
		MergeableState: "blocked",
		CIStatus:       "pending",
		HeadSHA:        "head-missing-required-checks",
		RequiredCheckFailures: []connector.PullRequestCheck{
			{Name: "Test", Status: "missing", Conclusion: "missing"},
			{Name: "Checks", Status: "missing", Conclusion: "missing"},
		},
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/detent#862"
	issue.PRRepository = "digitaldrywood/detent"
	issue.BranchName = "detent/merge-fast-path-missing-checks"

	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		Attempt:    maxMergeWorkerRunnerFailures,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "worker-a",
		Mode:       runpkg.RunModeMerge,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeMerge},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     runpkg.RunOutputMergeFastPathClean,
		},
	})

	if len(tracker.merges) != 0 {
		t.Fatalf("merges = %#v, want none with missing required checks", tracker.merges)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "Test, Checks") {
		t.Fatalf("comments = %#v, want missing required check names", tracker.comments)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after Rework handoff", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after Rework handoff", issue.ID)
	}
}

func TestMergingFastPathMissingRequiredChecksWaitsForPropagation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 14, 13, 25, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-propagating-required-checks", []string{"bug"}, &connector.PullRequest{
		Number:         864,
		URL:            "https://github.test/digitaldrywood/detent/pull/864",
		BranchName:     "detent/merge-fast-path-propagating-checks",
		State:          "OPEN",
		MergeableState: "blocked",
		CIStatus:       "pending",
		HeadSHA:        "head-propagating-required-checks",
		RequiredCheckFailures: []connector.PullRequestCheck{
			{Name: "Test", Status: "missing", Conclusion: "missing"},
		},
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/detent#864"
	issue.PRRepository = "digitaldrywood/detent"

	tracker, state := completeMergeFastPathTestRun(t, issue, now, 1)

	if len(tracker.merges) != 0 {
		t.Fatalf("merges = %#v, want none while required checks propagate", tracker.merges)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no state transition while required checks propagate", tracker.updates)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing while required checks propagate", issue.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", issue.ID, retry.Attempt)
	}
	if !strings.Contains(retry.Error, "required checks") {
		t.Fatalf("Retry[%q].Error = %q, want required-check propagation wait", issue.ID, retry.Error)
	}
}

func TestMergingFastPathHydrationUnavailableDefers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 14, 13, 25, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-fast-path-hydration-unavailable", []string{"bug"}, &connector.PullRequest{
		Number:                     865,
		URL:                        "https://github.test/digitaldrywood/detent/pull/865",
		BranchName:                 "detent/merge-fast-path-hydration-unavailable",
		State:                      "OPEN",
		MergeableState:             "behind",
		CIStatus:                   "success",
		HeadSHA:                    "head-hydration-unavailable",
		HydrationUnavailableReason: connector.PullRequestHydrationReasonRESTBudgetReserved,
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/detent#865"
	issue.PRRepository = "digitaldrywood/detent"

	tracker, state := completeMergeFastPathTestRun(t, issue, now, 2)

	if len(tracker.merges) != 0 {
		t.Fatalf("merges = %#v, want none without fresh pull request hydration", tracker.merges)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no state transition without fresh pull request hydration", tracker.updates)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing without fresh pull request hydration", issue.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want unchanged attempt 2", issue.ID, retry.Attempt)
	}
	if !strings.Contains(retry.Error, "pull request hydration") {
		t.Fatalf("Retry[%q].Error = %q, want pull request hydration wait", issue.ID, retry.Error)
	}
}

func completeMergeFastPathTestRun(
	t *testing.T,
	issue connector.Issue,
	now time.Time,
	attempt int,
) (*autoPromoteTickMergeConnector, State) {
	t.Helper()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    1,
		FailureRetryBaseDelay:  time.Minute,
		MaxRetryBackoff:        time.Hour,
		ContinuationRetryDelay: 5 * time.Second,
		MergeFastPathEnabled:   true,
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Merging"},
		TerminalStates:         []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		Attempt:    attempt,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "worker-a",
		Mode:       runpkg.RunModeMerge,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}
	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeMerge},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     runpkg.RunOutputMergeFastPathClean,
		},
	})
	return tracker, state
}

func TestMergingFastPathCleanPrecheckWaitsForCurrentHeadCI(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 25, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    1,
		FailureRetryBaseDelay:  time.Minute,
		MaxRetryBackoff:        time.Hour,
		ContinuationRetryDelay: 5 * time.Second,
		MergeFastPathEnabled:   true,
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Merging"},
		TerminalStates:         []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-clean-fast-path-ci", []string{"enhancement"}, &connector.PullRequest{
		Number:         861,
		URL:            "https://github.test/digitaldrywood/detent/pull/861",
		BranchName:     "detent/merge-fast-path-ci",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "pending",
		HeadSHA:        "head-fast-path-ci",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/detent#861"
	issue.PRRepository = "digitaldrywood/detent"
	issue.BranchName = "detent/merge-fast-path-ci"

	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		Attempt:    1,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "worker-a",
		Mode:       runpkg.RunModeMerge,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeMerge},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     runpkg.RunOutputMergeFastPathClean,
		},
	})

	if len(tracker.merges) != 0 {
		t.Fatalf("merges = %#v, want none while CI is pending", tracker.merges)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no state transition while CI is pending", tracker.updates)
	}
	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after fast-path CI wait", issue.ID)
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present while CI is pending", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing while CI is pending", issue.ID)
	}
	if retry.Attempt != 1 || retry.WorkerHost != "worker-a" {
		t.Fatalf("Retry[%q] = %#v, want same-attempt retry on worker-a", issue.ID, retry)
	}
	if !strings.Contains(retry.Error, "current-head CI") {
		t.Fatalf("Retry[%q].Error = %q, want current-head CI wait", issue.ID, retry.Error)
	}
	if !retry.DueAt.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("Retry[%q].DueAt = %s, want continuation retry delay", issue.ID, retry.DueAt)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing while waiting for CI", issue.ID)
	}
}

func TestMergeWorkerProgrammaticMergeDisposition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		state     string
		ci        string
		running   []string
		wantReady bool
		wantWait  bool
	}{
		{name: "clean green", state: "clean", ci: "success", wantReady: true},
		{name: "behind green", state: "behind", ci: "success", wantReady: true},
		{name: "behind pending", state: "behind", ci: "pending", wantWait: true},
		{name: "blocked running", state: "blocked", ci: "pending", running: []string{"Test"}, wantWait: true},
		{name: "blocked without running checks", state: "blocked", ci: "pending"},
		{name: "dirty", state: "dirty", ci: "success"},
		{name: "failed", state: "clean", ci: "failure"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := connector.Issue{
				ID:           "issue-disposition",
				PRRepository: "digitaldrywood/detent",
				PullRequest: &connector.PullRequest{
					Number:         863,
					State:          "open",
					MergeableState: tt.state,
					CIStatus:       tt.ci,
					HeadSHA:        "head-disposition",
					RunningChecks:  tt.running,
				},
			}
			if got := mergeWorkerProgrammaticMergeReady(issue); got != tt.wantReady {
				t.Fatalf("mergeWorkerProgrammaticMergeReady() = %t, want %t", got, tt.wantReady)
			}
			if got := mergeWorkerProgrammaticMergeWaiting(issue); got != tt.wantWait {
				t.Fatalf("mergeWorkerProgrammaticMergeWaiting() = %t, want %t", got, tt.wantWait)
			}
		})
	}
}

func receiveMergeFastPathCompletion(t *testing.T, completions <-chan runpkg.Completion) runpkg.Completion {
	t.Helper()

	select {
	case completion := <-completions:
		return completion
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for merge fast-path completion")
	}
	return runpkg.Completion{}
}

type mergeFastPathWorkspace struct {
	info          workspace.Info
	result        workspace.MergePrepareResult
	prepareCalls  atomic.Int64
	afterRunCalls atomic.Int64
}

func (w *mergeFastPathWorkspace) Create(context.Context, workspace.Issue) (workspace.Info, error) {
	return w.info, nil
}

func (w *mergeFastPathWorkspace) Cleanup(context.Context, string) error {
	return nil
}

func (w *mergeFastPathWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (w *mergeFastPathWorkspace) AfterRun(context.Context, workspace.Info, workspace.Issue) {
	w.afterRunCalls.Add(1)
}

func (w *mergeFastPathWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return w.result.DiffStat, nil
}

func (w *mergeFastPathWorkspace) PrepareMerge(context.Context, workspace.Info, workspace.Issue, workspace.MergePrepareOptions) (workspace.MergePrepareResult, error) {
	w.prepareCalls.Add(1)
	return w.result, nil
}

type mergeFastPathAgentBackend struct {
	calls atomic.Int64
}

func (b *mergeFastPathAgentBackend) RunTurn(context.Context, runpkg.AgentTurnRequest, runpkg.AgentUpdateHandler) (runpkg.AgentTurnResult, error) {
	b.calls.Add(1)
	return runpkg.AgentTurnResult{}, errors.New("agent backend should not run during clean merge fast-path")
}
