package executionapi

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d EvidenceDeps) CheckAttachmentEvidenceTenant(ctx context.Context, scope hubscope.NativeScope, input attachment.EvidenceRequest) error {
	if input.LeaseID == "" || input.FencingToken <= 0 {
		return hubapi.NativeStaleExecution("Evidence producer lease is no longer current")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if _, err := d.ValidateLease(ctx, scope, input.LeaseID, input.FencingToken); err != nil {
		return d.StaleLease(err, "Evidence producer lease is no longer current")
	}
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(ctx, scope.Project, input.WorkItemID)
		if err != nil {
			return hubapi.ProjectRecordError(err)
		}
		if item.Archived {
			return hubapi.NativeNotFound()
		}
		lease, err := r.Lease(ctx, string(input.LeaseID))
		if err != nil {
			return hubapi.ProjectRecordError(err)
		}
		now := d.Now().UTC()
		if lease.OrganizationID != scope.Organization || lease.IssueID != item.ID || lease.FencingToken != int64(input.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
			return hubapi.NativeStaleExecution("Evidence producer lease is no longer current")
		}
		attempt, err := r.Attempt(ctx, input.AttemptID)
		if err != nil {
			return hubapi.ProjectRecordError(err)
		}
		if attempt.OrganizationID != scope.Organization || attempt.IssueID != item.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Status != "running" {
			return hubapi.NativeStaleExecution("Evidence producer is not the running attempt")
		}
		if d.PlansEnabled() {
			if _, err := r.CollaborationBytes(ctx); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return &hubapi.NativeError{Code: "storage_owner_unavailable", Message: "Evidence upload accounting is unavailable with PostgreSQL storage", Status: http.StatusServiceUnavailable, PublicMessage: true}
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

func PublishEvidenceCompletion(ctx context.Context, w tenantstore.Writer, publication hubrecords.AttachmentEvidencePublication) error {
	if _, err := w.AdjustCollaborationBytes(ctx, publication.Bytes, math.MaxInt64); err != nil {
		return err
	}
	if err := hubrecords.WriteWorkItemRecord(ctx, w, publication.Plan, publication.Fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, publication.Version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, publication.Event); err != nil {
		return err
	}
	return nil
}
