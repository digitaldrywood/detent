package gobudget

import "context"

type contextKey struct{}

// WithContext carries the runner's existing host budget to direct validation
// commands, which execute without an agent turn or a repository-root go.mod.
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
