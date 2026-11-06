package eventstore

import (
	"context"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReleaseDeliveryFacts struct {
	DependsOnSource bool
	DependencyReady bool
	ConflictingPR   bool
}

type ReleaseDeliveryFactsReader interface {
	ReleaseDeliveryFacts(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, tracker.ReleasePickInput) (ReleaseDeliveryFacts, error)
	ReleaseSourceReviews(context.Context, string, string) ([]Evidence, error)
}
