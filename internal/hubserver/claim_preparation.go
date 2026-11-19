package hubserver

import (
	"context"
)

type claimWriterKey struct{}

func claimWriting(ctx context.Context) bool {
	writing, ok := ctx.Value(claimWriterKey{}).(bool)
	return ok && writing
}
