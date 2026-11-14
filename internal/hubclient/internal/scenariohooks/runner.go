package scenariohooks

import (
	"context"

	"github.com/digitaldrywood/detent/internal/runner"
)

type StartedRunner struct {
	Started chan runner.RunRequest
}

func (r StartedRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	r.Started <- request
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}
