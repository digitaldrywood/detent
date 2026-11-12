package usagecosts

import (
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

const TimeLayout = "2006-01-02T15:04:05.000000000Z"
const SpriteBucket = "sprite_infrastructure"

type Observation = usagecost.Observation

type AttributedObservation struct {
	ProjectID  string    `json:"project_id"`
	ReceivedAt time.Time `json:"received_at,omitzero"`
	Observation
}

func Normalize(o *Observation, now time.Time) error {
	for _, value := range []string{o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, o.SourceID, o.Unit} {
		if strings.TrimSpace(value) != value || value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return hubapi.PublicInvalid("Cost observations require bounded provider, account, resource, bucket, metric, source and unit identifiers")
		}
	}
	for _, value := range []string{o.RateSource, o.EvidenceSource, o.ResourceName} {
		if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
			return hubapi.PublicInvalid("Cost evidence uses bounded identifiers, never credentials or invoice contents")
		}
	}
	o.From, o.To = o.From.UTC(), o.To.UTC()
	o.ObservedAt, o.FreshUntil = o.ObservedAt.UTC(), o.FreshUntil.UTC()
	o.RateEffectiveAt = o.RateEffectiveAt.UTC()
	if o.Revision <= 0 || o.From.IsZero() || !o.To.After(o.From) || o.To.Sub(o.From) > 366*24*time.Hour || o.To.After(o.ObservedAt) || o.ObservedAt.After(now) || o.FreshUntil.Before(o.ObservedAt) {
		return hubapi.PublicInvalid("Cost observations require a positive revision, a past interval of at most 366 days, observation time and freshness deadline")
	}
	if len(o.Currency) != 3 || strings.Trim(o.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return hubapi.PublicInvalid("Cost currency must be a three-letter uppercase code")
	}
	if !slices.Contains([]string{"complete", "partial", "unknown"}, o.Coverage) || !slices.Contains([]string{"estimated", "provider_billed", "runner_reported", "unknown"}, o.Basis) || !slices.Contains([]string{"measured", "provider_reported", "estimated", "unknown"}, o.QuantityBasis) {
		return hubapi.PublicInvalid("Cost observations require explicit coverage, quantity basis and cost basis")
	}
	if o.EvidenceSource == "" || (o.Quantity == nil) != (o.QuantityBasis == "unknown") {
		return hubapi.PublicInvalid("Cost observations require evidence and an explicit unknown basis for missing quantities")
	}
	if o.Quantity != nil && (math.IsNaN(*o.Quantity) || math.IsInf(*o.Quantity, 0) || *o.Quantity < 0 || *o.Quantity > 1e15) {
		return hubapi.PublicInvalid("Usage quantities must be finite and nonnegative")
	}
	if o.UnitPriceMicros != nil {
		if *o.UnitPriceMicros < 0 || *o.UnitPriceMicros > 1e15 || o.RateSource == "" || o.RateEffectiveAt.IsZero() || o.RateEffectiveAt.After(o.From) {
			return hubapi.PublicInvalid("A unit price requires a nonnegative rate, source and effective date no later than the interval")
		}
		if o.Basis == "estimated" && o.Quantity != nil {
			amount := math.Round(*o.Quantity * float64(*o.UnitPriceMicros))
			if math.IsInf(amount, 0) || amount > 1e15 {
				return hubapi.PublicInvalid("Estimated cost exceeds the supported amount")
			}
			calculated := int64(amount)
			if o.AmountMicros != nil && *o.AmountMicros != calculated {
				return hubapi.PublicInvalid("Estimated cost must match quantity multiplied by the recorded unit price")
			}
			o.AmountMicros = &calculated
		}
	}
	if o.AmountMicros != nil && (*o.AmountMicros > 1e15 || *o.AmountMicros < -1e15) {
		return hubapi.PublicInvalid("Cost exceeds the supported amount")
	}
	runnerEstimate := o.Bucket == RunnerBucket && o.AttemptID != "" && o.EstimatedAmountMicros != nil && o.EstimateSource != "" && o.AmountMicros != nil && *o.AmountMicros == *o.EstimatedAmountMicros
	if (o.AmountMicros == nil) != (o.Basis == "unknown") || (o.Basis == "estimated" && !runnerEstimate && (o.Quantity == nil || o.UnitPriceMicros == nil)) {
		return hubapi.PublicInvalid("Estimated costs require quantity and rate; missing costs require an unknown basis")
	}
	if o.Basis == "provider_billed" && o.QuantityBasis != "provider_reported" && o.QuantityBasis != "unknown" {
		return hubapi.PublicInvalid("Billed costs require provider evidence rather than locally estimated quantities")
	}

	if o.Basis == "runner_reported" && (o.Bucket != RunnerBucket || o.AttemptID == "" || o.BillingMode != "metered" || o.ReportedAmountMicros == nil || o.AmountMicros == nil || *o.AmountMicros != *o.ReportedAmountMicros || (o.EvidenceSource != "runner_report" && o.EvidenceSource != "backend_result")) {
		return hubapi.PublicInvalid("Runner-reported costs require explicit metered attempt evidence")
	}
	return nil
}

func Month(value string, now time.Time) (Window, error) {
	if value == "" {
		value = now.UTC().Format("2006-01")
	}
	from, err := time.Parse("2006-01", value)
	if err != nil {
		return Window{}, hubapi.PublicInvalid("Month must be YYYY-MM in UTC")
	}
	return Window{From: from, To: from.AddDate(0, 1, 0)}, nil
}

func allocateMicros(amount int64, from, to, start, end time.Time) int64 {
	duration := big.NewInt(to.Sub(from).Nanoseconds())
	boundary := func(at time.Time) *big.Int {
		product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(at.Sub(from).Nanoseconds()))
		return product.Quo(product, duration)
	}
	return new(big.Int).Sub(boundary(end), boundary(start)).Int64()
}
