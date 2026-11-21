package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requireBoardRecordProject(ctx context.Context, scope hubscope.NativeScope) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		return nil
	})
}

func (s *Service) dashboardBoardRecordPage(ctx context.Context, d *dashboardDelivery, sub *dashboardSubscription, scope hubscope.NativeScope) (bool, error) {
	frame := tracker.NativeBoardFrame{Deltas: []tracker.NativeBoardDelta{}}
	var rows []viewstore.BoardDelta
	status := map[tracker.NativeWorkItemID]viewstore.BoardStatusRecord{}
	var head int64
	var projectSlug string
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		projectSlug = project.Slug
		frame.GrantVersion, err = s.boardAccessVersion(ctx, r, scope)
		if err != nil {
			return err
		}
		sequence, err := r.BoardSequence(ctx, scope.Project)
		if err != nil {
			return err
		}
		head, frame.Sequence = sequence.Sequence, sequence.Sequence
		frame.Gap = sub.sequence < sequence.ReplayFloor || sub.sequence > head || sub.grantVersion != "" && sub.grantVersion != frame.GrantVersion
		if frame.Gap {
			return nil
		}
		rows, err = r.BoardDeltas(ctx, scope.Project, sub.sequence, min(d.config.PageSize, corestore.MaxPageSize))
		if err != nil {
			return err
		}
		selection := viewstore.BoardSelection{ProjectID: scope.Project}
		for _, row := range rows {
			if row.Topic == "card" {
				selection.WorkItemIDs = append(selection.WorkItemIDs, row.WorkItemID)
			}
		}
		if len(selection.WorkItemIDs) == 0 {
			return nil
		}
		model, ok := r.(interface {
			BoardStatusModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardStatusRecord, error)
		})
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		values, err := model.BoardStatusModel(ctx, selection)
		for _, value := range values {
			status[value.WorkItemID] = value
		}
		return err
	})
	if err != nil {
		return false, err
	}
	if !sub.initial && sub.sequence == head && sub.grantVersion == frame.GrantVersion {
		return false, nil
	}
	organizationSlug, err := s.readableOrganizationSlug(ctx)
	if err != nil {
		return false, err
	}
	if !frame.Gap {
		frame.Sequence = sub.sequence
		for _, row := range rows {
			if row.Sequence > head {
				break
			}
			if row.Sequence != frame.Sequence+1 {
				frame.Gap, frame.Sequence, frame.Deltas = true, head, []tracker.NativeBoardDelta{}
				break
			}
			if row.Topic != "card" {
				frame.Sequence = row.Sequence
				continue
			}
			delta := tracker.NativeBoardDelta{Sequence: row.Sequence}
			var stored [2]tracker.NativeIssue
			for position, item := range []struct {
				ref  *attachment.ArchiveReference
				card **tracker.NativeBoardCard
			}{{row.Previous, &delta.Previous}, {row.Current, &delta.Current}} {
				if item.ref == nil {
					continue
				}
				card, err := viewstore.NewContent(s.runDetailStore()).Board(ctx, scope.Organization, *item.ref)
				if err != nil {
					return false, err
				}
				if card.Issue.ProjectID != scope.Project || card.Issue.WorkItemID != row.WorkItemID {
					return false, viewstore.ErrInvalidView
				}
				if scope.Credential.HostedRole == "reporter" {
					err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						current, err := r.WorkItem(ctx, scope.Project, row.WorkItemID)
						if err != nil {
							return err
						}
						if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
							return corestore.ErrNotFound
						}
						return nil
					})
					if errors.Is(err, corestore.ErrNotFound) {
						continue
					}
					if err != nil {
						return false, err
					}
				}
				stored[position] = card.Issue
				if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, &card.Issue); err != nil {
					return false, err
				}
				if card.Change != nil {
					detail, err := s.selectedChangeDetail(ctx, scope, string(row.WorkItemID), card.Change.ID, s.config.Clock())
					if errors.Is(err, corestore.ErrNotFound) || hubapi.IsNativeNotFound(err) {
						card.Change = nil
					} else if err != nil {
						return false, err
					} else {
						card.Change.Status = detail.Summary.Status
					}
				}
				card.Issue.WebURL = hubrecords.IssueBrowserPath(organizationSlug, projectSlug, card.Issue.Number)
				card.Issue = s.nativeIssueResponse(scope.ReporterIssue(card.Issue))
				current := status[row.WorkItemID]
				attempt, execution := card.Attempt, card.Execution
				if attempt != nil && current.Attempt != nil && attempt.ID == current.Attempt.ID && current.Attempt.Status == "running" {
					attempt = nil
				}
				if execution != nil && current.Execution != nil && execution.ID == current.Execution.ID && (current.Execution.Running || current.Execution.Stage != "") {
					execution = nil
				}
				viewstore.ExpireLease(attempt, execution, "")
				card.StatusLine = nativeBoardStatusLine(card)
				*item.card = &card
			}
			if delta.Previous != nil && delta.Current != nil {
				unchanged, err := s.boardDeltaUnchangedForViewer(ctx, scope, delta, stored)
				if err != nil {
					return false, err
				}
				if unchanged {
					delta.Previous, delta.Current = nil, nil
				}
			}
			if delta.Previous != nil || delta.Current != nil {
				frame.Deltas = append(frame.Deltas, delta)
			}
			frame.Sequence = row.Sequence
		}
		if len(rows) == 0 && sub.sequence < head {
			frame.Gap, frame.Sequence = true, head
		}
	}
	params := make(url.Values, len(sub.params)+2)
	for key, values := range sub.params {
		params[key] = append([]string{}, values...)
	}
	params.Set("include", "board")
	params.Set("limit", "1")
	params.Del("cursor")
	summary, err := s.readBoardRecords(ctx, scope, params)
	if err != nil {
		return false, err
	}
	frame.CountsIndependent = true
	if summary.ChangeSequence != head || summary.GrantVersion != frame.GrantVersion {
		frame.Gap, frame.Sequence, frame.GrantVersion, frame.Deltas = true, summary.ChangeSequence, summary.GrantVersion, []tracker.NativeBoardDelta{}
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return false, err
	}
	if !sub.enqueue(dashboardFrame{sequence: frame.Sequence, event: "activity", data: string(raw)}) {
		return false, nil
	}
	sub.sequence, sub.initial, sub.grantVersion = frame.Sequence, false, frame.GrantVersion
	return !frame.Gap && frame.Sequence < head, nil
}

