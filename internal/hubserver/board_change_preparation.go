package hubserver

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardChangePreparation struct {
	rows     map[tenantstore.BoardChangeRequest]tenantstore.BoardChangeRows
	policy   currentChangePolicyRows
	approval *ChangeVersionApproval
	err      error
}

func (s *Service) prepareBoardChangeRows(ctx context.Context, scope nativeScope, cards []tracker.NativeBoardCard) (boardChangePreparation, bool) {
	requests := []tenantstore.BoardChangeRequest{}
	seen := map[tenantstore.BoardChangeRequest]bool{}
	for _, card := range cards {
		if card.Change == nil {
			continue
		}
		request := tenantstore.BoardChangeRequest{WorkItemID: card.Issue.WorkItemID, ChangeID: card.Change.ID}
		if !seen[request] {
			seen[request] = true
			requests = append(requests, request)
		}
	}
	result := boardChangePreparation{rows: map[tenantstore.BoardChangeRequest]tenantstore.BoardChangeRows{}}
	if len(requests) == 0 {
		return result, true
	}
	supported := false
	result.err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		batch, ok := reader.(tenantstore.BoardChangeRowsReader)
		if !ok {
			return nil
		}
		supported = true
		project, err := reader.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		for start := 0; start < len(requests); start += corestore.MaxPageSize {
			rows, err := batch.BoardChangeRows(ctx, scope.project, requests[start:min(start+corestore.MaxPageSize, len(requests))])
			if err != nil {
				return err
			}
			for _, row := range rows {
				result.rows[row.Request] = row
			}
		}
		result.policy, err = readCurrentChangePolicyRows(ctx, reader, scope)
		return err
	})
	if result.err == nil && supported {
		approval, err := hydrateCurrentChangeVersionApproval(ctx, eventstore.NewContent(s.runDetailStore()), scope, result.policy, "")
		result.approval = &approval
		result.err = err
	}
	return result, supported
}
func (p boardChangePreparation) summary(ctx context.Context, s *Service, scope nativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, error) {
	if p.err != nil {
		return tracker.ChangeSummary{}, projectRecordError(p.err)
	}
	row, ok := p.rows[tenantstore.BoardChangeRequest{WorkItemID: item, ChangeID: id}]
	if !ok || row.Change == nil {
		return tracker.ChangeSummary{}, nativeNotFound()
	}
	if row.Change.CurrentVersionID != nil && row.Version == nil {
		return tracker.ChangeSummary{}, projectRecordError(corestore.ErrNotFound)
	}
	detail, err := s.hydrateCurrentChangeDetailWithFacts(ctx, scope, item, *row.Change, row.Version, p.policy, now, &row, p.approval)
	return detail.Summary, err
}
func boardCurrentApproval(ctx context.Context, content *eventstore.Content, scope nativeScope, rows currentChangePolicyRows, approval *ChangeVersionApproval) (ChangeVersionApproval, error) {
	if approval != nil {
		return *approval, nil
	}
	return hydrateCurrentChangeVersionApproval(ctx, content, scope, rows, "")
}
func filterBoardChangeRecord(scope nativeScope, change tracker.ChangeRequest, rows tenantstore.BoardChangeRows) (tracker.ChangeRequest, bool, error) {
	if change.OrganizationID != scope.organization || change.ProjectID != scope.project {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	if len(rows.Links) > 32 {
		return tracker.ChangeRequest{}, false, corestore.ErrInvalidPage
	}
	linked := make([]tracker.NativeWorkItemID, 0, len(rows.Links))
	for _, link := range rows.Links {
		if link.ProjectID != scope.project {
			return tracker.ChangeRequest{}, false, corestore.ErrTenantMismatch
		}
		if !link.ItemExists || !link.VersionExists || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(link.ActorPrincipalID)}) {
			continue
		}
		linked = append(linked, link.WorkItemID)
	}
	slices.Sort(linked)
	linked = slices.Compact(linked)
	if len(linked) == 0 || !slices.Contains(linked, change.WorkItemID) {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	change.LinkedIssues = linked
	return change, true, nil
}
