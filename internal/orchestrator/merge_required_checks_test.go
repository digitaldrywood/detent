package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestMergeRequiredCheckStreakEscalation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		autoPromoteEnabled bool
		steps              []mergeRequiredCheckEvaluationStep
		wantBlocked        bool
	}{
		{
			name:               "check arrives before threshold",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
			},
		},
		{
			name:               "check never arrives",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
			},
			wantBlocked: true,
		},
		{
			name:               "config change mid-count",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test", "Lint"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test", "Lint"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test", "Checks"}},
			},
		},
		{
			name:               "head SHA change mid-count",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-b", requiredChecks: []string{"Test"}},
			},
			wantBlocked: true,
		},
		{
			name: "auto-promote disabled",
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}},
			},
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orch, tracker, state, closeStore := newMergeRequiredCheckTestOrchestrator(t, tt.autoPromoteEnabled)
			defer closeStore()
			now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
			for index, step := range tt.steps {
				runMergeRequiredCheckEvaluation(t, orch, tracker, &state, step, now.Add(time.Duration(index)*time.Minute))
			}

			blocked, ok := state.Blocked[mergeRequiredCheckTestIssueID]
			if ok != tt.wantBlocked {
				t.Fatalf("Blocked[%q] present = %t, want %t", mergeRequiredCheckTestIssueID, ok, tt.wantBlocked)
			}
			if !tt.wantBlocked {
				if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != autoPromoteReworkState {
					t.Fatalf("updates = %#v, want final Rework transition", tracker.updates)
				}
				return
			}
			if !strings.Contains(blocked.Reason, "Test") {
				t.Fatalf("Blocked[%q].Reason = %q, want missing check name", mergeRequiredCheckTestIssueID, blocked.Reason)
			}
			if blocked.RecoveryReason != string(BlockedRecoveryReasonHumanBlocker) {
				t.Fatalf("Blocked[%q].RecoveryReason = %q, want needs-human signal", mergeRequiredCheckTestIssueID, blocked.RecoveryReason)
			}
			snapshot := state.Snapshot(now.Add(time.Hour))
			if len(snapshot.Blocked) != 1 || !strings.Contains(snapshot.Blocked[0].Error, "Test") || snapshot.Blocked[0].RecoveryReason != string(BlockedRecoveryReasonHumanBlocker) {
				t.Fatalf("snapshot.Blocked = %#v, want board-visible named needs-human reason", snapshot.Blocked)
			}
			if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != blockedStatusState {
				t.Fatalf("updates = %#v, want final Blocked transition", tracker.updates)
			}
			if len(tracker.comments) == 0 || !strings.Contains(tracker.comments[len(tracker.comments)-1].body, "Test") {
				t.Fatalf("comments = %#v, want persistent missing-check detail", tracker.comments)
			}
		})
	}
}

func TestMergeRequiredCheckStreakClearsOnTerminalCompletion(t *testing.T) {
	t.Parallel()

	orch, tracker, state, closeStore := newMergeRequiredCheckTestOrchestrator(t, true)
	defer closeStore()
	now := time.Date(2026, 8, 7, 16, 0, 0, 0, time.UTC)
	step := mergeRequiredCheckEvaluationStep{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}}
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, step, now)
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, step, now.Add(time.Minute))

	terminalIssue := mergeRequiredCheckTestIssue(step)
	terminalIssue.State = "Done"
	orch.completeTerminalRunning(t.Context(), &state, terminalIssue.ID, Running{
		Issue:     terminalIssue,
		Attempt:   1,
		StartedAt: now.Add(2 * time.Minute),
		Mode:      runpkg.RunModeMerge,
	}, now.Add(3*time.Minute), TokenTotals{})
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, step, now.Add(4*time.Minute))

	if _, ok := state.Blocked[mergeRequiredCheckTestIssueID]; ok {
		t.Fatalf("Blocked[%q] present after terminal reset and one missing evaluation", mergeRequiredCheckTestIssueID)
	}
	if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != autoPromoteReworkState {
		t.Fatalf("updates = %#v, want Rework after reset", tracker.updates)
	}
}

func TestMergeRequiredCheckStreakClearsWhenBlocked(t *testing.T) {
	t.Parallel()

	orch, tracker, state, closeStore := newMergeRequiredCheckTestOrchestrator(t, true)
	defer closeStore()
	now := time.Date(2026, 8, 7, 16, 30, 0, 0, time.UTC)
	step := mergeRequiredCheckEvaluationStep{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}}
	for evaluation := range mergeWorkerMissingRequiredCheckLimit {
		runMergeRequiredCheckEvaluation(t, orch, tracker, &state, step, now.Add(time.Duration(evaluation)*time.Minute))
	}

	if _, ok := state.Blocked[mergeRequiredCheckTestIssueID]; !ok {
		t.Fatalf("Blocked[%q] missing after threshold", mergeRequiredCheckTestIssueID)
	}
	delete(state.Blocked, mergeRequiredCheckTestIssueID)
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, step, now.Add(time.Hour))

	if _, ok := state.Blocked[mergeRequiredCheckTestIssueID]; ok {
		t.Fatalf("Blocked[%q] present on first recovery evaluation", mergeRequiredCheckTestIssueID)
	}
	if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != autoPromoteReworkState {
		t.Fatalf("updates = %#v, want fresh Rework propagation window", tracker.updates)
	}
}

