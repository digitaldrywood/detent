package workspace

import "context"

type ProcessSource func(context.Context, string) ([]int, error)

type processSourceKey struct{}

func WithProcessSource(ctx context.Context, source ProcessSource) context.Context {
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, processSourceKey{}, source)
}
