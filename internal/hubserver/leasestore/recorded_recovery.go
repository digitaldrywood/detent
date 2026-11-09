package leasestore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type RecordedRecovery struct {
	ProjectID         tracker.ProjectID
	WorkItemID        tracker.NativeWorkItemID
	Attempt           Attempt
	ReportEventID     string
	ReportSequence    int64
	ReportRevision    int64
	WorkflowEventID   string
	WorkflowSequence  int64
	WorkflowObjectKey string
	DependencyNumbers []int64
	Now               time.Time
}

type RecordedRecoveryWriter interface {
	AuthorizeRecordedRecovery(context.Context, RecordedRecovery) error
}
