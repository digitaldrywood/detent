package gobudget

import "context"

type contextKey struct{}

func WithContext(ctx context.Context, budget Budget) context.Context {
	return context.WithValue(ctx, contextKey{}, budget)
}

func FromContext(ctx context.Context) Budget {
	budget, ok := ctx.Value(contextKey{}).(Budget)
	if !ok {
		return Budget{}
	}
	return budget
}
