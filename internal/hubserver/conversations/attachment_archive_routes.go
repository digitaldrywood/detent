package conversations

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s AttachmentArchiveHTTP) CheckAttachmentArchive(c echo.Context) error {
	var input attachment.ArchiveCheck
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if input.Reference != nil || input.Fence != nil || hubscope.NativeRequestScope(c).Credential.Scope == hubapi.APIScopeWorker {
		return s.writeAttachmentArchive(c, input.Reference, input.Fence, false)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	row, _, err := s.deps.ReadWorkItem(ctx, scope, c.Param("item"))
	if err != nil {
		return s.deps.APIError(c, err)
	}
	if err := s.deps.RequireReporterTriage(scope, tracker.NativeIssue{State: hubapi.StringValue(row.WorkflowStateName)}); err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.Credential.ID})
}

func (s AttachmentArchiveHTTP) RecordAttachmentArchive(c echo.Context) error {
	var input attachment.ArchivePublication
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	return s.writeAttachmentArchive(c, &input.ArchiveReference, input.Fence, true)
}

func (s AttachmentArchiveHTTP) writeAttachmentArchive(c echo.Context, ref *attachment.ArchiveReference, fence *attachment.ArchiveFence, publish bool) error {
	scope := hubscope.NativeRequestScope(c)
	if (scope.Credential.Scope == hubapi.APIScopeWorker) != (fence != nil) || fence != nil && !fence.Valid() {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid archive execution fence"))
	}
	item := tracker.NativeWorkItemID(c.Param("item"))
	var record attachmentstore.ArchiveRecord
	if ref != nil {
		organization := string(scope.Organization)
		if ref.Validate(organization) != nil || (fence != nil) != ref.ExecutionScoped(organization) {
			return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid archive reference"))
		}
		if fence != nil {
			key, err := fence.Key(organization, string(scope.Project), string(item), ref.BindingID, ref.SHA256)
			if err != nil || ref.Key != key {
				return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid archive reference"))
			}
		}
		identity := ref.Key
		if ref.BindingID != "" {
			identity = ref.BindingID + "\n" + identity
		}
		record = attachmentstore.ArchiveRecord{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: "att_" + artifact.Digest([]byte(organization + "\n" + identity))[:32], Uploader: scope.Credential.ID, CreatedAt: s.deps.Now(), Reference: *ref}
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope.Action = operatortool.UploadAttachment
	checkFence := func(ctx context.Context, w tenantstore.Writer, row corestore.WorkItem) error {
		if fence == nil {
			return nil
		}
		return s.deps.AuthorizeFence(ctx, w, scope, row, tracker.Mutation{LeaseID: fence.LeaseID, FencingToken: fence.FencingToken})
	}
	err := s.deps.WriteTenant(ctx, scope.Organization, func(w tenantstore.Writer) error {
		row, err := s.lockArchiveItem(ctx, w, scope, item)
		if err != nil {
			return err
		}
		if err := checkFence(ctx, w, row); err != nil || ref == nil {
			return err
		}
		err = s.persistArchive(ctx, w, record, row.NativeID, publish)
		if err != nil {
			return err
		}
		return checkFence(ctx, w, row)
	})
	if publish && fence != nil && errors.Is(err, tracker.ErrStaleFencingToken) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.ReadTimeout)
		defer cancel()
		cleanupErr := s.deps.WriteTenant(cleanup, scope.Organization, func(w tenantstore.Writer) error {
			row, err := s.lockArchiveItem(cleanup, w, scope, item)
			if err != nil {
				return err
			}
			if err := checkFence(cleanup, w, row); !errors.Is(err, tracker.ErrStaleFencingToken) {
				return err
			}
			return w.RejectAttachmentArchive(cleanup, record, item, s.deps.Now().UTC())
		})
		failure := &hubapi.NativeError{Code: "stale_fencing_token", Message: "Lease fencing token is stale", Status: http.StatusConflict, PublicMessage: true}
		if cleanupErr != nil {
			return s.deps.APIError(c, failure, "cleanup_error", cleanupErr)
		}
		return s.deps.APIError(c, failure)
	}
	if err != nil {
		return s.deps.APIError(c, hubapi.ProjectRecordError(err))
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	if !publish {
		return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.Credential.ID})
	}
	return c.JSON(http.StatusCreated, ref)
}

func (s AttachmentArchiveHTTP) lockArchiveItem(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
		return corestore.WorkItem{}, err
	}
	if _, err := s.deps.WriteAuthority(ctx, w, scope, nil); err != nil {
		return corestore.WorkItem{}, err
	}
	row, err := w.LockWorkItem(ctx, scope.Project, item)
	if err != nil {
		return row, err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
		return row, hubapi.NativeNotFound()
	}
	return row, s.deps.RequireReporterTriage(scope, tracker.NativeIssue{State: hubapi.StringValue(row.WorkflowStateName)})
}

func (s AttachmentArchiveHTTP) persistArchive(ctx context.Context, w tenantstore.Writer, record attachmentstore.ArchiveRecord, item tracker.NativeWorkItemID, publish bool) error {
	if publish {
		return w.PublishAttachmentArchive(ctx, record, item)
	}
	created, err := w.PrepareAttachmentArchive(ctx, record)
	if err != nil || !created {
		return err
	}
	limit, err := s.deps.QuotaLimit(ctx, w)
	if err != nil {
		return err
	}
	delta := record.Reference.CompressedSize
	if record.Reference.CustomerOwned {
		delta = 0
	}
	usage, err := w.AdjustCollaborationBytes(ctx, delta, limit)
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
	}
	return nil
}

func (s AttachmentArchiveHTTP) ReadAttachmentArchive(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	if !artifact.ValidHash(c.Param("digest"), 64) {
		return s.deps.APIError(c, hubapi.NativeNotFound())
	}
	if _, _, err := s.deps.ReadWorkItem(ctx, scope, c.Param("item")); err != nil {
		return s.deps.APIError(c, err)
	}
	if scope.Credential.HostedRole == "reporter" {
		return s.deps.APIError(c, hubapi.NativeNotFound())
	}
	query := attachmentstore.ArchiveQuery{ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(c.Param("item")), Digest: c.Param("digest"), Section: c.QueryParam("section")}
	if c.QueryParams().Has("binding") {
		binding := c.QueryParam("binding")
		query.Binding = &binding
	}
	if query.Section != "" {
		revision, err := strconv.ParseInt(c.QueryParam("revision"), 10, 64)
		if err != nil || revision < 1 {
			return s.deps.APIError(c, hubapi.NativeNotFound())
		}
		query.Revision = revision
		if strings.HasPrefix(query.Section, "conversation:") {
			if _, err := s.deps.ReadConversation(ctx, scope, strings.TrimPrefix(query.Section, "conversation:")); err != nil {
				return s.deps.APIError(c, err)
			}
		}
	}
	var ref attachment.ArchiveReference
	err := s.deps.ReadTenant(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := s.deps.Authorize(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		ref, err = r.AttachmentArchive(ctx, query)
		return err
	})
	if err != nil {
		return s.deps.APIError(c, hubapi.ProjectRecordError(err))
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, ref)
}
