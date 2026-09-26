package orchestrator

import (
	"context"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const workspaceCleanupBatchSize = 10

type workspaceCleanupResult struct {
	before State
	after  State
}

// The event loop owns state; the existing reaper works only on a private copy.
// There is at most one sweep, including retention and cache trimming, in flight.
func (o *Orchestrator) startWorkspaceCleanup(ctx context.Context, state *State, now time.Time) {
	if o.reaper == nil || o.workspaceCleanupCancel != nil {
		return
	}
	if o.workspaceCleanupResults == nil {
		o.workspaceCleanupResults = make(chan workspaceCleanupResult, 1)
	}
	before := state.clone()
	after := before.clone()
	after.RecentEvents = nil
	if o.quarantineWarnings == nil {
		o.quarantineWarnings = make(map[string]struct{})
	}
	worker := &Orchestrator{cfg: o.cfg, connector: o.connector, reaper: o.reaper, logger: o.logger, trimHostCache: o.trimHostCache, quarantineWarnings: o.quarantineWarnings}
	remaining := workspaceCleanupBatchSize
	worker.workspaceCleanupRemaining = &remaining
	cleanupCtx, cancel := context.WithCancel(ctx)
	cleanupCtx = workspace.WithCleanupBatch(cleanupCtx, workspaceCleanupBatchSize)
	o.workspaceCleanupCancel = cancel
	o.workspaceCleanupWG.Add(1)
	go func() {
		defer o.workspaceCleanupWG.Done()
		worker.reapWorkspacesIfDue(cleanupCtx, &after, now)
		if cleanupCtx.Err() == nil {
			worker.reapDueWorkspacesAfterRefresh(cleanupCtx, &after, now)
		}
		o.workspaceCleanupResults <- workspaceCleanupResult{before: before, after: after}
	}()
}

func (o *Orchestrator) finishWorkspaceCleanup(state *State, result workspaceCleanupResult) {
	o.workspaceCleanupCancel()
	o.workspaceCleanupCancel = nil
	before, after := result.before, result.after
	state.LastWorkspaceCleanupAt = after.LastWorkspaceCleanupAt
	state.workspaceCleanupCursor = after.workspaceCleanupCursor
	state.WorkspaceRetention = after.WorkspaceRetention
	for id, at := range after.ReapedWorkspaces {
		if _, existed := before.ReapedWorkspaces[id]; existed {
			continue
		}
		// A new dispatch clears this marker. A result from an older sweep must
		// not restore it while the replacement worker owns the workspace.
		if _, running := state.Running[id]; running {
			continue
		}
		if _, retry := state.Retry[id]; retry {
			continue
		}
		if _, blocked := state.Blocked[id]; blocked {
			continue
		}
		if state.Completed[id].StartedAt.After(before.Completed[id].StartedAt) {
			continue
		}
		state.ReapedWorkspaces[id] = at
	}
	for path, old := range before.CleanupFailures {
		if _, remains := after.CleanupFailures[path]; !remains && state.CleanupFailures[path] == old {
			delete(state.CleanupFailures, path)
		}
	}
	for path, failure := range after.CleanupFailures {
		if failure != before.CleanupFailures[path] {
			state.CleanupFailures[path] = failure
		}
	}
	if after.CleanupFailureAt.After(state.CleanupFailureAt) {
		state.CleanupFailureAt = after.CleanupFailureAt
	}
	for _, event := range after.RecentEvents {
		recordStateEvent(state, event)
	}
}

func (o *Orchestrator) stopWorkspaceCleanup() {
	if o.workspaceCleanupCancel != nil {
		o.workspaceCleanupCancel()
	}
	o.workspaceCleanupWG.Wait()
}

func cleanupIssueOrder(issues []connector.Issue, cursor string) []connector.Issue {
	ordered := append([]connector.Issue(nil), issues...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	next := sort.Search(len(ordered), func(i int) bool { return ordered[i].ID > cursor })
	return append(ordered[next:], ordered[:next]...)
}
