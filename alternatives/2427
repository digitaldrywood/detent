package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/compute"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestTerminalAttemptPersistsSessionUsage(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		err   error
		final string
	}{
		{"session timeout", errors.Join(runpkg.ErrSessionDurationExceeded, context.DeadlineExceeded), runpkg.FinalStateSessionDurationExceeded},
		{"turn timeout", errors.Join(runpkg.ErrTurnDurationExceeded, context.DeadlineExceeded), "failed"},
		{"failure", errors.New("worker failed"), "failed"},
		{"success", nil, "completed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := terminalRetryTestIssue("metrics")
			attempts := &terminalRetryWorkAttemptStore{}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
			o := &Orchestrator{cfg: cfg, connector: &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}, workAttempts: attempts}
			state := newState(cfg)
			now := time.Now()
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 42, Mode: runpkg.RunModeImplement, StartedAt: now.Add(-2 * time.Hour), TurnCount: 2, Tokens: TokenTotals{InputTokens: 567990, OutputTokens: 4310, TotalTokens: 572300}}
			totals := TokenTotals{InputTokens: 60500000, OutputTokens: 500000, TotalTokens: 61000000, RuntimeSeconds: 7321.9}
			o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Result: runpkg.RunResult{TurnStarted: true, TurnCount: 120, Tokens: totals, TokenUSD: 1.25, Compute: &compute.Usage{CPUSeconds: 3, AvgMemoryBytes: 1e9, WallSeconds: 5, ComputeUSD: .0001}, FinalState: tt.final}, Err: tt.err, CompletedAt: now})
			if len(attempts.completions) != 1 {
				t.Fatalf("completions = %d, want 1", len(attempts.completions))
			}
			completion := attempts.completions[0]
			if tt.name == "session timeout" && completion.TerminalState != store.WorkAttemptTerminalTimedOut {
				t.Fatalf("terminal state = %s", completion.TerminalState)
			}
			var metrics struct {
				compute.Usage
				TokenUSD float64 `json:"token_usd"`
				Input    int64   `json:"input_tokens"`
				Output   int64   `json:"output_tokens"`
				Total    int64   `json:"total_tokens"`
				Runtime  float64 `json:"runtime_seconds"`
				Turns    int     `json:"turns"`
			}
			if err := json.Unmarshal([]byte(completion.MetricsJSON), &metrics); err != nil {
				t.Fatal(err)
			}
			if metrics.CPUSeconds != 3 || metrics.AvgMemoryBytes != 1e9 || metrics.WallSeconds != 5 || metrics.ComputeUSD != .0001 || metrics.TokenUSD != 1.25 {
				t.Fatalf("compute metrics = %+v", metrics)
			}
			if metrics.Turns != 120 {
				t.Fatalf("turns = %d, want 120", metrics.Turns)
			}
			if metrics.Total != totals.TotalTokens || metrics.Input != totals.InputTokens || metrics.Output != totals.OutputTokens || metrics.Runtime != totals.RuntimeSeconds {
				t.Fatalf("metrics = %+v, want session totals %+v", metrics, totals)
			}
		})
	}
}
