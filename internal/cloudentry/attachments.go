package cloudentry

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

var errAttachmentReadAudit = errors.New("attachment read audit unavailable")

func (s *Service) registerAttachmentRoutes(e *echo.Echo) {
	for _, base := range []string{"/organizations/:organization/api/v2/projects/:project/attachments", "/api/v2/organizations/:organization/projects/:project/attachments"} {
		e.POST(base, s.uploadAttachment)
		e.GET(base+"/:attachment", s.readAttachment)
		e.GET(base+"/:attachment/metadata", s.readAttachmentMetadata)
		e.DELETE(base+"/:attachment", s.deleteAttachment)
		e.POST(base+"/:attachment/reference", s.referenceAttachment)
	}
}

func attachmentError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]string{"code": code, "message": message})
}

func attachmentNotFound(c echo.Context) error {
	return attachmentError(c, http.StatusNotFound, "not_found", "Resource was not found")
}

func (s *Service) attachmentCall(c echo.Context, organization Organization, method, suffix string, payload any) (int, []byte, error) {
	if !safeID(c.Param("project")) {
		return http.StatusNotFound, nil, nil
	}
	path := "/api/v2/organizations/" + organization.ID + "/projects/" + c.Param("project") + "/attachment-metadata" + suffix
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
	}
	claims, err := s.claims(organization, cloudassert.KindMachine, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	var bearer, csrf string
	if header := c.Request().Header.Get(echo.HeaderAuthorization); header != "" {
		var ok bool
		bearer, ok = strings.CutPrefix(header, "Bearer ")
		if !ok || bearer == "" {
			return http.StatusNotFound, nil, nil
		}
	} else {
		status, _, err := s.browserClaims(c, organization, &claims)
		if err != nil {
			return status, nil, nil
		}
		if method != http.MethodGet {
			csrf = c.Request().Header.Get("X-CSRF-Token")
			if csrf == "" || subtle.ConstantTimeCompare([]byte(csrf), []byte(claims.CSRF)) != 1 {
				return http.StatusForbidden, nil, nil
			}
		}
	}
	return s.signedCallCSRF(c.Request().Context(), organization, claims, body, bearer, csrf)
}

func attachmentCallError(c echo.Context, status int, body []byte, err error, hide bool) error {
	if err != nil {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "The organization is temporarily unavailable")
	}
	if hide || len(body) == 0 {
		return attachmentNotFound(c)
	}
	return c.JSONBlob(status, body)
}

func (s *Service) uploadAttachment(c echo.Context) error {
	if s.attachments == nil {
		return attachmentError(c, http.StatusServiceUnavailable, "attachments_not_configured", "Attachments not configured")
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return attachmentNotFound(c)
	}
	status, raw, err := s.attachmentCall(c, organization, http.MethodPost, "/check", map[string]int64{"size": 0})
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, false)
	}
	var authorized struct {
		Principal string `json:"principal_id"`
	}
	if err := json.Unmarshal(raw, &authorized); err != nil || authorized.Principal == "" {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Attachment authorization failed")
	}
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, attachment.MaxBytes+(1<<20))
	var reader io.Reader = request.Body
	scratch := s.config.StateDir
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		if provided := os.Getenv(variable); provided != "" {
			scratch = provided
			break
		}
	}
	name, media := request.Header.Get("X-Attachment-Name"), request.Header.Get(echo.HeaderContentType)
	if strings.HasPrefix(media, "multipart/form-data") {
		parts, err := request.MultipartReader()
		if err != nil {
			return attachmentError(c, http.StatusBadRequest, "invalid_attachment", "A file is required")
		}
		part, err := parts.NextPart()
		if err != nil || part.FormName() != "file" {
			return attachmentError(c, http.StatusBadRequest, "invalid_attachment", "One file part is required")
		}
		reader, name, media = part, part.FileName(), part.Header.Get(echo.HeaderContentType)
		upload, err := attachment.Prepare(reader, name, media, scratch)
		if err != nil {
			return attachmentUploadError(c, err)
		}
		defer s.closeAttachmentUpload(upload)
		if _, err := parts.NextPart(); !errors.Is(err, io.EOF) {
			return attachmentError(c, http.StatusBadRequest, "invalid_attachment", "An upload carries one file part")
		}
		return s.storeAttachment(c, organization, authorized.Principal, upload)
	}
	upload, err := attachment.Prepare(reader, name, media, scratch)
	if err != nil {
		return attachmentUploadError(c, err)
	}
	defer s.closeAttachmentUpload(upload)
	return s.storeAttachment(c, organization, authorized.Principal, upload)
}