func (s *Service) boardDeltaUnchangedForViewer(ctx context.Context, scope hubscope.NativeScope, delta tracker.NativeBoardDelta, stored [2]tracker.NativeIssue) (bool, error) {
	previous, err := json.Marshal(delta.Previous)
	if err != nil {
		return false, err
	}
	current, err := json.Marshal(delta.Current)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(previous, current) {
		return false, nil
	}
	hydrated := map[tracker.NativeWorkItemID]bool{}
	for _, issue := range []tracker.NativeIssue{delta.Previous.Issue, delta.Current.Issue} {
		for _, links := range [][]tracker.NativeDependency{issue.Blockers, issue.Dependents, issue.Related} {
			for _, link := range links {
				hydrated[link.ID] = true
			}
		}
	}
	projects := map[tracker.ProjectID]bool{}
	for _, issue := range stored {
		for _, links := range [][]tracker.NativeDependency{issue.Blockers, issue.Dependents, issue.Related} {
			for _, link := range links {
				projects[link.ProjectID] = true
			}
		}
	}
	visible, err := s.relationshipProjectAccess(ctx, scope, projects)
	if err != nil {
		return false, err
	}
	var views [2][3][]tracker.NativeDependency
	for position, issue := range stored {
		for kind, links := range [][]tracker.NativeDependency{issue.Blockers, issue.Dependents, issue.Related} {
			for _, link := range links {
				if visible[link.ProjectID] && (scope.Credential.HostedRole != "reporter" || hydrated[link.ID]) {
					views[position][kind] = append(views[position][kind], link)
				}
			}
		}
	}
	return reflect.DeepEqual(views[0], views[1]), nil
}
