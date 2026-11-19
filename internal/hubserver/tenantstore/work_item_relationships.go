package tenantstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkItemRelationshipBatchReader interface {
	WorkItemRelationshipFacts(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) ([]WorkItemRelationshipFact, error)
}

type WorkItemBlockerBatchReader interface {
	ClaimOpenBlockerCounts(context.Context, ClaimBlockerQuery) (map[tracker.NativeWorkItemID]int, error)
}

type ClaimBlockerQuery struct {
	Project      tracker.ProjectID
	WorkItems    []tracker.NativeWorkItemID
	CredentialID string
	Now          time.Time
	SampleLimit  int32
}

type WorkItemRelationshipFact struct {
	WorkItemID            tracker.NativeWorkItemID
	Relationship          corestore.Relationship
	RelatedProjectVisible bool
}
