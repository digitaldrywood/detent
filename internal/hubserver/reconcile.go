package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/githubbinding"
)

type reconcileTarget struct {
	RepositoryTarget
	Profile           string
	RepositoryEnabled bool
	Cursor            *time.Time
	HydrationSince    *time.Time
	Hydrations        []HydrationRequest
}

func (d *database) reconcileTargets(ctx context.Context) ([]reconcileTarget, error) {
	return nil, ErrPostgresFeatureUnavailable
}

func validateReconcileSnapshot(snapshot ReconcileSnapshot) error {
	return githubbinding.ValidateSnapshot(snapshot)
}
