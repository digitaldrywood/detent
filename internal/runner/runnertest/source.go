package runnertest

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type RecoveredSource struct {
	Source workspace.ChangeSource
	Err    error
}

func (RecoveredSource) SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
}
func (s RecoveredSource) RecoverChangeSource(context.Context) (workspace.ChangeSource, error) {
	return s.Source, s.Err
}
