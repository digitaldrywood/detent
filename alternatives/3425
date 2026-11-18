package pgstoretest

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

type RepositoryTask struct {
	Done   chan struct{}
	Err    error
	Cancel context.CancelFunc
}

func StartRepositoryTask(t *testing.T, run func(context.Context) error) *RepositoryTask {
	t.Helper()
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	task := &RepositoryTask{Done: make(chan struct{}), Cancel: cancel}
	go func() { defer close(task.Done); task.Err = run(ctx) }()
	t.Cleanup(func() {
		cancel()
		cleanup := testenv.WaitContext(t, context.WithoutCancel(t.Context()))
		select {
		case <-task.Done:
		case <-cleanup.Done():
			t.Error("repository task did not exit during cleanup")
		}
	})
	return task
}

func (task *RepositoryTask) Wait(t *testing.T) error {
	t.Helper()
	ctx := testenv.WaitContext(t, t.Context())
	select {
	case <-task.Done:
		return task.Err
	case <-ctx.Done():
		t.Fatal("repository task did not finish")
		return ctx.Err()
	}
}

func WaitRepositorySignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	ctx := testenv.WaitContext(t, t.Context())
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("repository signal did not arrive")
	}
}
