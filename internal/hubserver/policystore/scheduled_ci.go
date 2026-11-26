package policystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScheduledReader interface {
	ReadScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure, string, time.Time) (progresscheck.CIDecision, bool, error)
	ScheduledLandedWorkItems(context.Context, tracker.ProjectID, string) ([]string, error)
}

type ScheduledWriter interface {
	ScheduledReader
	ClaimScheduledClassification(context.Context, tracker.ProjectID, string, string, time.Time, time.Time) (bool, error)
	LockScheduledClassification(context.Context, tracker.ProjectID, string, string, time.Time) (bool, error)
	ReleaseScheduledClassification(context.Context, tracker.ProjectID, string, string, *time.Time) (bool, error)
	StoreScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure, progresscheck.CIDecision, time.Time, int64) (billingstore.UsageResult, error)
	UpdateScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure, progresscheck.CIDecision, int64) (billingstore.UsageResult, error)
}
