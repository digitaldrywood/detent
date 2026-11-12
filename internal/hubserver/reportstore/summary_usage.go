package reportstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type SummaryUsageReader interface {
	ReadSummaryUsage(context.Context, tracker.ProjectID, string, time.Time, time.Time, []tracker.NativeWorkItemID) ([]UsageFact, error)
}

type CompletionCostItem struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
}

type CompletionCostQuery struct {
	Items     []CompletionCostItem
	Principal string
	CurrentTo time.Time
	PriorTo   time.Time
}

type CompletionCostFact struct {
	CompletionCostItem
	Cost      float64
	PriorCost float64
	Complete  bool
}

type CompletionCostReader interface {
	ReadCompletionCosts(context.Context, CompletionCostQuery) ([]CompletionCostFact, error)
}
