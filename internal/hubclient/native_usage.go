package hubclient

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Per-attempt usage (decisions section 17.5). The runner reports what one
// turn spent; the execution keeps the attempt's running total on its run
// data, so the next run.checkpointed or run.finished carries it without a
// second request. Reporting the total rather than a delta is what makes a
// redelivered event safe: the hub overwrites the row instead of adding to it.

// RecordUsage adds one turn's usage to the attempt's running total. A report
// that names no provider or model, or that spent nothing, changes nothing.
func (e *nativeExecution) RecordUsage(_ context.Context, entry tracker.NativeUsage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, existing := range e.data.Usage {
		if existing.Provider == strings.TrimSpace(entry.Provider) && existing.Model == strings.TrimSpace(entry.Model) && existing.Currency != "" && entry.Currency != "" && existing.Currency != entry.Currency {
			return errors.New("attempt usage cannot combine currencies for the same provider and model")
		}
	}
	e.data.Usage = mergeNativeUsage(e.data.Usage, entry)
	return nil
}

// mergeNativeUsage returns the total with entry added to the row for its
// provider and model. The result never shares its backing array with
// existing: an event already built from the earlier total must not change
// underneath it.
func mergeNativeUsage(existing []tracker.NativeUsage, entry tracker.NativeUsage) []tracker.NativeUsage {
	entry.Provider = strings.TrimSpace(entry.Provider)
	entry.Model = strings.TrimSpace(entry.Model)
	if entry.Provider == "" || entry.Model == "" {
		return existing
	}
	if entry.Input <= 0 && entry.CachedInput <= 0 && entry.Output <= 0 && entry.CostEstimate <= 0 && entry.ReportedCostMicros == nil {
		return existing
	}
	merged := slices.Clone(existing)
	for i := range merged {
		if merged[i].Provider != entry.Provider || merged[i].Model != entry.Model {
			continue
		}

		if merged[i].BillingMode != entry.BillingMode {
			merged[i].BillingMode = "unknown"
		}
		if merged[i].Currency != "" && entry.Currency != "" && merged[i].Currency != entry.Currency {
			return existing
		}
		if (merged[i].ReportedCostMicros != nil || entry.ReportedCostMicros != nil) && (merged[i].ReportedCostMicros == nil || entry.ReportedCostMicros == nil || entry.CostCoverage == "partial") {
			merged[i].CostCoverage = "partial"
		}
		if entry.ReportedCostMicros != nil {
			total := *entry.ReportedCostMicros
			if merged[i].ReportedCostMicros != nil {
				total += *merged[i].ReportedCostMicros
			}
			merged[i].ReportedCostMicros = &total
			if merged[i].CostSource == "" {
				merged[i].CostSource = entry.CostSource
			} else if merged[i].CostSource != entry.CostSource {
				merged[i].CostSource = "runner_report"
			}
		}
		merged[i].Input += entry.Input
		merged[i].CachedInput += entry.CachedInput
		merged[i].Output += entry.Output
		merged[i].CostEstimate += entry.CostEstimate
		if merged[i].Currency == "" {
			merged[i].Currency = entry.Currency
		}
		return merged
	}
	return append(merged, entry)
}
