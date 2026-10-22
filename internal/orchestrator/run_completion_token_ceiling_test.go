package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestHandleRunResultRetriesSlowTokenCeilingFailure(t *testing.T) {
	t.Parallel()

	tracker := &dependencyAutoUnblockConnector{}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress", "Rework"},
		ObservedStates: []string{"Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	state := newState(cfg)
	issue := connector.Issue{
		ID:         "issue-token-ceiling",
		Identifier: "digitaldrywood/detent#1221",
		Title:      "Token ceiling retry loop",
		State:      "In Progress",
	}
	completedAt := time.Date(2026, 7, 10, 23, 5, 0, 0, time.UTC)
	state.Running[issue.ID] = Running{
		Issue:     issue,
		Attempt:   1,
		StartedAt: completedAt.Add(-5 * time.Minute),
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: completedAt.Add(-5 * time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateTokenCeilingExceeded,
			Tokens:     TokenTotals{TotalTokens: 16_100_000, RuntimeSeconds: 300},
		},
		Err: &runpkg.SessionTokenCeilingError{
			TotalTokens:   16_100_000,
			CeilingTokens: 16_000_000,
			Source:        runpkg.TokenCeilingSourceAbsolute,
		},
		CompletedAt:  completedAt,
		RetryAttempt: 2,
		RetryDelay:   time.Minute,
	})

	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 2 || retry.Issue.State != "In Progress" || !retry.DueAt.After(completedAt) || retry.DueAt.After(completedAt.Add(cfg.MaxRetryBackoff)) {
		t.Fatalf("failure did not retain bounded retry: %+v", retry)
	}
	if len(state.Blocked) != 0 || len(state.Claimed) != 0 || len(tracker.updates) != 0 {
		t.Fatal("machine failure retained a hold or changed the executable lane")
	}
	if retry.Error == "" {
		t.Fatal("retry lost failure evidence")
	}
}

func TestHandleRunResultRetriesBudgetProjectionFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	tracker := &dependencyAutoUnblockConnector{}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress", "Rework"},
		ObservedStates: []string{"Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	state := newState(cfg)
	issue := connector.Issue{
		ID:         "issue-budget-projection",
		Identifier: "digitaldrywood/example-project#2255",
		State:      "In Progress",
	}
	completedAt := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, StartedAt: completedAt.Add(-time.Hour)}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: completedAt.Add(-time.Hour)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Issue: issue, Attempt: 1, Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState:  runpkg.FinalStateBudgetProjectionExceeded,
			TurnStarted: true,
			Tokens:      TokenTotals{TotalTokens: 51_900_000},
		},
		Err: &runpkg.SessionBudgetProjectionError{
			ObservedCostUSD:  40.25,
			ProjectedCostUSD: 10.00,
			Model:            "gpt-5.6-sol",
			EstimateSource:   budget.EstimateSourceHistorical,
		},
		CompletedAt:  completedAt,
		Retryable:    false,
		RetryAttempt: 2,
		RetryDelay:   time.Minute,
	})

	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 2 || retry.Issue.State != "In Progress" || !retry.DueAt.After(completedAt) || retry.DueAt.After(completedAt.Add(cfg.MaxRetryBackoff)) {
		t.Fatalf("failure did not retain bounded retry: %+v", retry)
	}
	if len(state.Blocked) != 0 || len(state.Claimed) != 0 || len(tracker.updates) != 0 {
		t.Fatal("machine failure retained a hold or changed the executable lane")
	}
	if retry.Error == "" {
		t.Fatal("retry lost failure evidence")
	}
}
