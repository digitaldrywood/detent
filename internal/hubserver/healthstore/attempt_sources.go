package healthstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttemptSources struct {
	Attempts map[string]leasestore.Attempt
	Metadata tenantstore.AttemptMetadata
}

type AttemptSourceReader interface {
	HealthAttemptSources(context.Context, tracker.ProjectID, []string) (AttemptSources, error)
}
