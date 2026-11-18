package workspace

import (
	"context"

	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
)

// Usage serializes a worker's workspace lifetime with background removal.
// Workers wait for a removal already in progress; cleanup never waits for workers.
type Usage interface {
	Use(context.Context, Issue) (func(), error)
}

type workspaceUses = hookproc.Registry
type HookExitUnverifiedError = hookproc.HookExitUnverifiedError

func WithHookProcesses(ctx context.Context) (context.Context, func() error) {
	return hookproc.WithHookProcesses(ctx)
}

func (l *LocalGit) Use(ctx context.Context, issue Issue) (func(), error) {
	info, err := l.infoForIssue(issue)
	if err != nil {
		return nil, err
	}
	return l.uses.Use(ctx, info.Path)
}

func (f *Filesystem) Use(ctx context.Context, issue Issue) (func(), error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return nil, err
	}
	return f.uses.Use(ctx, info.Path)
}
