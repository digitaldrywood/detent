package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerCostBucket = "runner_api"
const lunaCostBucket = "luna_api"

func validateRunnerCostUsage(entry tracker.NativeUsage) error {
	if !finiteUsageCost(entry.CostEstimate) {
		return nativePublicInvalid("Usage cost must be finite and bounded")
	}
	if entry.BillingMode != "" && entry.BillingMode != "unknown" && entry.BillingMode != "metered" && entry.BillingMode != "subscription" {
		return nativePublicInvalid("Usage billing mode must be metered, subscription or unknown")
	}
	if entry.UsageKind != "" && entry.UsageKind != "cumulative" && entry.UsageKind != "incremental" {
		return nativePublicInvalid("Usage kind must be cumulative or incremental")
	}
	if entry.UsageKind == "incremental" && (entry.SourceID == "" || entry.Revision <= 0 || entry.From.IsZero() || entry.To.IsZero()) {
		return nativePublicInvalid("Incremental usage requires a source, revision and execution interval")
	}
	if entry.UsageKind != "incremental" && entry.SourceID != "" {
		return nativePublicInvalid("Cumulative usage uses the attempt's canonical source")
	}
	if len(entry.SourceID) > 64 || strings.ContainsAny(entry.SourceID, "\x00\r\n") || entry.Revision < 0 {
		return nativePublicInvalid("Usage requires a bounded source and nonnegative revision")
	}
	if entry.CostCoverage != "" && entry.CostCoverage != "complete" && entry.CostCoverage != "partial" {
		return nativePublicInvalid("Reported cost coverage must be complete or partial")
	}
	if entry.ReportedCostMicros != nil {
		if *entry.ReportedCostMicros < 0 || *entry.ReportedCostMicros > 1e15 || (entry.CostSource != "backend_result" && entry.CostSource != "runner_report") {
			return nativePublicInvalid("Reported cost requires a nonnegative bounded amount and explicit runner provenance")
		}
		if len(entry.Currency) != 3 || strings.Trim(entry.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return nativePublicInvalid("Reported cost requires a three-letter uppercase currency")
		}
	} else if entry.CostSource != "" || entry.CostCoverage != "" {
		return nativePublicInvalid("Cost provenance requires a reported amount")
	}
	return nil
}

func finiteUsageCost(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1e9
}

func recordRunnerUsageCosts(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.NativeRunEvent, prices UsageConfig, now time.Time) error {
	if request.Type != "run.finished" || len(request.Data.Usage) == 0 {
		return nil
	}
	var organization, project, item, started, ended, runnerID, machineID, placement string
	var legacy sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT a.organization_id,a.project_id,a.work_item_id,a.started_at,a.updated_at,coalesce(lr.runner_id,''),coalesce(a.machine_id,''),CASE WHEN a.usage_facts_ready IS NULL THEN COALESCE(NULLIF(a.payload_object_key,''),a.data_json) END FROM native_attempts a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.id=?`, request.Data.AttemptID).Scan(&organization, &project, &item, &started, &ended, &runnerID, &machineID, &legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read usage attribution: %w", err)
	}
	if legacy.Valid {
		var data tracker.NativeRunData
		if err := decodeRunDetail(ctx, tx, legacy.String, &data); err != nil {
			return err
		}
		machineID = string(data.MachineID)
	}
	facts, factErr := readMachineSpriteFacts(ctx, tx, tracker.OrganizationID(organization), tracker.MachineID(machineID))
	placement = "unknown"
	if factErr == nil {
		placement = "local"
		if facts.Name != nil {
			placement = "sprite"
		}
	} else if !errors.Is(factErr, sql.ErrNoRows) {
		return factErr
	}
	if organization != string(scope.organization) || project != string(scope.project) {
		return nativePublicInvalid("Usage preserves the attempt's original organization and project")
	}
	from, err := parseTimeValue(started)
	if err != nil {
		return err
	}
	for _, entry := range request.Data.Usage {
		if err := validateRunnerCostUsage(entry); err != nil {
			return err
		}
		start, end := entry.From, entry.To
		if start.IsZero() {
			start = from
		}
		if end.IsZero() {
			end = request.OccurredAt
			if end.IsZero() {
				end, err = parseTimeValue(ended)
				if err != nil {
					return err
				}
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
			currency = prices.currency()
		}
		source := request.Data.AttemptID + ":" + model
		if kind == "incremental" {
			source += ":" + entry.SourceID
		}
		quantity := float64(entry.Input) + float64(entry.Output)
		o := costObservation{ReportedCostSource: entry.CostSource, RunnerID: runnerID, MachineID: machineID, Placement: placement, Provider: provider, ProviderAccount: "runner", ResourceID: request.Data.AttemptID, AttemptID: request.Data.AttemptID, WorkItemID: item, Model: model, BillingMode: mode, UsageKind: kind, Input: entry.Input, CachedInput: entry.CachedInput, Output: entry.Output, Bucket: runnerCostBucket, Metric: model, SourceID: source, Revision: revision, From: start, To: end, Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", Currency: currency, Basis: "unknown", EvidenceSource: "runner_finish", ObservedAt: observed, FreshUntil: observed, Coverage: "complete", ReportedAmountMicros: entry.ReportedCostMicros}
		if entry.CostEstimate > 0 {
			amount := int64(math.Round(entry.CostEstimate * 1e6))
			o.EstimatedAmountMicros, o.EstimateSource = &amount, "runner_token_pricing"
		} else if estimated, estimateCurrency, found := prices.estimate(model, entry.Input, entry.CachedInput, entry.Output); found && estimateCurrency == currency && finiteUsageCost(estimated) {
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
		var previous costObservation
		if kind == "incremental" {
			var raw string
			err := tx.QueryRowContext(ctx, `SELECT observation_json FROM usage_cost_observations WHERE organization_id=? AND provider=? AND provider_account=? AND source_id=? AND period_start=? AND period_end=? ORDER BY revision DESC LIMIT 1`, organization, o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costTimeLayout), o.To.UTC().Format(costTimeLayout)).Scan(&raw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if err := decodeStorageJSON(ctx, []byte(raw), &previous); err != nil {
					return err
				}
			}
		}
		recorded, err := recordCostObservation(ctx, tx, organization, project, o, now)
		if err != nil {
			return err
		}
		if recorded && kind == "incremental" {
			var cost, oldCost float64
			if o.EstimatedAmountMicros != nil {
				cost = float64(*o.EstimatedAmountMicros) / 1e6
			}
			if previous.EstimatedAmountMicros != nil {
				oldCost = float64(*previous.EstimatedAmountMicros) / 1e6
			}
			delta := tracker.NativeUsage{Input: o.Input - previous.Input, CachedInput: o.CachedInput - previous.CachedInput, Output: o.Output - previous.Output, CostEstimate: cost - oldCost}
			if err := updateAttemptUsageSummary(ctx, tx, scope, request.Data.AttemptID, delta); err != nil {
				return err
			}
		}
	}
	return nil
}

func nativeUsageCorrection(previous, next tracker.NativeRunData) bool {
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
