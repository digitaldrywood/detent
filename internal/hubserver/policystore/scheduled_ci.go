package policystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScheduledReader interface {
	ReadScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure) (progresscheck.CIDecision, error)
	ScheduledLandedWorkItems(context.Context, tracker.ProjectID, string) ([]string, error)
}

type ScheduledWriter interface {
	ScheduledReader
	StoreScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure, progresscheck.CIDecision, time.Time) error
}
