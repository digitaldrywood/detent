package hubserver

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardChangePreparation struct {
	rows     map[tenantstore.BoardChangeRequest]tenantstore.BoardChangeRows
	policy   hubrecords.CurrentChangePolicyRows
	approval *hubrecords.ChangeVersionApproval
	err      error
	workload corestore.Workload
}

func (s *Service) prepareBoardChangeRows(ctx context.Context, scope hubscope.NativeScope, cards []tracker.NativeBoardCard) (boardChangePreparation, bool) {
	return s.prepareBoardChangeRowsFor(ctx, scope, cards, corestore.Serving)
}

func (s *Service) prepareBoardChangeRowsFor(ctx context.Context, scope hubscope.NativeScope, cards []tracker.NativeBoardCard, workload corestore.Workload) (boardChangePreparation, bool) {
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
	result := boardChangePreparation{rows: map[tenantstore.BoardChangeRequest]tenantstore.BoardChangeRows{}, workload: workload}
	if len(requests) == 0 {
		return result, true
	}
	supported := false
	hasChanges := false
	finishRows := timeSpan(ctx, "board_change_rows")
	result.err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, workload, func(reader tenantstore.Reader) error {
		batch, ok := reader.(tenantstore.BoardChangeRowsReader)
		if !ok {
			return nil
		}
		supported = true
		project, err := reader.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		for start := 0; start < len(requests); start += corestore.MaxPageSize {
			rows, err := batch.BoardChangeRows(ctx, scope.Project, requests[start:min(start+corestore.MaxPageSize, len(requests))])
			if err != nil {
				return err
			}
			for _, row := range rows {
				result.rows[row.Request] = row
				hasChanges = hasChanges || row.Change != nil
			}
		}
		if !hasChanges {
			return nil
		}
		result.policy, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, reader, scope)
		return err
	})
	finishRows()
	if result.err == nil && supported && hasChanges {
		refs := []attachment.ArchiveReference{eventstore.Reference(result.policy.Project.PayloadObjectKey, result.policy.Project.PayloadSHA256, result.policy.Project.PayloadBytes, result.policy.Project.PayloadContentBytes)}
		if result.policy.HasReview {
			r := result.policy.Review
			refs = append(refs, eventstore.Reference(r.PayloadObjectKey, r.PayloadSHA256, r.PayloadBytes, r.PayloadContentBytes))
		}
		for _, row := range result.rows {
			if r := row.Change; r != nil {
				refs = append(refs, eventstore.Reference(r.PayloadObjectKey, r.PayloadSHA256, r.PayloadBytes, r.PayloadContentBytes))
			}
			if r := row.Version; r != nil {
				refs = append(refs, eventstore.Reference(r.PayloadObjectKey, r.PayloadSHA256, r.PayloadBytes, r.PayloadContentBytes))
			}
			for _, r := range row.Evidence {
				current := row.Version != nil && r.VersionID != nil && *r.VersionID == row.Version.ID
				if r.Kind == "review" && (current || r.Decision == "approved") || r.Kind == "check" && current {
					refs = append(refs, eventstore.Reference(r.PayloadObjectKey, r.PayloadSHA256, r.PayloadBytes, r.PayloadContentBytes))
				}
			}
		}
		finishPayloads := timeSpan(ctx, "board_change_payloads")
		s.prefetchDetails(ctx, string(scope.Organization), refs)
		finishPayloads()
		finishPolicy := timeSpan(ctx, "board_change_policy")
		approval, err := hydrateChangeSummaryPolicy(ctx, eventstore.NewContent(s.runDetailStoreFor(ctx)), scope, result.policy)
		finishPolicy()
		result.approval = &approval
		result.err = err
	}
	return result, supported
}
func (p boardChangePreparation) summary(ctx context.Context, s *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, error) {
	if p.err != nil {
		return tracker.ChangeSummary{}, hubapi.ProjectRecordError(p.err)
	}
	row, ok := p.rows[tenantstore.BoardChangeRequest{WorkItemID: item, ChangeID: id}]
	if !ok || row.Change == nil {
		return tracker.ChangeSummary{}, hubapi.NativeNotFound()
	}
	if row.Change.CurrentVersionID != nil && row.Version == nil {
		return tracker.ChangeSummary{}, hubapi.ProjectRecordError(corestore.ErrNotFound)
	}
	detail, err := s.hydrateCurrentChangeDetailWithFacts(ctx, scope, item, *row.Change, row.Version, p.policy, now, &row, p.approval, p.workload)
	return detail.Summary, err
}
func (p boardChangePreparation) evaluate(ctx context.Context, s *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, *viewstore.BoardChangeProvenance, error) {
	if p.err != nil {
		return tracker.ChangeSummary{}, nil, hubapi.ProjectRecordError(p.err)
	}
	row, ok := p.rows[tenantstore.BoardChangeRequest{WorkItemID: item, ChangeID: id}]
	if !ok || row.Change == nil {
		return tracker.ChangeSummary{}, nil, hubapi.NativeNotFound()
	}
	if row.Change.CurrentVersionID != nil && row.Version == nil {
		return tracker.ChangeSummary{}, nil, hubapi.ProjectRecordError(corestore.ErrNotFound)
	}
	detail, err := s.hydrateCurrentChangeDetailWithFacts(ctx, scope, item, *row.Change, row.Version, p.policy, now, &row, p.approval, p.workload)
	if err != nil {
		return tracker.ChangeSummary{}, nil, err
	}
	if len(detail.Versions) == 0 {
		return detail.Summary, &viewstore.BoardChangeProvenance{BaseStatus: detail.Summary.Status}, nil
	}
	version := detail.Versions[0]
	provenance := &viewstore.BoardChangeProvenance{VersionID: version.ID, PolicyID: version.PolicyID, ReviewPolicyID: version.ReviewPolicy.ID, BaseStatus: detail.Summary.Status}
	if p.approval != nil {
		policyID, reviewID := changerequest.CurrentPolicyIdentities(&version, p.approval.Policy, p.approval.ReviewPolicy)
		if policyID == version.PolicyID && reviewID == version.ReviewPolicy.ID {
			return detail.Summary, provenance, nil
		}
	}
	own := hubrecords.ChangeVersionApproval{Policy: version.Policy, ReviewPolicy: version.ReviewPolicy}
	base, err := s.hydrateCurrentChangeDetailWithFacts(ctx, scope, item, *row.Change, row.Version, p.policy, now, &row, &own, p.workload)
	if err != nil {
		return tracker.ChangeSummary{}, nil, err
	}
	provenance.BaseStatus = base.Summary.Status
	return detail.Summary, provenance, nil
}

func boardCurrentApproval(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, approval *hubrecords.ChangeVersionApproval) (hubrecords.ChangeVersionApproval, error) {
	if approval != nil {
		return *approval, nil
	}
	return hydrateChangeSummaryPolicy(ctx, content, scope, rows)
}
func filterBoardChangeRecord(scope hubscope.NativeScope, change tracker.ChangeRequest, rows tenantstore.BoardChangeRows) (tracker.ChangeRequest, bool, error) {
	if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	if len(rows.Links) > 32 {
		return tracker.ChangeRequest{}, false, corestore.ErrInvalidPage
	}
	linked := make([]tracker.NativeWorkItemID, 0, len(rows.Links))
	for _, link := range rows.Links {
		if link.ProjectID != scope.Project {
			return tracker.ChangeRequest{}, false, corestore.ErrTenantMismatch
		}
		if !link.ItemExists || !link.VersionExists || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(link.ActorPrincipalID)}) {
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
