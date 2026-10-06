package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/usagecost"
)

const costTimeLayout = "2006-01-02T15:04:05.000000000Z"
const spriteCostBucket = "sprite_infrastructure"

type costObservation = usagecost.Observation

type attributedCostObservation struct {
	ProjectID  string    `json:"project_id"`
	ReceivedAt time.Time `json:"received_at,omitzero"`
	costObservation
}

func normalizeCostObservation(o *costObservation, now time.Time) error {
	for _, value := range []string{o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, o.SourceID, o.Unit} {
		if strings.TrimSpace(value) != value || value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return nativeInvalid("Cost observations require bounded provider, account, resource, bucket, metric, source and unit identifiers")
		}
	}
	for _, value := range []string{o.RateSource, o.EvidenceSource, o.ResourceName} {
		if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
			return nativeInvalid("Cost evidence uses bounded identifiers, never credentials or invoice contents")
		}
	}
	o.From, o.To = o.From.UTC(), o.To.UTC()
	o.ObservedAt, o.FreshUntil = o.ObservedAt.UTC(), o.FreshUntil.UTC()
	o.RateEffectiveAt = o.RateEffectiveAt.UTC()
	if o.Revision <= 0 || o.From.IsZero() || !o.To.After(o.From) || o.To.Sub(o.From) > 366*24*time.Hour || o.To.After(o.ObservedAt) || o.ObservedAt.After(now) || o.FreshUntil.Before(o.ObservedAt) {
		return nativeInvalid("Cost observations require a positive revision, a past interval of at most 366 days, observation time and freshness deadline")
	}
	if len(o.Currency) != 3 || strings.Trim(o.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return nativeInvalid("Cost currency must be a three-letter uppercase code")
	}
	if !slices.Contains([]string{"complete", "partial", "unknown"}, o.Coverage) || !slices.Contains([]string{"estimated", "provider_billed", "runner_reported", "unknown"}, o.Basis) || !slices.Contains([]string{"measured", "provider_reported", "estimated", "unknown"}, o.QuantityBasis) {
		return nativeInvalid("Cost observations require explicit coverage, quantity basis and cost basis")
	}
	if o.EvidenceSource == "" || (o.Quantity == nil) != (o.QuantityBasis == "unknown") {
		return nativeInvalid("Cost observations require evidence and an explicit unknown basis for missing quantities")
	}
	if o.Quantity != nil && (math.IsNaN(*o.Quantity) || math.IsInf(*o.Quantity, 0) || *o.Quantity < 0 || *o.Quantity > 1e15) {
		return nativeInvalid("Usage quantities must be finite and nonnegative")
	}
	if o.UnitPriceMicros != nil {
		if *o.UnitPriceMicros < 0 || *o.UnitPriceMicros > 1e15 || o.RateSource == "" || o.RateEffectiveAt.IsZero() || o.RateEffectiveAt.After(o.From) {
			return nativeInvalid("A unit price requires a nonnegative rate, source and effective date no later than the interval")
		}
		if o.Basis == "estimated" && o.Quantity != nil {
			amount := math.Round(*o.Quantity * float64(*o.UnitPriceMicros))
			if math.IsInf(amount, 0) || amount > 1e15 {
				return nativeInvalid("Estimated cost exceeds the supported amount")
			}
			calculated := int64(amount)
			if o.AmountMicros != nil && *o.AmountMicros != calculated {
				return nativeInvalid("Estimated cost must match quantity multiplied by the recorded unit price")
			}
			o.AmountMicros = &calculated
		}
	}
	if o.AmountMicros != nil && (*o.AmountMicros > 1e15 || *o.AmountMicros < -1e15) {
		return nativeInvalid("Cost exceeds the supported amount")
	}
	runnerEstimate := o.Bucket == runnerCostBucket && o.AttemptID != "" && o.EstimatedAmountMicros != nil && o.EstimateSource != "" && o.AmountMicros != nil && *o.AmountMicros == *o.EstimatedAmountMicros
	if (o.AmountMicros == nil) != (o.Basis == "unknown") || (o.Basis == "estimated" && !runnerEstimate && (o.Quantity == nil || o.UnitPriceMicros == nil)) {
		return nativeInvalid("Estimated costs require quantity and rate; missing costs require an unknown basis")
	}
	if o.Basis == "provider_billed" && o.QuantityBasis != "provider_reported" && o.QuantityBasis != "unknown" {
		return nativeInvalid("Billed costs require provider evidence rather than locally estimated quantities")
	}

	if o.Basis == "runner_reported" && (o.Bucket != runnerCostBucket || o.AttemptID == "" || o.BillingMode != "metered" || o.ReportedAmountMicros == nil || o.AmountMicros == nil || *o.AmountMicros != *o.ReportedAmountMicros || (o.EvidenceSource != "runner_report" && o.EvidenceSource != "backend_result")) {
		return nativeInvalid("Runner-reported costs require explicit metered attempt evidence")
	}
	return nil
}

