package hubserver

import (
	"context"
)

type attemptSummaryContextKey struct{}

func attemptSummaryRead(ctx context.Context) bool {
	value, ok := ctx.Value(attemptSummaryContextKey{}).(bool)
	return ok && value
}
