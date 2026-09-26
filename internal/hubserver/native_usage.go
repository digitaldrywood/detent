package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Per-attempt usage ingestion (decisions section 17.5). A run event carries
// the attempt's running total; the hub records only the increment over what
// it already holds for the attempt, filed under the hour the event arrived.
// A redelivered event therefore adds nothing, spend after midnight lands on
// the day it happened, and a runner that reports nothing leaves the stored
// total alone.

const (
	// maxNativeUsageEntries bounds one report: a turn uses one model, and an
	// attempt that touched more than this many is not a usage problem.
	maxNativeUsageEntries = 32
	// maxNativeUsageNameBytes bounds a provider or model identifier.
	maxNativeUsageNameBytes = 128
	// usagePeriodLayout is how attempt_usage.period is stored: a fixed-width
	// UTC hour start that sorts and compares as a string.
	usagePeriodLayout = "2006-01-02T15:04:05Z"
	// usageDayLayout is how a daily report bucket is written.
	usageDayLayout = "2006-01-02"
)

// validateNativeRunUsage checks the usage a run event carries. Usage belongs
// to a checkpoint or a finish; a run that has only started has spent nothing
// worth reporting.
func validateNativeRunUsage(request tracker.NativeRunEvent) error {
	usage := request.Data.Usage
	if len(usage) == 0 {
		return nil
	}
	if request.Type == "run.started" {
		return nativeInvalid("Usage requires a checkpoint or a finished run")
	}
	if len(usage) > maxNativeUsageEntries {
		return nativeInvalid(fmt.Sprintf("A run event carries at most %d usage entries", maxNativeUsageEntries))
	}
	seen := make(map[string]bool, len(usage))
	for _, entry := range usage {
		provider := strings.TrimSpace(entry.Provider)
		model := strings.TrimSpace(entry.Model)
		if provider == "" || model == "" || len(provider) > maxNativeUsageNameBytes || len(model) > maxNativeUsageNameBytes {
			return nativeInvalid("Usage requires a provider and a model of at most 128 bytes each")
		}
		key := tracker.UsageProviderID(provider) + "\x00" + model
		if seen[key] {
			return nativeInvalid("Usage names " + provider + " " + model + " twice")
		}
		seen[key] = true
		if entry.Input < 0 || entry.CachedInput < 0 || entry.Output < 0 || entry.CostEstimate < 0 {
			return nativeInvalid("Usage counts and cost cannot be negative")
		}
	}
	return nil
}

// recordAttemptUsage stores what a run event reports. The event carries the
// attempt's running total; the hub subtracts what it already recorded for
// the attempt and files the remainder under the hour the event arrived.
func recordAttemptUsage(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.NativeRunEvent, prices UsageConfig, now time.Time) error {
	usage := request.Data.Usage
	if len(usage) == 0 {
		return nil
	}
	attempt := request.Data.AttemptID
	period := usagePeriod(now)
	currency := prices.currency()
	for _, entry := range usage {
		provider := tracker.UsageProviderID(entry.Provider)
		model := strings.TrimSpace(entry.Model)
		var recorded tracker.NativeUsage
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(input), 0), coalesce(sum(cached_input), 0), coalesce(sum(output), 0), coalesce(sum(CASE WHEN currency = ? THEN cost_estimate ELSE 0 END), 0)
 FROM attempt_usage WHERE attempt_id = ? AND provider = ? AND model = ?`, currency, attempt, provider, model).
			Scan(&recorded.Input, &recorded.CachedInput, &recorded.Output, &recorded.CostEstimate); err != nil {
			return fmt.Errorf("read recorded attempt usage: %w", err)
		}
		delta := usageDelta(entry, recorded, prices)
		if delta.Input == 0 && delta.CachedInput == 0 && delta.Output == 0 && delta.CostEstimate == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_usage
 (attempt_id, organization_id, project_id, period, provider, model, input, cached_input, output, cost_estimate, currency, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (attempt_id, period, provider, model, currency) DO UPDATE SET
 input = input + excluded.input, cached_input = cached_input + excluded.cached_input, output = output + excluded.output,
 cost_estimate = cost_estimate + excluded.cost_estimate, updated_at = excluded.updated_at`,
			attempt, scope.organization, scope.project, period, provider, model,
			delta.Input, delta.CachedInput, delta.Output, delta.CostEstimate, currency, formatHubTime(now)); err != nil {
			return fmt.Errorf("record attempt usage: %w", err)
		}
	}
	return nil
}

// usageDelta is the part of a reported running total the hub has not yet
// recorded. A count that went backwards contributes nothing rather than
// debt. Cost is the runner's own estimate when it priced the usage in the
// hub's currency; otherwise the hub prices the new tokens from its table, so
// every stored row is denominated in the hub's one currency.
func usageDelta(total, recorded tracker.NativeUsage, prices UsageConfig) tracker.NativeUsage {
	delta := tracker.NativeUsage{
		Input:       max(total.Input-recorded.Input, 0),
		CachedInput: max(total.CachedInput-recorded.CachedInput, 0),
		Output:      max(total.Output-recorded.Output, 0),
	}
	runnerCurrency := strings.ToUpper(strings.TrimSpace(total.Currency))
	if total.CostEstimate > 0 && (runnerCurrency == "" || runnerCurrency == prices.currency()) {
		delta.CostEstimate = max(total.CostEstimate-recorded.CostEstimate, 0)
		return delta
	}
	if estimated, _, found := prices.estimate(strings.TrimSpace(total.Model), delta.Input, delta.CachedInput, delta.Output); found {
		delta.CostEstimate = estimated
	}
	return delta
}

// usagePeriod is the hour bucket a moment is filed under.
func usagePeriod(at time.Time) string {
	return at.UTC().Truncate(time.Hour).Format(usagePeriodLayout)
}
