package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type UsageExecution interface {
	RecordUsage(context.Context, tracker.NativeUsage) error
}

type ProcessUsageExecution interface {
	RecordProcessUsage(procgroup.Usage)
}

// usagePricer prices a turn. It matches (*Runner).usageCostUSD so the hub
// report and the runner's own usage events agree on the estimate.
type usagePricer func(model string, input, cachedInput, output int64, backendKind string) float64

// usageCurrency is what the runner's price table is denominated in. The hub
// keeps its own table and its own currency; this is only what a runner-side
// estimate means when it arrives.
const usageCurrency = "USD"

func turnUsageEntry(result RunResult, sessionModel, backendKind string, price usagePricer, includeSession bool) (tracker.NativeUsage, bool) {
	model := strings.TrimSpace(effectiveModel(
		effectiveModel(result.RuntimeIdentity.ResolvedModel.Value, result.Model),
		strings.TrimSpace(sessionModel),
	))
	if model == "" {
		return tracker.NativeUsage{}, false
	}
	entry := tracker.NativeUsage{
		BillingMode:        result.BillingMode,
		ReportedCostMicros: result.ReportedCostMicros,
		CostSource:         result.CostSource,
		Provider:           tracker.UsageProviderID(backendKind),
		Model:              model,
		Input:              max(result.Tokens.InputTokens, 0),
		CachedInput:        max(result.Tokens.CachedInputTokens, 0),
		Output:             max(result.Tokens.OutputTokens, 0),
		Currency:           usageCurrency,
	}
	if entry.Input == 0 && entry.CachedInput == 0 && entry.Output == 0 && entry.ReportedCostMicros == nil && !includeSession {
		return tracker.NativeUsage{}, false
	}
	if price != nil {
		entry.CostEstimate = max(price(model, entry.Input, entry.CachedInput, entry.Output, backendKind), 0)
	}
	return entry, true
}

func reportTurnUsage(ctx context.Context, execution Execution, result RunResult, sessionModel, backendKind string, price usagePricer, logger *slog.Logger) error {
	recorder, ok := execution.(UsageExecution)
	if !ok || recorder == nil {
		return nil
	}
	entry, reportable := turnUsageEntry(result, sessionModel, backendKind, price, false)
	if !reportable {
		return nil
	}
	if err := recorder.RecordUsage(ctx, entry); err != nil {
		return fmt.Errorf("publish Hub usage: %w", err)
	}
	return nil
}

type turnUsageReporter struct {
	baseline  TokenTotals
	sourceID  string
	revision  int64
	startedAt time.Time
	published *tracker.NativeUsage
}

func (u *turnUsageReporter) report(ctx context.Context, execution Execution, result RunResult, model, backend string, price usagePricer, finished bool) error {
	recorder, ok := execution.(UsageExecution)
	if !ok {
		return nil
	}
	result.Tokens.InputTokens = max(0, result.Tokens.InputTokens-u.baseline.InputTokens)
	result.Tokens.CachedInputTokens = max(0, result.Tokens.CachedInputTokens-u.baseline.CachedInputTokens)
	result.Tokens.OutputTokens = max(0, result.Tokens.OutputTokens-u.baseline.OutputTokens)
	entry, reportable := turnUsageEntry(result, model, backend, price, finished && result.TurnStarted && !result.TurnStartRefused)
	if !reportable {
		return nil
	}
	if u.sourceID == "" {
		return errors.New("publish Hub usage: session identity is required")
	}
	if u.published != nil {
		previous := *u.published
		previous.SourceID, previous.UsageKind, previous.Revision, previous.From = "", "", 0, time.Time{}
		if reflect.DeepEqual(previous, entry) {
			return nil
		}
	}
	u.revision++
	entry.SourceID, entry.UsageKind, entry.Revision, entry.From = u.sourceID, "incremental", u.revision, u.startedAt
	if err := recorder.RecordUsage(ctx, entry); err != nil {
		return fmt.Errorf("publish Hub usage: %w", err)
	}
	u.published = &entry
	return nil
}
