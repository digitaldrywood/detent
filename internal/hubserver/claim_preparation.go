package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type claimWriterKey struct{}

const claimPreparationLimit = corestore.MaxRelationshipPreparationPages

func claimWriting(ctx context.Context) bool {
	writing, ok := ctx.Value(claimWriterKey{}).(bool)
	return ok && writing
}