func recordCostObservation(ctx context.Context, tx *sql.Tx, organization, project string, o costObservation, now time.Time) (bool, error) {
	if err := normalizeCostObservation(&o, now); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(o)
	if err != nil {
		return false, fmt.Errorf("encode cost observation: %w", err)
	}
	var previous string
	var previousProject string
	var revision int64
	identity := []any{organization, o.Provider, o.ProviderAccount, o.SourceID, o.From.Format(costTimeLayout), o.To.Format(costTimeLayout)}
	err = tx.QueryRowContext(ctx, `SELECT project_id, revision, observation_json FROM usage_cost_observations
WHERE organization_id=? AND provider=? AND provider_account=? AND source_id=? AND period_start=? AND period_end=? ORDER BY revision DESC LIMIT 1`, identity...).Scan(&previousProject, &revision, &previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read cost observation: %w", err)
	}
	if err == nil {
		var old costObservation
		if err := json.Unmarshal([]byte(previous), &old); err != nil {
			return false, fmt.Errorf("decode cost observation: %w", err)
		}

		if old.AttemptID != "" {
			o.RunnerID, o.MachineID, o.Placement = old.RunnerID, old.MachineID, old.Placement
			encoded, err = json.Marshal(o)
			if err != nil {
				return false, err
			}
		}
		if previousProject != project || old.ResourceID != o.ResourceID || old.Bucket != o.Bucket || old.Metric != o.Metric || old.Unit != o.Unit || old.Currency != o.Currency || old.AttemptID != o.AttemptID || old.WorkItemID != o.WorkItemID || old.Model != o.Model || old.UsageKind != o.UsageKind {
			return false, nativeInvalid("Corrections preserve original attribution, metric, unit and currency")
		}
		if o.Revision < revision {
			return false, nil
		}
		if o.Revision == revision {
			if previous != string(encoded) {
				return false, nativeConflict(0)
			}
			return false, nil
		}
	}
	var overlaps int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_cost_observations u
WHERE u.organization_id=? AND u.provider=? AND u.provider_account=? AND u.resource_id=? AND u.bucket=? AND u.metric=?
AND COALESCE(json_extract(u.observation_json,'$.voided'),0)=0
AND u.period_start<? AND u.period_end>? AND NOT (u.source_id=? AND u.period_start=? AND u.period_end=?)
AND NOT EXISTS (SELECT 1 FROM usage_cost_observations newer WHERE newer.organization_id=u.organization_id AND newer.provider=u.provider AND newer.provider_account=u.provider_account AND newer.source_id=u.source_id AND newer.period_start=u.period_start AND newer.period_end=u.period_end AND newer.revision>u.revision)`,
		organization, o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, o.To.Format(costTimeLayout), o.From.Format(costTimeLayout), o.SourceID, o.From.Format(costTimeLayout), o.To.Format(costTimeLayout)).Scan(&overlaps); err != nil {
		return false, fmt.Errorf("check cost source coverage: %w", err)
	}
	if overlaps != 0 {
		return false, nativeInvalid("Overlapping usage must correct the original source period instead of adding another source")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_cost_observations
(organization_id,project_id,provider,provider_account,resource_id,bucket,metric,source_id,period_start,period_end,revision,observation_json,received_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, organization, project, o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, o.SourceID, o.From.Format(costTimeLayout), o.To.Format(costTimeLayout), o.Revision, string(encoded), now.UTC().Format(costTimeLayout))
	if err != nil {
		return false, fmt.Errorf("record cost observation: %w", err)
	}
	return true, nil
}

func costMonth(value string, now time.Time) (usageWindow, error) {
	if value == "" {
		value = now.UTC().Format("2006-01")
	}
	from, err := time.Parse("2006-01", value)
	if err != nil {
		return usageWindow{}, nativeInvalid("Month must be YYYY-MM in UTC")
	}
	return usageWindow{From: from, To: from.AddDate(0, 1, 0)}, nil
}

func readCostObservations(ctx context.Context, query nativeQueryer, organization string, projects []string, window usageWindow) ([]attributedCostObservation, error) {
	encoded, err := json.Marshal(projects)
	if err != nil {
		return nil, err
	}
	rows, err := query.QueryContext(ctx, `SELECT u.project_id,u.observation_json,u.received_at FROM usage_cost_observations u
WHERE u.organization_id=? AND u.period_start<? AND u.period_end>? AND (? OR u.project_id IN (SELECT value FROM json_each(?)))
AND COALESCE(json_extract(u.observation_json,'$.voided'),0)=0
AND NOT EXISTS (SELECT 1 FROM usage_cost_observations newer WHERE newer.organization_id=u.organization_id AND newer.provider=u.provider AND newer.provider_account=u.provider_account AND newer.source_id=u.source_id AND newer.period_start=u.period_start AND newer.period_end=u.period_end AND newer.revision>u.revision)
ORDER BY u.project_id,u.bucket,u.provider,u.provider_account,u.resource_id,u.metric,u.period_start,u.source_id`, organization, window.To.Format(costTimeLayout), window.From.Format(costTimeLayout), projects == nil, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("read monthly cost observations: %w", err)
	}
	defer rows.Close()
	result := []attributedCostObservation{}
	for rows.Next() {
		var entry attributedCostObservation
		var raw, received string
		if err := rows.Scan(&entry.ProjectID, &raw, &received); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &entry.costObservation); err != nil {
			return nil, fmt.Errorf("decode monthly cost observation: %w", err)
		}
		entry.ReceivedAt, err = time.Parse(costTimeLayout, received)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func allocateCostMicros(amount int64, from, to, start, end time.Time) int64 {
	duration := big.NewInt(to.Sub(from).Nanoseconds())
	boundary := func(at time.Time) *big.Int {
		product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(at.Sub(from).Nanoseconds()))
		return product.Quo(product, duration)
	}
	return new(big.Int).Sub(boundary(end), boundary(start)).Int64()
}
