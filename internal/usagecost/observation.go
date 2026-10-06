package usagecost

import "time"

type Observation struct {
	Voided          bool      `json:"voided,omitempty"`
	Provider        string    `json:"provider"`
	ProviderAccount string    `json:"provider_account"`
	ResourceID      string    `json:"resource_id"`
	ResourceName    string    `json:"resource_name,omitempty"`
	Bucket          string    `json:"bucket"`
	Metric          string    `json:"metric"`
	SourceID        string    `json:"source_id"`
	Revision        int64     `json:"revision"`
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	Quantity        *float64  `json:"quantity"`
	Unit            string    `json:"unit"`
	QuantityBasis   string    `json:"quantity_basis"`
	AmountMicros    *int64    `json:"amount_micros"`
	Currency        string    `json:"currency"`
	Basis           string    `json:"basis"`
	UnitPriceMicros *int64    `json:"unit_price_micros"`
	RateSource      string    `json:"rate_source"`
	RateEffectiveAt time.Time `json:"rate_effective_at"`
	EvidenceSource  string    `json:"evidence_source"`
	ObservedAt      time.Time `json:"observed_at"`
	FreshUntil      time.Time `json:"fresh_until"`
	Coverage        string    `json:"coverage"`
}
