package usagecosts

import (
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const RunnerBucket = "runner_api"
const LunaBucket = "luna_api"

func ValidateRunnerUsage(entry tracker.NativeUsage) error {
	if !finiteCost(entry.CostEstimate) {
		return hubapi.PublicInvalid("Usage cost must be finite and bounded")
	}
	if entry.BillingMode != "" && entry.BillingMode != "unknown" && entry.BillingMode != "metered" && entry.BillingMode != "subscription" {
		return hubapi.PublicInvalid("Usage billing mode must be metered, subscription or unknown")
	}
	if entry.UsageKind != "" && entry.UsageKind != "cumulative" && entry.UsageKind != "incremental" {
		return hubapi.PublicInvalid("Usage kind must be cumulative or incremental")
	}
	if entry.UsageKind == "incremental" && (entry.SourceID == "" || entry.Revision <= 0 || entry.From.IsZero() || entry.To.IsZero()) {
		return hubapi.PublicInvalid("Incremental usage requires a source, revision and execution interval")
	}
	if entry.UsageKind != "incremental" && entry.SourceID != "" {
		return hubapi.PublicInvalid("Cumulative usage uses the attempt's canonical source")
	}
	if len(entry.SourceID) > 64 || strings.ContainsAny(entry.SourceID, "\x00\r\n") || entry.Revision < 0 {
		return hubapi.PublicInvalid("Usage requires a bounded source and nonnegative revision")
	}
	if entry.CostCoverage != "" && entry.CostCoverage != "complete" && entry.CostCoverage != "partial" {
		return hubapi.PublicInvalid("Reported cost coverage must be complete or partial")
	}
	if entry.ReportedCostMicros != nil {
		if *entry.ReportedCostMicros < 0 || *entry.ReportedCostMicros > 1e15 || (entry.CostSource != "backend_result" && entry.CostSource != "runner_report") {
			return hubapi.PublicInvalid("Reported cost requires a nonnegative bounded amount and explicit runner provenance")
		}
		if len(entry.Currency) != 3 || strings.Trim(entry.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return hubapi.PublicInvalid("Reported cost requires a three-letter uppercase currency")
		}
	} else if entry.CostSource != "" || entry.CostCoverage != "" {
		return hubapi.PublicInvalid("Cost provenance requires a reported amount")
	}
	return nil
}

func finiteCost(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1e9
}

type RunnerObservation struct {
	Kind        string
	Observation Observation
}

type RunnerFacts struct {
	WorkItemID string
	RunnerID   string
	MachineID  string
	Placement  string
	StartedAt  time.Time
	EndedAt    time.Time
}

func BuildRunnerObservations(request tracker.NativeRunEvent, prices Prices, facts RunnerFacts) ([]RunnerObservation, error) {
	var observations []RunnerObservation
	for _, entry := range request.Data.Usage {
		if err := ValidateRunnerUsage(entry); err != nil {
			return nil, err
		}
		start, end := entry.From, entry.To
		if start.IsZero() {
			start = facts.StartedAt
		}
		if end.IsZero() {
			end = request.OccurredAt
			if end.IsZero() {
				end = facts.EndedAt
			}
		}
		observed := entry.ReportedAt
		if observed.IsZero() {
			observed = end
		}
		revision := entry.Revision
		if revision == 0 {
			revision = max(request.Data.Sequence, 1)
		}
		mode := entry.BillingMode
		if mode == "" {
			mode = "unknown"
		}
		kind := entry.UsageKind
		if kind == "" {
			kind = "cumulative"
		}
		provider, model := tracker.UsageProviderID(entry.Provider), strings.TrimSpace(entry.Model)
		currency := entry.Currency
		if currency == "" {
			currency = prices.CurrencyValue()
		}
		source := request.Data.AttemptID + ":" + model
		if kind == "incremental" {
			source += ":" + entry.SourceID
		}
		quantity := float64(entry.Input) + float64(entry.Output)
		o := Observation{ReportedCostSource: entry.CostSource, RunnerID: facts.RunnerID, MachineID: facts.MachineID, Placement: facts.Placement, Provider: provider, ProviderAccount: "runner", ResourceID: request.Data.AttemptID, AttemptID: request.Data.AttemptID, WorkItemID: facts.WorkItemID, Model: model, BillingMode: mode, UsageKind: kind, Input: entry.Input, CachedInput: entry.CachedInput, Output: entry.Output, Bucket: RunnerBucket, Metric: model, SourceID: source, Revision: revision, From: start, To: end, Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", Currency: currency, Basis: "unknown", EvidenceSource: "runner_finish", ObservedAt: observed, FreshUntil: observed, Coverage: "complete", ReportedAmountMicros: entry.ReportedCostMicros}
		if entry.CostEstimate > 0 {
			amount := int64(math.Round(entry.CostEstimate * 1e6))
			o.EstimatedAmountMicros, o.EstimateSource = &amount, "runner_token_pricing"
		} else if estimated, estimateCurrency, found := prices.Estimate(model, entry.Input, entry.CachedInput, entry.Output); found && estimateCurrency == currency && finiteCost(estimated) {
			amount := int64(math.Round(estimated * 1e6))
			o.EstimatedAmountMicros, o.EstimateSource = &amount, "hub_token_pricing"
		}
		if entry.CostCoverage == "partial" {
			o.Coverage = "partial"
		}
		if mode == "metered" {
			if o.ReportedAmountMicros != nil {
				o.AmountMicros, o.Basis, o.EvidenceSource = o.ReportedAmountMicros, "runner_reported", entry.CostSource
			} else if o.EstimatedAmountMicros != nil {
				o.AmountMicros, o.Basis = o.EstimatedAmountMicros, "estimated"
			}
		}
		observations = append(observations, RunnerObservation{Kind: kind, Observation: o})
	}
	return observations, nil
}

func UsageCorrection(previous, next tracker.NativeRunData) bool {
	if len(next.Usage) == 0 {
		return false
	}
	previous.Usage, next.Usage = nil, nil
	previous.Sequence, next.Sequence = 0, 0
	if previous.Runtime != nil && next.Runtime != nil {
		previousRuntime, nextRuntime := *previous.Runtime, *next.Runtime
		if len(previousRuntime.Pipeline) == 0 {
			previousRuntime.Pipeline = nil
		}
		if len(nextRuntime.Pipeline) == 0 {
			nextRuntime.Pipeline = nil
		}
		previous.Runtime, next.Runtime = &previousRuntime, &nextRuntime
	}
	return reflect.DeepEqual(previous, next)
}
