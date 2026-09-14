package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Per-attempt usage ingestion (decisions section 17.5). A run event carries
// the attempt's running total, so each event replaces the attempt's rows
// rather than adding to them: a redelivered event cannot double-count, and a
// runner that reports nothing leaves the stored total alone.

const (
	// maxNativeUsageEntries bounds one report: a turn uses one model, and an
	// attempt that touched more than this many is not a usage problem.
	maxNativeUsageEntries = 32
	// maxNativeUsageNameBytes bounds a provider or model identifier.
	maxNativeUsageNameBytes = 128
	// usageDayLayout is how attempt_usage.day is stored: a UTC calendar day
	// that sorts and compares as a string.
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
		key := provider + "\x00" + model
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

// attemptUsageDay is the day an attempt's usage is filed under: the day the
// attempt started, so an attempt that runs past midnight keeps one row per
// provider and model instead of writing its running total twice. An attempt
// the hub has no start for is filed under the event.
func attemptUsageDay(ctx context.Context, tx *sql.Tx, attemptID string, now time.Time) (string, error) {
	var started string
	err := tx.QueryRowContext(ctx, "SELECT started_at FROM native_attempts WHERE id = ?", attemptID).Scan(&started)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return now.UTC().Format(usageDayLayout), nil
		}
		return "", fmt.Errorf("read attempt start: %w", err)
	}
	at, err := parseTimeValue(started)
	if err != nil {
		return now.UTC().Format(usageDayLayout), nil
	}
	return at.UTC().Format(usageDayLayout), nil
}

// recordAttemptUsage stores what a run event reports. The rows are replaced,
// not added to: the event carries the attempt's running total.
func recordAttemptUsage(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.NativeRunEvent, prices UsageConfig, now time.Time) error {
	usage := request.Data.Usage
	if len(usage) == 0 {
		return nil
	}
	attempt := request.Data.AttemptID
	day, err := attemptUsageDay(ctx, tx, attempt, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM attempt_usage WHERE attempt_id = ?", attempt); err != nil {
		return fmt.Errorf("clear attempt usage: %w", err)
	}
	for _, entry := range usage {
		provider := tracker.UsageProviderID(entry.Provider)
		model := strings.TrimSpace(entry.Model)
		cost, currency := entry.CostEstimate, strings.TrimSpace(entry.Currency)
		if cost <= 0 {
			// The runner had no price table for this model; the hub's own
			// table answers for it.
			estimated, priced, found := prices.estimate(model, entry.Input, entry.CachedInput, entry.Output)
			if found {
				cost, currency = estimated, priced
			}
		}
		if currency == "" {
			currency = prices.currency()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_usage
 (attempt_id, organization_id, project_id, day, provider, model, input, cached_input, output, cost_estimate, currency, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (attempt_id, day, provider, model) DO UPDATE SET
 organization_id = excluded.organization_id, project_id = excluded.project_id,
 input = excluded.input, cached_input = excluded.cached_input, output = excluded.output,
 cost_estimate = excluded.cost_estimate, currency = excluded.currency, updated_at = excluded.updated_at`,
			attempt, scope.organization, scope.project, day, provider, model,
			entry.Input, entry.CachedInput, entry.Output, cost, currency, conversationTime(now)); err != nil {
			return fmt.Errorf("record attempt usage: %w", err)
		}
	}
	return nil
}
