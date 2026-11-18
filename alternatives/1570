package billingstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConversationPrice struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	Provider       string
	Model          string
	EffectiveAt    time.Time
	Input          float64
	CachedInput    float64
	Output         float64
}

type ConversationUsage struct {
	OrganizationID  tracker.OrganizationID
	ProjectID       tracker.ProjectID
	ConversationID  string
	TurnID          string
	Provider        string
	Model           string
	OccurredAt      time.Time
	Input           int64
	CachedInput     int64
	Output          int64
	ReasoningOutput int64
	Outcome         string
	PriceID         *int64
	CostUSD         *float64
	CacheSavingsUSD *float64
}

type ConversationUsageSummary struct {
	Input           int64
	CachedInput     int64
	Output          int64
	ReasoningOutput int64
	Turns           int64
	UnpricedTurns   int64
	CostUSD         float64
}

type ConversationUsageBucket struct {
	Period          time.Time
	Provider        string
	Model           string
	Turns           int64
	Input           int64
	CachedInput     int64
	Output          int64
	CostUSD         float64
	CacheSavingsUSD float64
}
