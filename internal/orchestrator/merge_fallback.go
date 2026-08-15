package orchestrator

import (
	"context"
	"errors"
	"strings"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func (o *Orchestrator) handleMergeFallbackBudgetExceeded(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	if !errors.Is(event.Err, runpkg.ErrMergeFallbackBudgetExceeded) {
		return false
	}
	if o.completeLatestTerminalMergeWorkerResult(ctx, state, event, running) {
		return true
	}
	findings := strings.TrimSpace(event.Result.MergeFallbackFindings)
	if findings == "" {
		findings = strings.TrimSpace(event.Result.Output)
	}
	o.reworkMergeWorkerResult(
		ctx,
		state,
		event,
		running,
		running.Issue,
		mergeFallbackBudgetExceededReason,
		nil,
		findings,
	)
	return true
}
