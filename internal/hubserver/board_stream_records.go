package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requireBoardRecordProject(ctx context.Context, scope nativeScope) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		return nil
	})
}

func (s *Service) dashboardBoardRecordPage(ctx context.Context, d *dashboardDelivery, sub *dashboardSubscription, scope nativeScope) (bool, error) {
	frame := tracker.NativeBoardFrame{Deltas: []tracker.NativeBoardDelta{}}
	var rows []viewstore.BoardDelta
	var head int64
	var projectSlug string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
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
		sequence, err := r.BoardSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		head, frame.Sequence = sequence.Sequence, sequence.Sequence
		frame.Gap = sub.sequence < sequence.ReplayFloor || sub.sequence > head || !sub.initial && sub.grantVersion != frame.GrantVersion
		if frame.Gap {
			return nil
		}
		rows, err = r.BoardDeltas(ctx, scope.project, sub.sequence, min(d.config.pageSize, corestore.MaxPageSize))
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
			delta := tracker.NativeBoardDelta{Sequence: row.Sequence}
			for _, item := range []struct {
				ref  *attachment.ArchiveReference
				card **tracker.NativeBoardCard
			}{{row.Previous, &delta.Previous}, {row.Current, &delta.Current}} {
				if item.ref == nil {
					continue
				}
				card, err := viewstore.NewContent(s.runDetailStore()).Board(ctx, scope.organization, *item.ref)
				if err != nil {
					return false, err
				}
				if card.Issue.ProjectID != scope.project || card.Issue.WorkItemID != row.WorkItemID {
					return false, viewstore.ErrInvalidView
				}
				if scope.credential.HostedRole == "reporter" {
					err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
						current, err := r.WorkItem(ctx, scope.project, row.WorkItemID)
						if err != nil {
							return err
						}
						if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
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
				if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, &card.Issue); err != nil {
					return false, err
				}
				if card.Change != nil {
					detail, err := s.selectedChangeDetail(ctx, scope, string(row.WorkItemID), card.Change.ID, s.config.now())
					if errors.Is(err, corestore.ErrNotFound) || isNativeNotFound(err) {
						card.Change = nil
					} else if err != nil {
						return false, err
					} else {
						card.Change.Status = detail.Summary.Status
					}
				}
				card.Issue.WebURL = issueBrowserPath(organizationSlug, projectSlug, card.Issue.Number)
				card.Issue = s.nativeIssueResponse(scope.reporterIssue(card.Issue))
				card.StatusLine = nativeBoardStatusLine(card)
				*item.card = &card
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
	frame.Work = summary.Work
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
