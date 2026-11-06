package hubserver

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authorizeBoardModel(ctx context.Context, reader tenantstore.Reader, scope nativeScope, snapshot *viewstore.BoardModelPage) error {
	ids := make([]tracker.NativeWorkItemID, 0, len(snapshot.Records)+len(snapshot.WorkRecords))
	bytes := 0
	changes := []string{}
	for _, records := range [][]viewstore.BoardRecord{snapshot.Records, snapshot.WorkRecords} {
		for _, row := range records {
			card, err := boardModelCard(scope, row)
			if err != nil {
				return err
			}
			if row.Display.Change != nil {
				changes = append(changes, row.Display.Change.ID)
			}
			ids = append(ids, row.WorkItemID)
			ids = append(ids, card.Issue.Dependencies...)
			for _, links := range [][]tracker.NativeDependency{card.Issue.Blockers, card.Issue.Dependents, card.Issue.Related} {
				for _, link := range links {
					ids = append(ids, link.ID)
					bytes += len(link.ID)
				}
			}
			if len(ids) > 20000 || bytes > 1<<20 {
				return operatortool.ErrResultTooLarge
			}
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > 10000 {
		return operatortool.ErrResultTooLarge
	}
	if len(ids) == 0 {
		return nil
	}
	model, ok := reader.(interface {
		BoardAuthorizedItems(context.Context, accessstore.Authority, []tracker.NativeWorkItemID, []string, string) (viewstore.BoardReadAuthority, error)
	})
	if !ok {
		return workItemStorageUnavailable()
	}
	reporter := ""
	if scope.credential.HostedRole == "reporter" {
		reporter = scope.actor().PrincipalID
	}
	allowed, err := model.BoardAuthorizedItems(ctx, s.accessAuthority(scope, false), ids, changes, reporter)
	if err != nil {
		return err
	}
	for _, records := range [][]viewstore.BoardRecord{snapshot.Records, snapshot.WorkRecords} {
		for index := range records {
			row := &records[index]
			if row.Display.Change != nil && !allowed.Changes[row.Display.Change.ID] {
				row.Display.Change = nil
			}
			if allowed.Items[row.WorkItemID] != scope.project {
				return accessstore.ErrDenied
			}
			card, err := boardModelCard(scope, *row)
			if err != nil {
				return err
			}
			hidden := false
			keep := func(values []tracker.NativeDependency) []tracker.NativeDependency {
				if values == nil {
					return nil
				}
				result := make([]tracker.NativeDependency, 0, len(values))
				for _, link := range values {
					if allowed.Items[link.ID] == link.ProjectID {
						result = append(result, link)
					} else {
						hidden = true
					}
				}
				return result
			}
			issue := card.Issue
			issue.Blockers, issue.Dependents, issue.Related = keep(issue.Blockers), keep(issue.Dependents), keep(issue.Related)
			if issue.Dependencies != nil {
				ids := make([]tracker.NativeWorkItemID, 0, len(issue.Dependencies))
				for _, id := range issue.Dependencies {
					if allowed.Items[id] != "" {
						ids = append(ids, id)
					} else {
						hidden = true
					}
				}
				issue.Dependencies = ids
			}
			if hidden {
				issue.BlockedReason, issue.UnblocksReason = tracker.DependencyReason{}, tracker.DependencyReason{}
				issue.DependencyReady = false
				issue.OmittedFields = append(slices.Clone(issue.OmittedFields), "dependency_ready", "blocked_reason", "unblocks_reason")
			}
			row.PageIssue = &issue
			if reporter != "" {
				facts := *row.ReporterRelations
				facts.Dependencies, facts.Blockers, facts.Dependents, facts.Related = issue.Dependencies, issue.Blockers, issue.Dependents, issue.Related
				facts.DependencyReady, facts.BlockedReason, facts.UnblocksReason = issue.DependencyReady, issue.BlockedReason, issue.UnblocksReason
				row.ReporterRelations = &facts
			}
		}
	}
	return nil
}
