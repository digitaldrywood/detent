package cli

import (
	"context"
	"strings"
	"sync"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type doctorWorkflowCache struct {
	mu      sync.Mutex
	entries map[string]*doctorWorkflowCacheEntry
}

type doctorWorkflowCacheEntry struct {
	ready    chan struct{}
	workflow workflowconfig.Workflow
	err      error
}

func newDoctorWorkflowCache() *doctorWorkflowCache {
	return &doctorWorkflowCache{entries: map[string]*doctorWorkflowCacheEntry{}}
}

func (c *doctorWorkflowCache) load(
	ctx context.Context,
	project globalconfig.Project,
	deps doctorDeps,
) (workflowconfig.Workflow, error) {
	key := strings.Join([]string{
		strings.TrimSpace(project.ID),
		strings.TrimSpace(project.Workflow),
		strings.TrimSpace(project.WorkflowRef),
		strings.TrimSpace(project.Workdir),
	}, "\x00")

	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok {
		c.mu.Unlock()
		select {
		case <-entry.ready:
			return entry.workflow, entry.err
		case <-ctx.Done():
			return workflowconfig.Workflow{}, ctx.Err()
		}
	}
	entry = &doctorWorkflowCacheEntry{ready: make(chan struct{})}
	c.entries[key] = entry
	c.mu.Unlock()

	entry.workflow, entry.err = loadDoctorProjectWorkflowUncached(ctx, project, deps)
	close(entry.ready)
	return entry.workflow, entry.err
}
