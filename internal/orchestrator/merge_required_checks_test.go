package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"reflect"
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
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1},
				{headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 0},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 2},
			},
		},
		{
			name:               "check never arrives",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 2},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 3},
			},
			wantBlocked: true,
		},
		{
			name:               "config change mid-count",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test", "Lint"}, fingerprint: requiredCheckLintTestFingerprint, response: 1},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test", "Lint"}, fingerprint: requiredCheckLintTestFingerprint, response: 2},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test", "Checks"}, fingerprint: requiredCheckChecksTestFingerprint, response: 1},
			},
		},
		{
			name:               "head SHA change mid-count",
			autoPromoteEnabled: true,
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 2},
				{missing: true, headSHA: "head-b", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1},
			},
		},
		{
			name: "auto-promote disabled",
			steps: []mergeRequiredCheckEvaluationStep{
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 2},
				{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 3},
			},
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orch, tracker, state, script := newMergeRequiredCheckTestOrchestrator(t, tt.autoPromoteEnabled)
			now := time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)
			for index, step := range tt.steps {
				runMergeRequiredCheckEvaluation(t, orch, tracker, &state, script, step, now.Add(time.Duration(index)*time.Minute))
			}

			if len(state.Blocked) != 0 {
				t.Fatal("missing check entered Blocked")
			}
			if tt.wantBlocked {
				retry, ok := state.Retry[mergeRequiredCheckTestIssueID]
				if !ok || !strings.Contains(retry.Error, "Test") {
					t.Fatalf("missing check retry lost detail: %+v", retry)
				}
			}
			for _, update := range tracker.updates {
				if update.state == blockedStatusState {
					t.Fatal("missing check wrote Blocked")
				}
			}

		})
	}
}

func TestPersistentlyMissingRequiredCheckParkRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		recovery     mergeRequiredCheckEvaluationStep
		mutateConfig func(*Config)
		wantMerging  bool
	}{
		{name: "missing context now owned by Detent", recovery: mergeRequiredCheckEvaluationStep{missing: true, headSHA: "head-b", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint}, mutateConfig: func(cfg *Config) { cfg.AutoPromote.Gate.LocalStatus = "Test" }, wantMerging: true},
		{
			name:        "check appears on current head",
			recovery:    mergeRequiredCheckEvaluationStep{headSHA: "head-b", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint},
			wantMerging: true,
		},
		{
			name:     "check remains missing on current head",
			recovery: mergeRequiredCheckEvaluationStep{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint},
		},
		{
			name:     "check appears while merge fast path is disabled",
			recovery: mergeRequiredCheckEvaluationStep{headSHA: "head-b", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint},
			mutateConfig: func(cfg *Config) {
				cfg.MergeFastPathEnabled = false
			},
		},
		{
			name:     "check appears while merging lane is inactive",
			recovery: mergeRequiredCheckEvaluationStep{headSHA: "head-b", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint},
			mutateConfig: func(cfg *Config) {
				cfg.ActiveStates = []string{"Todo", "In Progress", "Rework"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orch, tracker, state, script := newMergeRequiredCheckTestOrchestrator(t, true)
			now := time.Date(2026, 8, 7, 16, 0, 0, 0, time.UTC)
			missing := mergeRequiredCheckEvaluationStep{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint}
			for evaluation, response := range []int{1, 2, 3} {
				missing.response = response
				runMergeRequiredCheckEvaluation(t, orch, tracker, &state, script, missing, now.Add(time.Duration(evaluation)*time.Minute))
			}
			if tt.mutateConfig != nil {
				tt.mutateConfig(&orch.cfg)
				orch.cfg = normalizeConfig(orch.cfg)
			}

			issue := mergeRequiredCheckTestIssue(tt.recovery)
			issue.State = blockedStatusState
			if !orch.recoverCauseBlockedIssue(t.Context(), &state, issue, now.Add(time.Hour)) {
				t.Fatal("historical machine hold was not recovered")
			}
			if len(state.Blocked) != 0 {
				t.Fatal("historical missing-check hold survived")
			}
			if _, ok := state.Retry[issue.ID]; !ok {
				t.Fatal("recovery did not schedule retry")
			}

		})
	}
}

