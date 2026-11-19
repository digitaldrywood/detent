package leasestore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RecordedRecovery struct {
	Source                  *tracker.NativeChangeReference
	ExpectedRevision        int64
	ProjectID               tracker.ProjectID
	WorkItemID              tracker.NativeWorkItemID
	Attempt                 Attempt
	ReportEventID           string
	ReportSequence          int64
	ReportRevision          int64
	WorkflowEventID         string
	WorkflowSequence        int64
	WorkflowObjectKey       string
	DependencyNumbers       []int64
	MergedDependencyNumbers []int64
	Policy                  policystore.Revision
	AttemptPolicyID         string
	State                   string
	Now                     time.Time
}

type RecordedRecoveryWriter interface {
	AuthorizeRecordedRecovery(context.Context, RecordedRecovery) error
}
