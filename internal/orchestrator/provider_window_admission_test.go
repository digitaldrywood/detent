package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestProviderWindowAdmissionDecisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		issue               connector.Issue
		running             int
		modelPermitRequired *bool
		wantDispatchable    bool
		wantReason          string
	}{
		{
			name:             "checked head starts mechanically",
			issue:            providerWindowMergeIssue("fast-path"),
			running:          3,
			wantDispatchable: true,
		},
		{
			name:                "prechecked fallback needs model permit",
			issue:               providerWindowMergeIssue("fallback"),
			running:             3,
			modelPermitRequired: boolPointer(true),
			wantReason:          dispatchSkipRateWindowBackpressure,
		},
		{
			name:       "ordinary work needs model permit",
			issue:      dispatchTestIssue("ordinary", "Todo"),
			running:    3,
			wantReason: dispatchSkipRateWindowBackpressure,
		},
		{
			name:       "mechanical work still obeys hard project cap",
			issue:      providerWindowMergeIssue("hard-cap"),
			running:    5,
			wantReason: dispatchSkipGlobalCapacityFull,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := providerWindowTestConfig()
			state := providerWindowState(cfg, tt.running)
			planner := newDispatchPlanner(cfg)
			var decision dispatchableDecision
			if tt.modelPermitRequired == nil {
				decision = planner.dispatchableIssueDecision(tt.issue, &state, false, time.Now(), "")
			} else {
				decision = planner.dispatchableIssueDecisionForModelRequirement(
					tt.issue,
					&state,
					false,
					time.Now(),
					"",
					*tt.modelPermitRequired,
				)
			}
			if decision.dispatchable != tt.wantDispatchable || decision.reason != tt.wantReason {
				t.Fatalf("dispatchable decision = %#v, want dispatchable=%t reason=%q", decision, tt.wantDispatchable, tt.wantReason)
			}
		})
	}
}

func TestProviderWindowPlanAdmitsMechanicalMerge(t *testing.T) {
	t.Parallel()

	cfg := providerWindowTestConfig()
	state := providerWindowState(cfg, 3)
	ordinary := dispatchTestIssue("ordinary", "Todo")
	merge := providerWindowMergeIssue("merge")
	var decisions []dispatchPlanDecision

	plan := newDispatchPlanner(cfg).plan(
		&state,
		[]connector.Issue{ordinary, merge},
		time.Now(),
		dispatchPlanHooks{decision: func(decision dispatchPlanDecision) {
			decisions = append(decisions, decision)
		}},
	)

	if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != merge.ID {
		t.Fatalf("dispatches = %#v, want only mechanical merge", plan.Dispatches)
	}
	if running := state.Running[merge.ID]; !running.ModelPermitExempt {
		t.Fatalf("merge Running = %#v, want model permit exempt during precheck", running)
	}
	if len(decisions) != 2 || !decisions[0].Selected || decisions[1].SkipReason != dispatchSkipRateWindowBackpressure {
		t.Fatalf("decisions = %#v, want selected merge then model backpressure", decisions)
	}
}

func TestProviderWindowFallbackRetryRemainsDeferred(t *testing.T) {
	t.Parallel()

	cfg := providerWindowTestConfig()
	state := providerWindowState(cfg, 3)
	issue := providerWindowMergeIssue("fallback")
	precheck := &runpkg.MergePrecheck{Status: "conflict", Message: "README conflict"}
	state.Retry[issue.ID] = Retry{
		Issue:         issue,
		Attempt:       1,
		DueAt:         time.Now().Add(-time.Second),
		MergePrecheck: precheck,
	}
	var decision dispatchPlanDecision

	plan := newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, time.Now(), dispatchPlanHooks{
		decision: func(got dispatchPlanDecision) {
			decision = got
		},
	})

	if len(plan.Dispatches) != 0 || decision.SkipReason != dispatchSkipRateWindowBackpressure {
		t.Fatalf("plan = %#v decision = %#v, want deferred provider backpressure", plan, decision)
	}
	retry := state.Retry[issue.ID]
	if retry.MergePrecheck == nil || retry.MergePrecheck.Status != precheck.Status || retry.MergePrecheck.Message != precheck.Message {
		t.Fatalf("retry = %#v, want preserved merge precheck", retry)
	}
}

func TestProviderWindowMechanicalToModelTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		modelRuns       int
		wantErr         error
		wantExempt      bool
		wantPermitsUsed int
	}{
		{name: "defer at scaled cap", modelRuns: 3, wantErr: runpkg.ErrModelPermitUnavailable, wantExempt: true, wantPermitsUsed: 3},
		{name: "acquire below scaled cap", modelRuns: 2, wantExempt: false, wantPermitsUsed: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := providerWindowTestConfig()
			state := providerWindowState(cfg, tt.modelRuns)
			issue := providerWindowMergeIssue("transition")
			state.Running[issue.ID] = Running{Issue: issue, ModelPermitExempt: true}
			orch := &Orchestrator{cfg: cfg}

			err := orch.handleModelPermitRequest(&state, issue.ID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("handleModelPermitRequest() error = %v, want %v", err, tt.wantErr)
			}
			if got := state.Running[issue.ID].ModelPermitExempt; got != tt.wantExempt {
				t.Fatalf("ModelPermitExempt = %t, want %t", got, tt.wantExempt)
			}
			if got := modelPermitsUsed(&state); got != tt.wantPermitsUsed {
				t.Fatalf("modelPermitsUsed() = %d, want %d", got, tt.wantPermitsUsed)
			}
		})
	}
}

func TestProviderWindowScaledCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		remaining int64
		want      int
	}{
		{remaining: 100, want: 5},
		{remaining: 81, want: 5},
		{remaining: 80, want: 4},
		{remaining: 61, want: 4},
		{remaining: 60, want: 3},
		{remaining: 41, want: 3},
		{remaining: 40, want: 2},
		{remaining: 21, want: 2},
		{remaining: 20, want: 1},
		{remaining: 1, want: 1},
		{remaining: 0, want: 1},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("remaining_%d", tt.remaining), func(t *testing.T) {
			t.Parallel()

			cfg := providerWindowTestConfig()
			state := newState(cfg)
			state.RateLimits = providerRateLimits(tt.remaining, 100)
			if got := newDispatchPlanner(cfg).providerModelPermitSlots(&state); got != tt.want {
				t.Fatalf("providerModelPermitSlots() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProviderWindowPacingOffNeverBackpressures(t *testing.T) {
	t.Parallel()

	cfg := providerWindowTestConfig()
	cfg.RateWindowPacing = workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}.Normalized()
	state := providerWindowState(cfg, 1)
	issue := dispatchTestIssue("candidate", "Todo")
	decision := newDispatchPlanner(cfg).dispatchableIssueDecision(issue, &state, false, time.Now(), "")
	if !decision.dispatchable || decision.reason == dispatchSkipRateWindowBackpressure {
		t.Fatalf("dispatchable decision = %#v, want dispatchable without provider backpressure", decision)
	}
}

func TestModelPermitDeferralReleasesHardCapacityAndPreservesPrecheck(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 22, 41, 0, 0, time.UTC)
	cfg := providerWindowTestConfig()
	state := providerWindowState(cfg, 3)
	issue := providerWindowMergeIssue("deferred")
	state.Running[issue.ID] = Running{
		Issue:             issue,
		Attempt:           1,
		StartedAt:         now.Add(-time.Second),
		ModelPermitExempt: true,
	}
	precheck := &runpkg.MergePrecheck{Status: "dirty", Message: "local changes", DiffStats: DiffStats{FilesChanged: 1}}
	resumeState := store.AgentResumeState{
		DetentSessionID:   1659,
		ProviderThreadID:  "thread-1659",
		ProviderSessionID: "session-1659",
		Orphaned:          true,
	}
	orch := &Orchestrator{cfg: cfg}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{
			Issue:       issue,
			Mode:        runpkg.RunModeMerge,
			RetryMode:   runpkg.RetryModeResume,
			ResumeState: resumeState,
		},
		Result:      runpkg.RunResult{Output: runpkg.RunOutputMergeFallbackDeferred, MergePrecheck: precheck},
		Err:         runpkg.ErrModelPermitUnavailable,
		CompletedAt: now,
	})

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after fallback deferral", issue.ID)
	}
	if got := newDispatchPlanner(cfg).hardAvailableSlots(&state); got != 2 {
		t.Fatalf("hardAvailableSlots() = %d, want 2 after mechanical slot release", got)
	}
	retry := state.Retry[issue.ID]
	if retry.MergePrecheck == nil || retry.MergePrecheck.Status != precheck.Status || retry.MergePrecheck.Message != precheck.Message {
		t.Fatalf("retry = %#v, want preserved precheck", retry)
	}
	if retry.RetryMode != runpkg.RetryModeResume || retry.ResumeState != resumeState {
		t.Fatalf("retry = %#v, want preserved resume metadata", retry)
	}
}

func providerWindowTestConfig() Config {
	return normalizeConfig(Config{
		BillingMode:          workflowconfig.BillingModeSubscription,
		MaxConcurrentAgents:  5,
		MergeFastPathEnabled: true,
		ActiveStates:         []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:       []string{"Done", "Cancelled"},
	})
}

func providerWindowState(cfg Config, runningCount int) State {
	state := newState(cfg)
	state.RateLimits = providerRateLimits(48, 100)
	for index := range runningCount {
		issue := dispatchTestIssue(fmt.Sprintf("running-%d", index), "In Progress")
		state.Running[issue.ID] = Running{Issue: issue}
	}
	return state
}

func providerWindowMergeIssue(id string) connector.Issue {
	issue := dispatchTestIssue(id, "Merging")
	issue.PullRequest = &connector.PullRequest{
		State:          "open",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "checked-head",
	}
	return issue
}

func boolPointer(value bool) *bool {
	return &value
}
