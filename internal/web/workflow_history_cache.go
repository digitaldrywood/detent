package web

import (
	"context"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const workflowHistoryFreshness = 30 * time.Second
const maxWorkflowHistoryScopes = 16

type workflowHistoryEntry struct {
	metrics    telemetry.WorkflowMetrics
	cycleTime  telemetry.CycleTimeReport
	revision   int64
	observedAt time.Time
	loadedAt   time.Time
}

type workflowHistoryCache struct {
	mu      sync.Mutex
	entries map[string]workflowHistoryEntry
	loading chan struct{}
}

func (c *workflowHistoryCache) get(ctx context.Context, backend store.Store, projectID string, observedAt time.Time, clock func() time.Time, load func(context.Context, string, time.Time) workflowHistoryEntry) workflowHistoryEntry {
	if clock == nil {
		clock = time.Now
	}
	for {
		revision, err := workflowHistoryRevision(ctx, backend)
		if err != nil {
			return workflowHistoryEntry{metrics: telemetry.WorkflowMetrics{DegradedReason: "workflow history revision query failed"}, cycleTime: telemetry.CycleTimeReport{DegradedReason: "workflow history revision query failed"}}
		}
		now := clock()
		c.mu.Lock()
		entry, ok := c.entries[projectID]
		if ok && entry.revision == revision && workflowHistoryFresh(entry.loadedAt, now) && workflowHistoryFresh(entry.observedAt, observedAt) {
			c.mu.Unlock()
			return entry
		}
		if loading := c.loading; loading != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return workflowHistoryEntry{metrics: telemetry.WorkflowMetrics{DegradedReason: "workflow history query canceled"}, cycleTime: telemetry.CycleTimeReport{DegradedReason: "workflow history query canceled"}}
			case <-loading:
				continue
			}
		}
		c.loading = make(chan struct{})
		c.mu.Unlock()

		entry = load(ctx, projectID, observedAt)
		finalRevision, revisionErr := workflowHistoryRevision(ctx, backend)
		c.mu.Lock()
		if entry.metrics.Available && (projectID != "" || entry.cycleTime.Available) && ctx.Err() == nil && revisionErr == nil && revision == finalRevision {
			if c.entries == nil {
				c.entries = make(map[string]workflowHistoryEntry)
			}
			if len(c.entries) >= maxWorkflowHistoryScopes {
				var oldestProject string
				var oldest time.Time
				for project, candidate := range c.entries {
					if oldest.IsZero() || candidate.loadedAt.Before(oldest) {
						oldestProject, oldest = project, candidate.loadedAt
					}
				}
				delete(c.entries, oldestProject)
			}
			entry.revision, entry.observedAt, entry.loadedAt = revision, observedAt, now
			c.entries[projectID] = entry
		}
		close(c.loading)
		c.loading = nil
		c.mu.Unlock()
		return entry
	}
}

func workflowHistoryFresh(loadedAt, now time.Time) bool {
	elapsed := now.Sub(loadedAt)
	return elapsed >= 0 && elapsed < workflowHistoryFreshness
}

func workflowHistoryRevision(ctx context.Context, backend store.Store) (int64, error) {
	if reader, ok := backend.(store.WorkflowHistoryRevisionReader); ok {
		return reader.WorkflowHistoryRevision(ctx)
	}
	return 0, nil
}
