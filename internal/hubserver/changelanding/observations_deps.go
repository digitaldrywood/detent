package changelanding

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type ObservationsDeps struct {
	Repository        tenantstore.Repository
	RunDetailStoreFor func(context.Context) attachment.DetailStore
}

type Observations struct {
	deps ObservationsDeps
}

func NewObservations(deps ObservationsDeps) *Observations {
	return &Observations{deps: deps}
}
