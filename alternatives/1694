package eventstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReleaseDeliveryReader interface {
	ReleaseDelivery(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (ReleaseDelivery, error)
}

type ReleaseDeliveryWriter interface {
	ReleaseDeliveryReader
	PutReleaseDelivery(context.Context, ReleaseDelivery) error
}