func attachmentUploadError(c echo.Context, err error) error {
	var capError *http.MaxBytesError
	if errors.Is(err, attachment.ErrTooLarge) || errors.As(err, &capError) {
		return attachmentError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "A file must be at most 20 MiB")
	}
	if errors.Is(err, attachment.ErrInvalid) {
		return attachmentError(c, http.StatusBadRequest, "invalid_attachment", "The attachment type, content, name or dimensions are invalid")
	}
	return attachmentError(c, http.StatusServiceUnavailable, "attachment_unavailable", "Attachment could not be prepared")
}

func (s *Service) closeAttachmentUpload(upload *attachment.Upload) {
	if err := upload.Close(); err != nil {
		s.config.Logger.Warn("attachment scratch cleanup failed", "attachment", upload.ID)
	}
}

func (s *Service) storeAttachment(c echo.Context, organization Organization, principal string, upload *attachment.Upload) error {
	s.attachmentMu.RLock()
	defer s.attachmentMu.RUnlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmentNotFound(c)
	}
	status, raw, err := s.attachmentCall(c, organization, http.MethodPost, "/check", map[string]int64{"size": upload.Size})
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, false)
	}
	key, err := attachment.Key(organization.ID, upload.ID)
	if err != nil {
		return attachmentNotFound(c)
	}
	ctx := c.Request().Context()
	if err := s.auth.audit(ctx, principal, organization.ID, "attachment_uploaded"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err := s.attachments.Put(ctx, key, upload.File, upload.Size, upload.ContentType, upload.SHA256); err != nil {
		return attachmentError(c, http.StatusBadGateway, "attachment_storage_unavailable", "Attachment storage is unavailable")
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, "", upload.Metadata)
	if err == nil && status == http.StatusCreated {
		return attachmentUploadResponse(c, organization.ID, status, raw)
	}
	if err != nil || status >= http.StatusInternalServerError {
		readStatus, recorded, readErr := s.attachmentCall(c, organization, http.MethodGet, "/"+upload.ID, nil)
		if readErr == nil && readStatus == http.StatusOK {
			return attachmentUploadResponse(c, organization.ID, http.StatusCreated, recorded)
		}
		if readErr != nil || readStatus != http.StatusNotFound {
			return attachmentCallError(c, status, raw, err, false)
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := s.auth.audit(cleanup, principal, organization.ID, "attachment_deleted"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err := s.attachments.Delete(cleanup, key); err != nil {
		return attachmentError(c, http.StatusBadGateway, "attachment_storage_unavailable", "Attachment cleanup failed")
	}
	return attachmentCallError(c, status, raw, err, false)
}

func (s *Service) attachmentMetadata(c echo.Context) (Organization, attachment.Metadata, int, []byte, error) {
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return Organization{}, attachment.Metadata{}, http.StatusNotFound, nil, nil
	}
	if _, err := attachment.Key(organization.ID, c.Param("attachment")); err != nil {
		return Organization{}, attachment.Metadata{}, http.StatusNotFound, nil, nil
	}
	status, raw, err := s.attachmentCall(c, organization, http.MethodGet, "/"+c.Param("attachment"), nil)
	if err != nil || status != http.StatusOK {
		return organization, attachment.Metadata{}, status, raw, err
	}
	var record attachment.Metadata
	if err := json.Unmarshal(raw, &record); err != nil || record.ID != c.Param("attachment") || record.ProjectID != c.Param("project") || record.DeletedAt != nil || record.Validate() != nil || record.AuthorizedPrincipal == "" {
		return organization, record, http.StatusNotFound, nil, nil
	}
	return organization, record, status, raw, nil
}

func (s *Service) readAttachmentMetadata(c echo.Context) error {
	if s.attachments == nil {
		return attachmentNotFound(c)
	}
	_, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, true)
	}
	if err := s.auth.audit(c.Request().Context(), record.AuthorizedPrincipal, c.Param("organization"), "attachment_read"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	record.AuthorizedPrincipal = ""
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, record)
}

