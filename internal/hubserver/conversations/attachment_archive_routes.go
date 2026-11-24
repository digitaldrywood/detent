package conversations

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
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
	var input struct {
		Reference *attachment.ArchiveReference `json:"reference,omitempty"`
	}
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if input.Reference != nil {
		return s.writeAttachmentArchive(c, *input.Reference, false)
	}
	ctx := c.Request().Context()
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
	var ref attachment.ArchiveReference
	if err := hubapi.DecodeAPIJSON(c, &ref); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	return s.writeAttachmentArchive(c, ref, true)
}

func (s AttachmentArchiveHTTP) writeAttachmentArchive(c echo.Context, ref attachment.ArchiveReference, publish bool) error {
	scope := hubscope.NativeRequestScope(c)
	if ref.Validate(string(scope.Organization)) != nil {
		return s.deps.APIError(c, hubapi.NativePublicInvalid("Invalid archive reference"))
	}
	ctx := c.Request().Context()
	scope.Action = operatortool.UploadAttachment
	identity := ref.Key
	if ref.BindingID != "" {
		identity = ref.BindingID + "\n" + identity
	}
	record := attachmentstore.ArchiveRecord{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: "att_" + artifact.Digest([]byte(string(scope.Organization) + "\n" + identity))[:32], Uploader: scope.Credential.ID, CreatedAt: s.deps.Now(), Reference: ref}
	err := s.deps.WriteTenant(ctx, scope.Organization, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if _, err := s.deps.WriteAuthority(ctx, w, scope, nil); err != nil {
			return err
		}
		row, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(c.Param("item")))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		if err := s.deps.RequireReporterTriage(scope, tracker.NativeIssue{State: hubapi.StringValue(row.WorkflowStateName)}); err != nil {
			return err
		}
		if publish {
			return w.PublishAttachmentArchive(ctx, record, row.NativeID)
		}
		created, err := w.PrepareAttachmentArchive(ctx, record)
		if err != nil || !created {
			return err
		}
		limit, err := s.deps.QuotaLimit(ctx, w)
		if err != nil {
			return err
		}
		delta := ref.CompressedSize
		if ref.CustomerOwned {
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
	})
	if err != nil {
		return s.deps.APIError(c, hubapi.ProjectRecordError(err))
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	if !publish {
		return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.Credential.ID})
	}
	return c.JSON(http.StatusCreated, ref)
}

func (s AttachmentArchiveHTTP) ReadAttachmentArchive(c echo.Context) error {
	ctx := c.Request().Context()
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
