package hubserver

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) checkAttachmentEvidenceTenant(ctx context.Context, scope nativeScope, input attachment.EvidenceRequest) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if _, err := s.validateRunnerLeaseTenant(ctx, scope, input.LeaseID, input.FencingToken); err != nil {
		return nativeStaleLease(err, "Evidence producer lease is no longer current")
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(ctx, scope.project, input.WorkItemID)
		if err != nil {
			return projectRecordError(err)
		}
		if item.Archived {
			return nativeNotFound()
		}
		lease, err := r.Lease(ctx, string(input.LeaseID))
		if err != nil {
			return projectRecordError(err)
		}
		now := s.config.now().UTC()
		if lease.OrganizationID != scope.organization || lease.IssueID != item.ID || lease.FencingToken != int64(input.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
			return nativeStaleExecution("Evidence producer lease is no longer current")
		}
		attempt, err := r.Attempt(ctx, input.AttemptID)
		if err != nil {
			return projectRecordError(err)
		}
		if attempt.OrganizationID != scope.organization || attempt.IssueID != item.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Status != "running" {
			return nativeStaleExecution("Evidence producer is not the running attempt")
		}
		if s.database.plans() != nil {
			if _, err := r.CollaborationBytes(ctx); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return &nativeError{Code: "storage_owner_unavailable", Message: "Evidence upload accounting is unavailable with PostgreSQL storage", status: http.StatusServiceUnavailable, publicMessage: true}
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) publishEvidenceCompletion(ctx context.Context, w tenantstore.Writer, publication attachmentEvidencePublication) error {
	if _, err := w.AdjustCollaborationBytes(ctx, publication.bytes, math.MaxInt64); err != nil {
		return err
	}
	if err := writeWorkItemRecord(ctx, w, publication.plan, publication.fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, publication.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, publication.event); err != nil {
		return err
	}
	return w.PutSearch(ctx, publication.search, 0)
}
