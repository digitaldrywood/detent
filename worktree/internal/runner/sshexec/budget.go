package sshexec

import (
	"context"

	"github.com/digitaldrywood/detent/internal/budget"
)

type BudgetGuard struct{ Peer Caller }

func (b BudgetGuard) CheckDispatch(ctx context.Context, request budget.DispatchRequest) (budget.Decision, error) {
	var result budget.Decision
	err := b.Peer.Call(ctx, "budget.check", &result, request)
	return result, err
}
func (b BudgetGuard) EstimateDispatch(ctx context.Context, project string) (budget.TokenEstimate, error) {
	var result budget.TokenEstimate
	err := b.Peer.Call(ctx, "budget.estimate", &result, project)
	return result, err
}
