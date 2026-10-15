package usagecost

import "time"

type Observation struct {
	ReportedCostSource    string    `json:"reported_cost_source,omitempty"`
	RunnerID              string    `json:"runner_id,omitempty"`
	MachineID             string    `json:"machine_id,omitempty"`
	Placement             string    `json:"placement,omitempty"`
	AttemptID             string    `json:"attempt_id,omitempty"`
	WorkItemID            string    `json:"work_item_id,omitempty"`
	Model                 string    `json:"model,omitempty"`
	BillingMode           string    `json:"billing_mode,omitempty"`
	UsageKind             string    `json:"usage_kind,omitempty"`
	Input                 int64     `json:"input,omitempty"`
	CachedInput           int64     `json:"cached_input,omitempty"`
	Output                int64     `json:"output,omitempty"`
	EstimatedAmountMicros *int64    `json:"estimated_amount_micros,omitempty"`
	EstimateSource        string    `json:"estimate_source,omitempty"`
	ReportedAmountMicros  *int64    `json:"reported_amount_micros,omitempty"`
	Voided                bool      `json:"voided,omitempty"`
	Provider              string    `json:"provider"`
	ProviderAccount       string    `json:"provider_account"`
	ResourceID            string    `json:"resource_id"`
	ResourceName          string    `json:"resource_name,omitempty"`
	Bucket                string    `json:"bucket"`
	Metric                string    `json:"metric"`
	SourceID              string    `json:"source_id"`
	Revision              int64     `json:"revision"`
	From                  time.Time `json:"from"`
	To                    time.Time `json:"to"`
	Quantity              *float64  `json:"quantity"`
	Unit                  string    `json:"unit"`
	QuantityBasis         string    `json:"quantity_basis"`
	AmountMicros          *int64    `json:"amount_micros"`
	Currency              string    `json:"currency"`
	Basis                 string    `json:"basis"`
	UnitPriceMicros       *int64    `json:"unit_price_micros"`
	RateSource            string    `json:"rate_source"`
	RateEffectiveAt       time.Time `json:"rate_effective_at"`
	EvidenceSource        string    `json:"evidence_source"`
	ObservedAt            time.Time `json:"observed_at"`
	FreshUntil            time.Time `json:"fresh_until"`
	Coverage              string    `json:"coverage"`
}
