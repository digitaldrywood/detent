package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type claimWriterKey struct{}

const claimPreparationLimit = corestore.MaxRelationshipPreparationPages
const claimEvidenceLimit = 1000

type claimHydration struct {
	run func(context.Context, nativeQueryer) error
}

func (*claimHydration) Error() string { return "claim content requires preparation" }

func claimWriting(ctx context.Context) bool {
	writing, ok := ctx.Value(claimWriterKey{}).(bool)
	return ok && writing
}
