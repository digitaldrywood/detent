package workspace

import (
	"context"
	"sync"
)

// Usage serializes a worker's workspace lifetime with background removal.
// Workers wait for a removal already in progress; cleanup never waits for workers.
type Usage interface {
	Use(context.Context, Issue) (func(), error)
}

type workspaceUses struct {
	mu    sync.Mutex
	paths map[string]chan struct{}
}

func (u *workspaceUses) permit(key string) chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.paths == nil {
		u.paths = make(map[string]chan struct{})
	}
	permit := u.paths[key]
	if permit == nil {
		permit = make(chan struct{}, 1)
		u.paths[key] = permit
	}
	return permit
}

func (u *workspaceUses) use(ctx context.Context, key string) (func(), error) {
	permit := u.permit(key)
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (u *workspaceUses) cleanup(key string) (func(), bool) {
	permit := u.permit(key)
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, true
	default:
		return nil, false
	}
}

func (l *LocalGit) Use(ctx context.Context, issue Issue) (func(), error) {
	info, err := l.infoForIssue(issue)
	if err != nil {
		return nil, err
	}
	return l.uses.use(ctx, info.Path)
}

func (f *Filesystem) Use(ctx context.Context, issue Issue) (func(), error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return nil, err
	}
	return f.uses.use(ctx, info.Path)
}
