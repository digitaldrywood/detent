package hubserver

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const attachmentMetadataBase = hubapi.NativeBase + "/attachment-metadata"

var cloudAttachmentReference = regexp.MustCompile(`\]\(\s*<?attachment:(att_[A-Za-z0-9_-]+)>?(?:\s+"[^"]*")?\s*\)`)

type cloudContentField struct {
	name  string
	value string
}

func cloudContentAttachmentReferences(body string, scope nativeScope) ([]attachment.Reference, error) {
	refs, err := attachment.MarkdownReferences(body, string(scope.organization), string(scope.project))
	if err != nil {
		return nil, err
	}
	for _, match := range cloudAttachmentReference.FindAllStringSubmatch(body, -1) {
		refs = append(refs, attachment.Reference{URL: "attachment:" + match[1], ID: match[1]})
	}
	return append(refs, attachment.References(body, string(scope.organization), string(scope.project))...), nil
}

func (s *Service) registerCloudAttachmentRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	s.registerAttachmentArchiveRoutes(e)
	read := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	write := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	e.POST(attachmentMetadataBase+"/check", s.checkCloudAttachment, write)
	e.POST(attachmentMetadataBase, s.recordCloudAttachment, write)
	e.POST(hubapi.NativeBase+"/attempts/:attempt/evidence/check", s.checkCloudAttachment, s.requireNativeScope(hubapi.APIScopeWorker))
	e.POST(hubapi.NativeBase+"/attempts/:attempt/evidence", s.recordCloudAttachment, s.requireNativeScope(hubapi.APIScopeWorker))
	e.GET(attachmentMetadataBase+"/:attachment", s.getCloudAttachment, read)
	e.DELETE(attachmentMetadataBase+"/:attachment", s.deleteCloudAttachment, s.requireNativeScope(hubapi.APIScopeOperator))
	e.POST(attachmentMetadataBase+"/:attachment/reference", s.referenceCloudAttachment, write)
	e.POST("/internal/v1/attachments/expired", s.expiredCloudAttachments)
	e.POST("/internal/v1/attachments/deleted", s.finishCloudAttachmentDeletion)
	e.POST("/internal/v1/attachments/exists", s.cloudAttachmentExists)
}

func (s *Service) checkCloudAttachment(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	var input struct {
		Size     int64                       `json:"size"`
		Upload   *attachment.UploadRequest   `json:"upload,omitempty"`
		Evidence *attachment.EvidenceRequest `json:"evidence,omitempty"`
	}
	if err := c.Bind(&input); err != nil || input.Size < 0 || input.Size > attachment.MaxBytes {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid attachment size"))
	}
	if input.Upload != nil {
		return s.prepareCloudAttachment(c, *input.Upload)
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}

	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	scope := nativeRequestScope(c)
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		limit, err := s.attachmentQuotaLimit(ctx, r)
		if err != nil {
			return err
		}
		used, err := r.CollaborationBytes(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.AttachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if input.Size > limit-used {
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}

	return c.JSON(http.StatusOK, map[string]string{"principal_id": nativeRequestScope(c).credential.ID})
}

func (s *Service) prepareCloudAttachment(c echo.Context, input attachment.UploadRequest) error {
	return s.prepareCloudAttachmentTenant(c, input)
}

func (s *Service) recordCloudAttachment(c echo.Context) error {
	var input attachment.UploadRequest
	if err := c.Bind(&input); err != nil || input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid attachment metadata"))
	}
	return s.recordCloudAttachmentTenant(c, input)
}

func (s *Service) getCloudAttachment(c echo.Context) error {
	scope := nativeRequestScope(c)
	record, err := s.readCloudAttachment(c.Request().Context(), scope, c.Param("attachment"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	record.AuthorizedPrincipal = scope.credential.ID
	return c.JSON(http.StatusOK, record)
}

func (s *Service) readCloudAttachment(ctx context.Context, scope nativeScope, id string) (attachment.Metadata, error) {
	return s.readAttachmentRecordTenant(ctx, scope, id)
}

func (s *Service) deleteCloudAttachment(c echo.Context) error {
	scope := nativeRequestScope(c)
	if err := s.deleteCloudAttachmentCommand(c.Request().Context(), scope, c.Param("attachment")); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) deleteCloudAttachmentCommand(ctx context.Context, scope nativeScope, id string) error {
	if scope.credential.Scope == hubapi.APIScopeWorker {
		return hubapi.NativeNotFound()
	}
	return s.deleteCloudAttachmentTenant(ctx, scope, id)
}

func (s *Service) referenceCloudAttachment(c echo.Context) error {
	var input struct {
		WorkItemID string `json:"work_item_id"`
		CommentID  string `json:"comment_id"`
	}
	if err := c.Bind(&input); err != nil || input.WorkItemID == "" {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("A work item is required"))
	}
	if err := s.referenceCloudAttachmentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("attachment"), attachment.SourceReference{WorkItemID: input.WorkItemID, CommentID: input.CommentID}); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) referenceCloudAttachmentCommand(ctx context.Context, scope nativeScope, id string, input attachment.SourceReference) error {
	return s.referenceCloudAttachmentTenant(ctx, scope, id, input)
}

func (s *Service) expiredCloudAttachments(c echo.Context) error {
	return s.expiredCloudAttachmentsTenant(c)
}

func (s *Service) finishCloudAttachmentDeletion(c echo.Context) error {
	return s.finishCloudAttachmentDeletionTenant(c)
}

func (s *Service) cloudAttachmentExists(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	var input struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := c.Bind(&input); err != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid attachment id"))
	}
	var exists bool
	if input.Key != "" {
		digest := input.Key[strings.LastIndex(input.Key, "/")+1:]
		key, err := attachment.ArchiveKey(claims.Audience, digest)
		if err != nil || key != input.Key {
			return s.nativeAPIError(c, hubapi.NativeInvalid("Invalid archive key"))
		}
	}
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(claims.Audience), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		exists, err = r.AttachmentObjectExists(ctx, input.ID, input.Key)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}

	return c.JSON(http.StatusOK, map[string]bool{"exists": exists})
}
