package reportstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type SummaryUsageReader interface {
	ReadSummaryUsage(context.Context, tracker.ProjectID, string, time.Time, time.Time, []tracker.NativeWorkItemID) ([]UsageFact, error)
}