func (s *Service) readAttachment(c echo.Context) error {
	if s.attachments == nil {
		return attachmentNotFound(c)
	}
	organization, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, true)
	}
	body, err := s.openAttachment(c.Request().Context(), organization, record)
	if errors.Is(err, errAttachmentReadAudit) {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err != nil {
		return attachmentNotFound(c)
	}
	defer s.closeAttachmentBody(body, record.ID)
	header := c.Response().Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Set("Cache-Control", "private, max-age=300")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	disposition := "attachment"
	if strings.HasPrefix(record.ContentType, "image/") {
		disposition = "inline"
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": record.Name}))
	return c.Stream(http.StatusOK, record.ContentType, io.LimitReader(body, record.Size))
}

func (s *Service) openAttachment(ctx context.Context, organization Organization, record attachment.Metadata) (io.ReadCloser, error) {
	key, err := attachment.Key(organization.ID, record.ID)
	if err != nil {
		return nil, err
	}
	if err := s.auth.audit(ctx, record.AuthorizedPrincipal, organization.ID, "attachment_read"); err != nil {
		return nil, errors.Join(errAttachmentReadAudit, err)
	}
	return s.attachments.Open(ctx, key)
}

func (s *Service) closeAttachmentBody(body io.ReadCloser, id string) {
	if err := body.Close(); err != nil {
		s.config.Logger.Warn("attachment read close failed", "attachment", id)
	}
}

func (s *Service) deleteAttachment(c echo.Context) error {
	s.attachmentMu.RLock()
	defer s.attachmentMu.RUnlock()
	if s.attachments == nil {
		return attachmentNotFound(c)
	}
	organization, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, true)
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodDelete, "/"+record.ID, nil)
	if err != nil || status != http.StatusNoContent {
		return attachmentCallError(c, status, raw, err, false)
	}
	if err := s.removeAttachment(c.Request().Context(), organization, record, record.AuthorizedPrincipal); err != nil {
		return attachmentError(c, http.StatusBadGateway, "attachment_storage_unavailable", "Attachment deletion is pending")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) referenceAttachment(c echo.Context) error {
	organization, _, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, true)
	}
	var input struct {
		WorkItemID string `json:"work_item_id"`
		CommentID  string `json:"comment_id"`
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
	if err := c.Bind(&input); err != nil {
		return attachmentError(c, http.StatusBadRequest, "invalid_request", "A work item reference is required")
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, "/"+c.Param("attachment")+"/reference", input)
	if err != nil || status != http.StatusNoContent {
		return attachmentCallError(c, status, raw, err, false)
	}
	return c.NoContent(status)
}

func (s *Service) removeAttachment(ctx context.Context, organization Organization, record attachment.Metadata, principal string) error {
	key, err := attachment.Key(organization.ID, record.ID)
	if err != nil {
		return err
	}
	if err := s.auth.audit(ctx, principal, organization.ID, "attachment_deleted"); err != nil {
		return err
	}
	if err := s.attachments.Delete(ctx, key); err != nil {
		return err
	}
	status, err := s.serviceRequest(ctx, organization, "/internal/v1/attachments/deleted", map[string]string{"id": record.ID}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return errors.New("attachment deletion metadata was refused")
	}
	return nil
}

func (s *Service) sweepAttachments(ctx context.Context) {
	if s.attachments == nil || s.config.now().Before(s.attachmentSweepAt) {
		return
	}
	s.attachmentSweepAt = s.config.now().Add(time.Hour)
	organizations, err := s.registry.List(ctx)
	if err != nil {
		s.config.Logger.Warn("attachment retention registry unavailable")
		return
	}
	for _, organization := range organizations {
		if organization.State != "ready" {
			continue
		}
		if err := s.sweepOrganizationAttachments(ctx, organization); err != nil {
			s.config.Logger.Warn("attachment retention failed", "organization", organization.ID)
		}
	}
}

func (s *Service) sweepOrganizationAttachments(ctx context.Context, organization Organization) error {
	s.attachmentMu.RLock()
	defer s.attachmentMu.RUnlock()
	status, raw, err := s.serviceCall(ctx, organization, "/internal/v1/attachments/expired", struct{}{}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errors.New("attachment retention metadata unavailable")
	}
	var records []attachment.Metadata
	if err := json.Unmarshal(raw, &records); err != nil {
		return err
	}
	for _, record := range records {
		if err := s.removeAttachment(ctx, organization, record, "entry"); err != nil {
			return err
		}
	}
	prefix, err := attachment.OrganizationPrefix(organization.ID)
	if err != nil {
		return err
	}
	prefix += "attachments/"
	return s.attachments.Walk(ctx, prefix, func(object attachment.Object) error {
		if object.CreatedAt.After(s.config.now().Add(-attachment.OrphanTTL)) {
			return nil
		}
		id := strings.TrimPrefix(object.Key, prefix)
		key, err := attachment.Key(organization.ID, id)
		if err != nil || key != object.Key {
			return attachment.ErrInvalid
		}
		status, raw, err := s.serviceCall(ctx, organization, "/internal/v1/attachments/exists", map[string]string{"id": id}, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return errors.New("attachment existence metadata unavailable")
		}
		var result struct {
			Exists *bool `json:"exists"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if result.Exists == nil {
			return errors.New("attachment existence metadata missing")
		}
		if *result.Exists {
			return nil
		}
		if err := s.auth.audit(ctx, "entry", organization.ID, "attachment_deleted"); err != nil {
			return err
		}
		return s.attachments.Delete(ctx, key)
	})
}

func attachmentUploadResponse(c echo.Context, organization string, status int, raw []byte) error {
	var record attachment.Metadata
	if err := json.Unmarshal(raw, &record); err != nil || record.Validate() != nil {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Attachment metadata is invalid")
	}
	record.Reference = record.Markdown(organization)
	record.AuthorizedPrincipal = ""
	return c.JSON(status, record)
}
