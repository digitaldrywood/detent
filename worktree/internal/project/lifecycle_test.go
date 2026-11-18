package project_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/project"
)

// projectTestWaitTimeout bounds real lifecycle work, not expected latency.
// Full-suite contention can delay dispatch and cleanup beyond one second.
// Match the workflow-watcher tests and return as soon as the event arrives.
const projectTestWaitTimeout = 10 * time.Second

func stopTestProjects(t *testing.T, projects ...*project.Project) {
	t.Helper()

	for _, item := range projects {
		// Cleanup needs a fresh context even when the test canceled its run.
		ctx, cancel := context.WithTimeout(context.Background(), projectTestWaitTimeout)
		err := item.Stop(ctx)
		cancel()
		if err != nil && !errors.Is(err, project.ErrNotRunning) {
			t.Errorf("Stop(%s) error = %v", item.ID(), err)
			continue
		}
		if item.Running() {
			t.Errorf("project %s still running after Stop", item.ID())
		}
	}
}
