package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The runner reports what a turn spent to the execution, which accumulates
// the attempt total (decisions section 17.5).

type recordingUsageExecution struct {
	stubExecution
	entries []tracker.NativeUsage
	err     error
}

func (e *recordingUsageExecution) RecordUsage(_ context.Context, entry tracker.NativeUsage) error {
	e.entries = append(e.entries, entry)
	return e.err
}

func TestTurnUsageEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		backendKind  string
		sessionModel string
		result       RunResult
		want         tracker.NativeUsage
		wantReported bool
	}{
		{
			name: "codex turn", backendKind: "codex", sessionModel: "gpt-6-astra",
			result: RunResult{Tokens: TokenTotals{InputTokens: 1200, CachedInputTokens: 900, OutputTokens: 340}},
			want:   tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 1200, CachedInput: 900, Output: 340, Currency: "USD"},
		},
		{
			name: "claude code turn", backendKind: "claude_code", sessionModel: "claude-opus-5",
			result: RunResult{Tokens: TokenTotals{InputTokens: 10, OutputTokens: 2}},
			want:   tracker.NativeUsage{Provider: "claude", Model: "claude-opus-5", Input: 10, Output: 2, Currency: "USD"},
		},
		{
			name: "the resolved model wins over the requested one", backendKind: "codex", sessionModel: "auto",
			result: RunResult{Model: "gpt-5.6-sol", Tokens: TokenTotals{InputTokens: 5, OutputTokens: 1}},
			want:   tracker.NativeUsage{Provider: "codex", Model: "gpt-5.6-sol", Input: 5, Output: 1, Currency: "USD"},
		},
		{name: "a turn that spent nothing", backendKind: "codex", sessionModel: "gpt-6-astra"},
		{name: "no model to name", backendKind: "codex", result: RunResult{Tokens: TokenTotals{InputTokens: 5}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := turnUsageEntry(test.result, test.sessionModel, test.backendKind, func(string, int64, int64, int64, string) float64 { return 0 })
			if ok != (test.want != tracker.NativeUsage{}) {
				t.Fatalf("turnUsageEntry() reported = %t, want %t", ok, !ok)
			}
			if ok && got != test.want {
				t.Fatalf("turnUsageEntry() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestTurnUsageEntryPricesTheTurn(t *testing.T) {
	t.Parallel()
	result := RunResult{Tokens: TokenTotals{InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 100}}
	var sawModel, sawKind string
	var sawInput, sawCached, sawOutput int64
	entry, ok := turnUsageEntry(result, "gpt-6-astra", "codex", func(model string, input, cached, output int64, kind string) float64 {
		sawModel, sawKind, sawInput, sawCached, sawOutput = model, kind, input, cached, output
		return 0.125
	})
	if !ok {
		t.Fatal("turnUsageEntry() reported nothing for a turn that spent tokens")
	}
	if entry.CostEstimate != 0.125 || entry.Currency != "USD" {
		t.Fatalf("entry cost = %v %s, want 0.125 USD", entry.CostEstimate, entry.Currency)
	}
	if sawModel != "gpt-6-astra" || sawKind != "codex" || sawInput != 1000 || sawCached != 400 || sawOutput != 100 {
		t.Fatalf("pricing saw (%q, %d, %d, %d, %q)", sawModel, sawInput, sawCached, sawOutput, sawKind)
	}
}

func TestReportTurnUsage(t *testing.T) {
	t.Parallel()
	execution := &recordingUsageExecution{}
	result := RunResult{Tokens: TokenTotals{InputTokens: 10, OutputTokens: 2}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reportTurnUsage(t.Context(), execution, result, "gpt-6-astra", "codex", func(string, int64, int64, int64, string) float64 { return 0.5 }, logger)
	if len(execution.entries) != 1 {
		t.Fatalf("recorded %d entries, want 1", len(execution.entries))
	}
	if execution.entries[0].Provider != "codex" || execution.entries[0].CostEstimate != 0.5 {
		t.Fatalf("recorded %+v", execution.entries[0])
	}
	// An execution that does not record usage, and a nil one, are both fine.
	reportTurnUsage(t.Context(), stubExecution{}, result, "gpt-6-astra", "codex", func(string, int64, int64, int64, string) float64 { return 0 }, logger)
	reportTurnUsage(t.Context(), nil, result, "gpt-6-astra", "codex", func(string, int64, int64, int64, string) float64 { return 0 }, logger)

	// A hub that refuses the report does not fail the turn, and a runner
	// with no logger still reports (decisions section 17.5).
	failing := &recordingUsageExecution{err: errors.New("hub refused")}
	reportTurnUsage(t.Context(), failing, result, "gpt-6-astra", "codex", nil, logger)
	reportTurnUsage(t.Context(), failing, result, "gpt-6-astra", "codex", nil, nil)
	if len(failing.entries) != 2 {
		t.Fatalf("a refused report recorded %d entries, want 2 attempts", len(failing.entries))
	}
}

// stubExecution satisfies Execution without doing anything, so a test can
// add only the capability it is about.
type stubExecution struct{}

func (stubExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
func (stubExecution) Validate(context.Context) error                               { return nil }
func (stubExecution) Start(context.Context, tracker.NativeExecutionIdentity) error { return nil }
func (stubExecution) Checkpoint(context.Context, tracker.NativeCheckpoint) error   { return nil }
func (stubExecution) Finish(context.Context, string) error                         { return nil }
func (stubExecution) Recovery() tracker.NativeRecovery                             { return tracker.NativeRecovery{} }
