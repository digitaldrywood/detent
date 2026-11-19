package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (e hubProjectExecutor) runnerProjectDiagnostics(ctx context.Context, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	page, err := e.service.readRunnerProjectDiagnostics(ctx, scope, args)
	if err != nil {
		return operatortool.Result{}, err
	}
	return hubProjectResult(page)
}
