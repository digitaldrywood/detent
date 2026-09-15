package runner

import (
	"context"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) SweepRetention(ctx context.Context, request workspace.RetentionRequest) (workspace.RetentionTotals, error) {
	sweeper, ok := r.workspace.(workspace.RetentionSweeper)
	if !ok {
		return workspace.RetentionTotals{}, nil
	}
	if reader, ok := r.store.(interface {
		ScratchRetentionState(context.Context, string) (bool, bool, error)
	}); ok {
		request.ScratchState = reader.ScratchRetentionState
	}
	return sweeper.SweepRetention(ctx, request)
}
