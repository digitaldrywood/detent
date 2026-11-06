package tenantstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkItemRelationshipBatchReader interface {
	WorkItemRelationshipFacts(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) ([]WorkItemRelationshipFact, error)
}

type WorkItemRelationshipFact struct {
	WorkItemID            tracker.NativeWorkItemID
	Relationship          corestore.Relationship
	RelatedProjectVisible bool
}
