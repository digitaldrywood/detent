package hubserver

import (
	"context"
	"time"
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
