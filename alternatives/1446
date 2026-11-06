package hubserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) registerAttachmentArchiveRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	base := attachmentMetadataBase + "/archives/:item"
	e.POST(base+"/check", s.checkAttachmentArchive, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.POST(base, s.recordAttachmentArchive, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.GET(base+"/:digest", s.readAttachmentArchive, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
}

func (s *Service) checkAttachmentArchive(c echo.Context) error {
	var input struct {
		Reference *attachment.ArchiveReference `json:"reference,omitempty"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if input.Reference != nil {
		return s.writeAttachmentArchive(c, *input.Reference, false)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	row, _, err := s.readWorkItemRecord(ctx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := requireReporterTriage(scope, tracker.NativeIssue{State: stringValue(row.WorkflowStateName)}); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.credential.ID})
}

func (s *Service) recordAttachmentArchive(c echo.Context) error {
	var ref attachment.ArchiveReference
	if err := decodeAPIJSON(c, &ref); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.writeAttachmentArchive(c, ref, true)
}

func (s *Service) writeAttachmentArchive(c echo.Context, ref attachment.ArchiveReference, publish bool) error {
	scope := nativeRequestScope(c)
	if ref.Validate(string(scope.organization)) != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid archive reference"))
	}
	ctx := c.Request().Context()
	scope.action = operatortool.UploadAttachment
	identity := ref.Key
	if ref.BindingID != "" {
		identity = ref.BindingID + "\n" + identity
	}
	record := attachmentstore.ArchiveRecord{OrganizationID: scope.organization, ProjectID: scope.project, ID: "att_" + artifact.Digest([]byte(string(scope.organization) + "\n" + identity))[:32], Uploader: scope.credential.ID, CreatedAt: s.config.now(), Reference: ref}
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		if _, err := s.attachmentWriteAuthority(ctx, w, scope, nil); err != nil {
			return err
		}
		row, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(c.Param("item")))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		if err := requireReporterTriage(scope, tracker.NativeIssue{State: stringValue(row.WorkflowStateName)}); err != nil {
			return err
		}
		if publish {
			return w.PublishAttachmentArchive(ctx, record, row.NativeID)
		}
		created, err := w.PrepareAttachmentArchive(ctx, record)
		if err != nil || !created {
			return err
		}
		limit, err := s.attachmentQuotaLimit(ctx, w)
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
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	if !publish {
		return c.JSON(http.StatusOK, map[string]string{"principal_id": scope.credential.ID})
	}
	return c.JSON(http.StatusCreated, ref)
}

func scanAttachmentArchive(row interface{ Scan(...any) error }) (attachment.ArchiveReference, error) {
	var ref attachment.ArchiveReference
	err := row.Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned)
	return ref, err
}

func (s *Service) readAttachmentArchive(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	if !artifact.ValidHash(c.Param("digest"), 64) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if _, _, err := s.readWorkItemRecord(ctx, scope, c.Param("item")); err != nil {
		return s.nativeAPIError(c, err)
	}
	if scope.credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	query := attachmentstore.ArchiveQuery{ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(c.Param("item")), Digest: c.Param("digest"), Section: c.QueryParam("section")}
	if c.QueryParams().Has("binding") {
		binding := c.QueryParam("binding")
		query.Binding = &binding
	}
	if query.Section != "" {
		revision, err := strconv.ParseInt(c.QueryParam("revision"), 10, 64)
		if err != nil || revision < 1 {
			return s.nativeAPIError(c, nativeNotFound())
		}
		query.Revision = revision
		if strings.HasPrefix(query.Section, "conversation:") {
			if _, err := s.readPGConversation(ctx, scope, strings.TrimPrefix(query.Section, "conversation:")); err != nil {
				return s.nativeAPIError(c, err)
			}
		}
	}
	var ref attachment.ArchiveReference
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		ref, err = r.AttachmentArchive(ctx, query)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, ref)
}
