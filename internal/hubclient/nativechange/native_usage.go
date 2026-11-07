package nativechange

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// RecordUsage adds one turn's usage to the attempt's running total. A report
// that names no provider or model, or that spent nothing, changes nothing.
func (e *nativeExecution) RecordUsage(ctx context.Context, entry tracker.NativeUsage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if entry.SourceID != "" && e.scheduler != nil {
		entry.To, entry.ReportedAt = e.scheduler.now().UTC(), e.scheduler.now().UTC()
	}
	for _, existing := range e.data.Usage {
		if existing.Provider == strings.TrimSpace(entry.Provider) && existing.Model == strings.TrimSpace(entry.Model) && existing.Currency != "" && entry.Currency != "" && existing.Currency != entry.Currency {
			return errors.New("attempt usage cannot combine currencies for the same provider and model")
		}
	}
	e.data.Usage = mergeNativeUsage(e.data.Usage, entry)
	observed := slices.Clone(e.data.Usage)
	if e.scheduler != nil {
		if err := e.flush(ctx); err != nil {
			return err
		}
		e.data.Usage = observed
	}

	if e.data.Identity != nil && e.data.Outcome == "" {
		if err := e.append(ctx, "run.observed", "", nil); err != nil {
			return err
		}
	}
	return nil
}

func (e *nativeExecution) RecordProcessUsage(usage procgroup.Usage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if previous := e.data.ProcessUsage; previous != nil {
		usage.PeakMemoryBytes = max(usage.PeakMemoryBytes, previous.PeakMemoryBytes)
		usage.UserCPUSeconds += previous.UserCPUSeconds
		usage.SystemCPUSeconds += previous.SystemCPUSeconds
		usage.WallSeconds += previous.WallSeconds
	}
	e.data.ProcessUsage = &usage
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

		if merged[i].SourceID != entry.SourceID {
			continue
		}
		if entry.SourceID != "" {
			entry.Input = max(entry.Input, merged[i].Input)
			entry.CachedInput = max(entry.CachedInput, merged[i].CachedInput)
			entry.Output = max(entry.Output, merged[i].Output)
			entry.CostEstimate = max(entry.CostEstimate, merged[i].CostEstimate)
			entry.Revision = max(entry.Revision, merged[i].Revision)
			if !merged[i].From.IsZero() {
				entry.From = merged[i].From
			}
			if entry.To.Before(merged[i].To) {
				entry.To = merged[i].To
			}
			if merged[i].ReportedCostMicros != nil && (entry.ReportedCostMicros == nil || *entry.ReportedCostMicros < *merged[i].ReportedCostMicros) {
				value := *merged[i].ReportedCostMicros
				entry.ReportedCostMicros = &value
			}
			merged[i] = entry
			return merged
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

func (e *nativeExecution) flushReasonUsage(ctx context.Context, before []tracker.NativeUsage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if reflect.DeepEqual(before, e.data.Usage) {
		return nil
	}
	data := e.data
	data.Sequence++
	e.pending = &tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10)}, Type: "run.finished", SchemaVersion: 1, Data: data}
	return e.flush(ctx)
}