func TestMergeRequiredCheckStreakClearsOnTerminalCompletion(t *testing.T) {
	t.Parallel()

	orch, tracker, state, script := newMergeRequiredCheckTestOrchestrator(t, true)
	now := time.Date(2026, 8, 7, 16, 0, 0, 0, time.UTC)
	step := mergeRequiredCheckEvaluationStep{missing: true, headSHA: "head-a", requiredChecks: []string{"Test"}, fingerprint: requiredCheckTestFingerprint, response: 1}
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, script, step, now)
	step.response = 2
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, script, step, now.Add(time.Minute))

	terminalIssue := mergeRequiredCheckTestIssue(step)
	terminalIssue.State = "Done"
	script.calls = append(script.calls, mergeRequiredCheckCall{method: "Clear", projectID: "default", issueID: "issue-persistent-missing-check"})
	if err := orch.completeTerminalRunning(t.Context(), &state, terminalIssue.ID, Running{
		Issue:     terminalIssue,
		Attempt:   1,
		StartedAt: now.Add(2 * time.Minute),
		Mode:      runpkg.RunModeMerge,
	}, now.Add(3*time.Minute), TokenTotals{}, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("completeTerminalRunning() error = %v", err)
	}
	script.assertConsumed()
	step.response = 1
	runMergeRequiredCheckEvaluation(t, orch, tracker, &state, script, step, now.Add(4*time.Minute))

	if _, ok := state.Blocked[mergeRequiredCheckTestIssueID]; ok {
		t.Fatalf("Blocked[%q] present after terminal reset and one missing evaluation", mergeRequiredCheckTestIssueID)
	}
	if len(tracker.updates) == 0 || tracker.updates[len(tracker.updates)-1].state != autoPromoteReworkState {
		t.Fatalf("updates = %#v, want Rework after reset", tracker.updates)
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
			if got := mergeWorkerCurrentHeadCIWaitReason(issue, ""); got != tt.want {
				t.Fatalf("mergeWorkerCurrentHeadCIWaitReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

const mergeRequiredCheckTestIssueID = "issue-persistent-missing-check"

const (
	requiredCheckTestFingerprint       = "95d98386f6adbac90c3d1cea7374a5b112a8f9cc44920633d66d25ba0f17ac1b"
	requiredCheckLintTestFingerprint   = "7155953c881ff9df55ee47caeea8cb73124d1526b0e046218579661754491f8c"
	requiredCheckChecksTestFingerprint = "7ae35dff3ab73269338841463352e28811db0b7f7cc3fae868f0444be398f9e9"
)

type mergeRequiredCheckEvaluationStep struct {
	missing        bool
	headSHA        string
	requiredChecks []string
	fingerprint    string
	response       int
}

type mergeRequiredCheckCall struct {
	method     string
	evaluation store.MergeRequiredCheckEvaluation
	response   []store.MergeRequiredCheckStreak
	projectID  string
	issueID    string
}

type mergeRequiredCheckScript struct {
	t     *testing.T
	calls []mergeRequiredCheckCall
	next  int
}

func (s *mergeRequiredCheckScript) take(method string) mergeRequiredCheckCall {
	s.t.Helper()
	if s.next == len(s.calls) {
		s.t.Fatalf("unexpected %s call at index %d", method, s.next)
	}
	call := s.calls[s.next]
	if call.method != method {
		s.t.Fatalf("call %d = %s, want %s", s.next, method, call.method)
	}
	s.next++
	return call
}

func (s *mergeRequiredCheckScript) EvaluateMergeRequiredChecks(_ context.Context, got store.MergeRequiredCheckEvaluation) ([]store.MergeRequiredCheckStreak, error) {
	s.t.Helper()
	call := s.take("Evaluate")
	if !reflect.DeepEqual(got, call.evaluation) {
		s.t.Fatalf("Evaluate call %d = %+v, want %+v", s.next-1, got, call.evaluation)
	}
	return append([]store.MergeRequiredCheckStreak(nil), call.response...), nil
}

func (s *mergeRequiredCheckScript) ClearMergeRequiredCheckStreaks(_ context.Context, projectID, issueID string) error {
	s.t.Helper()
	call := s.take("Clear")
	if projectID != call.projectID || issueID != call.issueID {
		s.t.Fatalf("Clear call %d = (%q,%q), want (%q,%q)", s.next-1, projectID, issueID, call.projectID, call.issueID)
	}
	return nil
}

func (s *mergeRequiredCheckScript) assertConsumed() {
	s.t.Helper()
	if s.next != len(s.calls) {
		s.t.Fatalf("consumed %d calls, want %d", s.next, len(s.calls))
	}
}

var _ store.MergeRequiredCheckStore = (*mergeRequiredCheckScript)(nil)

func newMergeRequiredCheckTestOrchestrator(
	t *testing.T,
	autoPromoteEnabled bool,
) (*Orchestrator, *autoPromoteTickMergeConnector, State, *mergeRequiredCheckScript) {
	t.Helper()
	script := &mergeRequiredCheckScript{t: t}
	t.Cleanup(script.assertConsumed)
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
		workflowMetrics:     &autoPromoteWorkflowMetricsRecorder{},
		mergeRequiredChecks: script,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return orch, tracker, newState(cfg), script
}

func runMergeRequiredCheckEvaluation(
	t *testing.T,
	orch *Orchestrator,
	tracker *autoPromoteTickMergeConnector,
	state *State,
	script *mergeRequiredCheckScript,
	step mergeRequiredCheckEvaluationStep,
	at time.Time,
) {
	t.Helper()
	missing := []string{}
	if step.missing {
		missing = []string{"Test"}
	}
	response := []store.MergeRequiredCheckStreak{}
	if step.response != 0 {
		response = []store.MergeRequiredCheckStreak{{CheckName: "Test", ConsecutiveMissing: step.response}}
	}
	script.calls = append(script.calls, mergeRequiredCheckCall{
		method: "Evaluate",
		evaluation: store.MergeRequiredCheckEvaluation{
			ProjectID: "default", IssueID: "issue-persistent-missing-check", Repository: "owner/repository", PRNumber: 1634,
			HeadSHA: step.headSHA, RequiredChecksFingerprint: step.fingerprint, MissingChecks: missing, EvaluatedAt: at,
		},
		response: response,
	})
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
	script.assertConsumed()
	retry, retrying := state.Retry["issue-persistent-missing-check"]
	const persistent = "merge_worker_required_checks_persistently_missing: Test"
	if step.response == 3 {
		if !retrying || retry.Error != persistent {
			t.Fatalf("threshold response was not consumed: retry=%+v, present=%t", retry, retrying)
		}
	} else if retrying && retry.Error == persistent {
		t.Fatalf("sub-threshold response selected persistent retry: %+v", retry)
	}
}

func mergeRequiredCheckTestIssue(step mergeRequiredCheckEvaluationStep) connector.Issue {
	pullRequest := &connector.PullRequest{
		Number:         1634,
		URL:            "https://github.test/owner/repository/pull/1634",
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
		Identifier:   "owner/repository#1634",
		State:        autoPromoteMergingState,
		PRRepository: "owner/repository",
		PullRequest:  pullRequest,
	}
}
