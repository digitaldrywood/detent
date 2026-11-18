package hubserver

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authorizeBoardModel(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, snapshot *viewstore.BoardModelPage) error {
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
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	reporter := ""
	if scope.Credential.HostedRole == "reporter" {
		reporter = scope.Actor().PrincipalID
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
				if row.PageIssue == nil {
					return viewstore.ErrInvalidView
				}
				issue := *row.PageIssue
				issue.Change, issue.ListChange, issue.PullRequest = nil, nil, nil
				row.PageIssue = &issue
			}
			if allowed.Items[row.WorkItemID] != scope.Project {
				return accessstore.ErrDenied
			}
			card, err := boardModelCard(scope, *row)
			if err != nil {
				return err
			}
			keep := func(values []tracker.NativeDependency) []tracker.NativeDependency {
				if values == nil {
					return nil
				}
				return slices.DeleteFunc(slices.Clone(values), func(link tracker.NativeDependency) bool { return allowed.Items[link.ID] != link.ProjectID })
			}
			issue := card.Issue
			issue.Blockers, issue.Dependents, issue.Related = keep(issue.Blockers), keep(issue.Dependents), keep(issue.Related)
			if issue.Dependencies != nil {
				issue.Dependencies = slices.DeleteFunc(slices.Clone(issue.Dependencies), func(id tracker.NativeWorkItemID) bool { return allowed.Items[id] == "" })
			}
			issue.DependencyReady = !slices.ContainsFunc(issue.Blockers, func(link tracker.NativeDependency) bool { return !link.Terminal })
			blocked, unblocks := make([]string, 0, len(issue.Dependencies)), make([]string, 0, len(issue.Dependents))
			for _, id := range issue.Dependencies {
				blocked = append(blocked, string(id))
			}
			for _, link := range issue.Dependents {
				unblocks = append(unblocks, string(link.ID))
			}
			crossProject := func(link tracker.NativeDependency) bool { return link.ProjectID != scope.Project }
			issue.BlockedReason = workitemread.VisibleDependencyReason(issue.BlockedReason, blocked, slices.ContainsFunc(issue.Blockers, crossProject))
			issue.UnblocksReason = workitemread.VisibleDependencyReason(issue.UnblocksReason, unblocks, slices.ContainsFunc(issue.Dependents, crossProject))
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