func TestMergeWorkerCurrentHeadCIWaitReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pullRequest *connector.PullRequest
		want        string
	}{
		{name: "missing pull request", want: "waiting for current-head CI"},
		{
			name: "unstarted check takes priority",
			pullRequest: &connector.PullRequest{
				UnstartedChecks:       []connector.PullRequestCheck{{Name: "Portability Verify", Status: "queued"}},
				RequiredCheckFailures: []connector.PullRequestCheck{{Name: "Portability Verify", Status: "queued"}},
				RunningChecks:         []string{"Test"},
			},
			want: "waiting for current-head CI: unstarted checks: Portability Verify",
		},
		{
			name: "pending required check",
			pullRequest: &connector.PullRequest{
				RequiredCheckFailures: []connector.PullRequestCheck{{Name: "Test", Status: "queued"}},
			},
			want: "waiting for current-head CI: pending required checks: Test",
		},
		{
			name:        "running check",
			pullRequest: &connector.PullRequest{RunningChecks: []string{"Test"}},
			want:        "waiting for current-head CI: pending checks: Test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := connector.Issue{PullRequest: tt.pullRequest}
			if got := mergeWorkerCurrentHeadCIWaitReason(issue); got != tt.want {
				t.Fatalf("mergeWorkerCurrentHeadCIWaitReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

const mergeRequiredCheckTestIssueID = "issue-persistent-missing-check"

type mergeRequiredCheckEvaluationStep struct {
	missing        bool
	headSHA        string
	requiredChecks []string
}

func newMergeRequiredCheckTestOrchestrator(
	t *testing.T,
	autoPromoteEnabled bool,
) (*Orchestrator, *autoPromoteTickMergeConnector, State, func()) {
	t.Helper()
	runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:    1,
		FailureRetryBaseDelay:  time.Minute,
		MaxRetryBackoff:        time.Hour,
		ContinuationRetryDelay: 5 * time.Second,
		MergeFastPathEnabled:   true,
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Merging"},
		TerminalStates:         []string{"Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled: autoPromoteEnabled,
			Gate: gate.Config{
				Kind:                 gate.KindCommand,
				RequiredStatusChecks: []string{"Test"},
			},
		},
	})
	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{},
	}
	orch := &Orchestrator{
		cfg:                 cfg,
		connector:           tracker,
		workflowMetrics:     runtimeStore,
		mergeRequiredChecks: runtimeStore,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return orch, tracker, newState(cfg), func() {
		if err := runtimeStore.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
}

func runMergeRequiredCheckEvaluation(
	t *testing.T,
	orch *Orchestrator,
	tracker *autoPromoteTickMergeConnector,
	state *State,
	step mergeRequiredCheckEvaluationStep,
	at time.Time,
) {
	t.Helper()
	orch.cfg.AutoPromote.Gate.RequiredStatusChecks = append([]string(nil), step.requiredChecks...)
	issue := mergeRequiredCheckTestIssue(step)
	tracker.stateIssues = []connector.Issue{cloneIssue(issue)}
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		Attempt:    maxMergeWorkerRunnerFailures,
		StartedAt:  at.Add(-time.Minute),
		WorkerHost: "worker-a",
		Mode:       runpkg.RunModeMerge,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: at.Add(-time.Minute)}
	orch.handleRunResult(context.Background(), state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: at,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeMerge},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     runpkg.RunOutputMergeFastPathClean,
		},
	})
}

func mergeRequiredCheckTestIssue(step mergeRequiredCheckEvaluationStep) connector.Issue {
	pullRequest := &connector.PullRequest{
		Number:         1634,
		URL:            "https://github.test/digitaldrywood/detent/pull/1634",
		BranchName:     "detent/1634",
		State:          "OPEN",
		MergeableState: "blocked",
		CIStatus:       "pending",
		HeadSHA:        step.headSHA,
	}
	if step.missing {
		pullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Test", Status: "missing", Conclusion: "missing"}}
	} else {
		pullRequest.RunningChecks = []string{"Test"}
	}
	return connector.Issue{
		ID:           mergeRequiredCheckTestIssueID,
		Identifier:   "digitaldrywood/detent#1634",
		State:        autoPromoteMergingState,
		PRRepository: "digitaldrywood/detent",
		PullRequest:  pullRequest,
	}
}
