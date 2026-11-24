package hubapi

import "context"

type requestParentKey struct{}

func WithRequestParent(ctx, parent context.Context) context.Context {
	return context.WithValue(ctx, requestParentKey{}, parent)
}

func ConnectionContext(ctx context.Context) context.Context {
	if parent, ok := ctx.Value(requestParentKey{}).(context.Context); ok {
		return parent
	}
	return ctx
}
