package orchestrator

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (o *Orchestrator) sweepRetention(ctx context.Context, state *State, now time.Time) {
	sweeper, ok := o.reaper.(workspace.RetentionSweeper)
	if !ok {
		return
	}
	active := activeWorkspaceIssues(state, o.cfg.TerminalStates)
	request := workspace.RetentionRequest{Now: now}
	for _, issue := range active {
		request.Active = append(request.Active, workspace.Issue{ProjectID: o.cfg.Project.ID, ID: issue.ID, Identifier: issue.Identifier})
	}
	request.Completed = func(ctx context.Context, owned []workspace.Issue) (map[string]time.Time, error) {
		completed := map[string]time.Time{}
		var ids []string
		for _, issue := range owned {
			if issue.ID != "" {
				ids = append(ids, issue.ID)
			}
		}
		for start := 0; start < len(ids); start += 100 {
			issues, err := o.connector.FetchIssueStatesByIDs(ctx, ids[start:min(start+100, len(ids))])
			if err != nil {
				return nil, err
			}
			for _, issue := range issues {
				if !issue.Closed && !stateIn(issue.State, []string{"done", "cancelled", "canceled"}) {
					continue
				}
				var since *time.Time
				if issue.Closed {
					since = issue.ClosedAt
				}
				if stateIn(issue.State, []string{"done", "cancelled", "canceled"}) {
					entered := issue.StageUpdatedAt
					if entered == nil {
						if observed, ok := state.laneEntries[workflowLaneEntryKey(issue)]; ok {
							entered = &observed
						}
					}
					if entered == nil {
						if reader, ok := o.connector.(connector.IssueStateTransitionReader); ok {
							transition, found, err := reader.IssueStateTransition(ctx, issue)
							if err != nil {
								return nil, err
							}
							if found {
								entered = &transition.EnteredAt
							}
						}
					}
					if entered != nil && (since == nil || entered.Before(*since)) {
						since = entered
					}
				}
				if since != nil {
					completed[issue.ID] = *since
				}
			}
		}
		return completed, nil
	}
	totals, err := sweeper.SweepRetention(ctx, request)
	if totals.Workdir != "" {
		state.WorkspaceRetention = []workspace.RetentionTotals{totals}
	}
	if err != nil {
		o.logger.Warn("workspace retention failed", "error", err)
	}
}
