package runner

import (
	"context"
	"log/slog"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Per-turn usage reporting (decisions section 17.5). The runner already
// prices a whole session for its own store; the hub wants the same numbers
// per turn so the usage report can group them by day, provider and model.
// The execution accumulates the attempt total, so the runner only has to
// say what this turn cost.

// UsageExecution is an Execution that records what a turn spent. An
// execution without it simply reports no usage, which is what a runner
// working against a hub that predates the report does.
type UsageExecution interface {
	RecordUsage(context.Context, tracker.NativeUsage) error
}

// usagePricer prices a turn. It matches (*Runner).usageCostUSD so the hub
// report and the runner's own usage events agree on the estimate.
type usagePricer func(model string, input, cachedInput, output int64, backendKind string) float64

// usageCurrency is what the runner's price table is denominated in. The hub
// keeps its own table and its own currency; this is only what a runner-side
// estimate means when it arrives.
const usageCurrency = "USD"

// turnUsageEntry describes what one turn spent. It reports false when there
// is nothing worth storing: no tokens, or no model to attribute them to.
func turnUsageEntry(result RunResult, sessionModel, backendKind string, price usagePricer) (tracker.NativeUsage, bool) {
	model := strings.TrimSpace(effectiveModel(
		effectiveModel(result.RuntimeIdentity.ResolvedModel.Value, result.Model),
		strings.TrimSpace(sessionModel),
	))
	if model == "" {
		return tracker.NativeUsage{}, false
	}
	entry := tracker.NativeUsage{
		Provider:    tracker.UsageProviderID(backendKind),
		Model:       model,
		Input:       max(result.Tokens.InputTokens, 0),
		CachedInput: max(result.Tokens.CachedInputTokens, 0),
		Output:      max(result.Tokens.OutputTokens, 0),
		Currency:    usageCurrency,
	}
	if entry.Input == 0 && entry.CachedInput == 0 && entry.Output == 0 {
		return tracker.NativeUsage{}, false
	}
	if price != nil {
		entry.CostEstimate = max(price(model, entry.Input, entry.CachedInput, entry.Output, backendKind), 0)
	}
	return entry, true
}

// reportTurnUsage hands one turn's usage to the execution. Usage is
// bookkeeping: a hub that cannot take it must not fail the turn, so a
// refusal is logged rather than returned.
func reportTurnUsage(ctx context.Context, execution Execution, result RunResult, sessionModel, backendKind string, price usagePricer, logger *slog.Logger) {
	recorder, ok := execution.(UsageExecution)
	if !ok || recorder == nil {
		return
	}
	entry, reportable := turnUsageEntry(result, sessionModel, backendKind, price)
	if !reportable {
		return
	}
	if err := recorder.RecordUsage(ctx, entry); err != nil && logger != nil {
		logger.Warn("runner.usage_report_failed", "model", entry.Model, "provider", entry.Provider, "error", err)
	}
}
