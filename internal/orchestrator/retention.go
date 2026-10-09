package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
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
	foreign, err := o.liveForeignWorkspaceIssues(ctx)
	if err != nil {
		o.logger.Warn("workspace retention skipped; live worker generation lookup failed", "project_id", o.cfg.Project.ID, "error", err)
		return
	}
	request.Active = append(request.Active, foreign...)
	request.Completed = func(ctx context.Context, owned []workspace.Issue) (map[string]time.Time, error) {
		completed := map[string]time.Time{}
		var ids []string
		for _, issue := range owned {
			if issue.ID != "" {
				ids = append(ids, issue.ID)
			}
		}
		for start := 0; start < len(ids); start += 100 {
			issues, err := o.fetchWorkspaceCleanupIssueStatesByIDs(ctx, ids[start:min(start+100, len(ids))])
			if err != nil {
				return nil, err
			}
			for _, issue := range issues {
				if !issue.Closed && !stateIn(issue.State, o.cfg.TerminalStates) {
					continue
				}
				var since *time.Time
				if issue.Closed {
					since = issue.ClosedAt
				}
				if stateIn(issue.State, o.cfg.TerminalStates) {
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
		o.warnRetentionFailure(err)
	}
}

func (o *Orchestrator) liveForeignWorkspaceIssues(ctx context.Context) ([]workspace.Issue, error) {
	generations, ok := o.workAttempts.(store.LiveForeignGenerationReader)
	if !ok {
		return nil, nil
	}
	foreign, err := generations.LiveForeignWorkerGenerations(ctx)
	if err != nil || len(foreign) == 0 {
		return nil, err
	}
	attempts, err := o.workAttempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: o.cfg.Project.ID})
	if err != nil {
		return nil, fmt.Errorf("list live worker generation attempts: %w", err)
	}
	var issues []workspace.Issue
	for _, attempt := range attempts {
		if slices.Contains(foreign, attempt.OwnerGeneration) {
			issues = append(issues, workspace.Issue{ProjectID: o.cfg.Project.ID, ID: attempt.IssueID, Identifier: attempt.Identifier})
		}
	}
	return issues, nil
}

func (o *Orchestrator) warnRetentionFailure(err error) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, failure := range joined.Unwrap() {
			o.warnRetentionFailure(failure)
		}
		return
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		parent := filepath.Join(".detent", "quarantine")
		path := filepath.Clean(pathError.Path)
		if relative, ok := strings.CutPrefix(path, parent+string(filepath.Separator)); ok {
			name, _, _ := strings.Cut(relative, string(filepath.Separator))
			key := filepath.Join(parent, name)
			if o.quarantineWarnings == nil {
				o.quarantineWarnings = make(map[string]struct{})
			}
			if _, warned := o.quarantineWarnings[key]; warned {
				return
			}
			o.quarantineWarnings[key] = struct{}{}
		}
	}
	o.logger.Warn("workspace retention failed", "error", err)
}
