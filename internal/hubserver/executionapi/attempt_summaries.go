package executionapi

import (
	"context"
)

type AttemptSummaryContextKey struct{}

func AttemptSummaryRead(ctx context.Context) bool {
	value, ok := ctx.Value(AttemptSummaryContextKey{}).(bool)
	return ok && value
}
