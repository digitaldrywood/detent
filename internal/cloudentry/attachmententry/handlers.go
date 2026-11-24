package attachmententry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type Organization = tenant.Organization

type Service struct {
	Lookup         func(context.Context, string) (Organization, error)
	ArchiveStorage func(context.Context, string, attachment.ArchiveReference) (attachment.ArchiveStorage, error)
	Storage        func() attachment.Storage
	Ready          func(context.Context, string) (Organization, error)
	Call           func(echo.Context, Organization, string, string, any) (int, []byte, error)
	ServiceCall    func(context.Context, Organization, string, any, func(*cloudassert.Claims)) (int, []byte, error)
	ServiceRequest func(context.Context, Organization, string, any, func(*cloudassert.Claims)) (int, error)
	Audit          func(context.Context, string, string, string) error
	Lock           func(string, bool) func()
	Now            func() time.Time
	Logger         *slog.Logger
	StateDir       string
	Echo           *echo.Echo
}

var errAttachmentReadAudit = errors.New("attachment read audit unavailable")

func attachmentEvidenceRequest(c echo.Context) (*attachment.EvidenceRequest, bool, error) {
	encoded := c.Request().Header.Get("X-Detent-Evidence")
	if c.Param("attempt") == "" {
		if encoded != "" {
			return nil, false, attachment.ErrInvalid
		}
		return nil, false, nil
	}
	if len(encoded) > 4096 {
		return nil, false, attachment.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false, err
	}
	var evidence attachment.EvidenceRequest
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, false, err
	}
	if evidence.AttemptID != c.Param("attempt") || evidence.IdempotencyKey == "" || evidence.IdempotencyKey != c.Request().Header.Get("Idempotency-Key") {
		return nil, false, attachment.ErrInvalid
	}
	return &evidence, true, nil
}

func (s *Service) Register(e *echo.Echo, archives func(*echo.Echo, string)) {
	e.GET("/organizations/:organization/api/v2/logo-image", s.readOrganizationLogoImage)
	for _, base := range []string{"/organizations/:organization/api/v2/attachments", "/api/v2/organizations/:organization/attachments"} {
		e.GET(base+"/:attachment", s.readAttachment)
	}
	for _, base := range []string{"/organizations/:organization/api/v2/projects/:project/attachments", "/api/v2/organizations/:organization/projects/:project/attachments"} {
		archives(e, base)
		e.POST(base, s.uploadAttachment)
		e.POST(strings.TrimSuffix(base, "/attachments")+"/attempts/:attempt/evidence", s.uploadAttachment)
		e.GET(base+"/:attachment", s.readAttachment)
		e.GET(base+"/:attachment/metadata", s.readAttachmentMetadata)
		e.DELETE(base+"/:attachment", s.deleteAttachment)
		e.POST(base+"/:attachment/reference", s.referenceAttachment)
	}
}

func Error(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]string{"code": code, "message": message})
}

func NotFound(c echo.Context) error {
	return Error(c, http.StatusNotFound, "not_found", "Resource was not found")
}

func CallError(c echo.Context, status int, body []byte, err error, hide bool) error {
	if err != nil {
		return Error(c, http.StatusBadGateway, "tenant_unavailable", "The organization is temporarily unavailable")
	}
	if hide || len(body) == 0 {
		return NotFound(c)
	}
	return c.JSONBlob(status, body)
}

func (s *Service) uploadAttachment(c echo.Context) error {
	evidence, _, err := attachmentEvidenceRequest(c)
	if err != nil {
		return Error(c, http.StatusBadRequest, "invalid_attachment", "Invalid attempt evidence authority")
	}
	var resource *attachment.Upload
	if audit, ok := mutation.FromContext(c.Request().Context()); ok && audit.Source == "mcp" {
		defer func() {
			audit.RetryIdentity, audit.InputHash = "", ""
			if resource != nil {
				audit.ResourceID = resource.ID
			}
			outcome := "failed"
			if c.Response().Status == http.StatusCreated {
				outcome = "succeeded"
			}
			encoded, err := json.Marshal(struct {
				mutation.Metadata
				Outcome string `json:"outcome"`
			}{audit, outcome})
			if err == nil {
				s.Logger.InfoContext(c.Request().Context(), "operator mutation", "audit", string(encoded))
			}
		}()
	}
	if s.Storage() == nil {
		return Error(c, http.StatusServiceUnavailable, "attachments_not_configured", "Attachments not configured")
	}
	organization, err := s.Ready(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return NotFound(c)
	}
	status, raw, err := s.Call(c, organization, http.MethodPost, "/check", map[string]any{"size": 0, "evidence": evidence})
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, false)
	}
	var authorized struct {
		Principal string `json:"principal_id"`
	}
	if err := json.Unmarshal(raw, &authorized); err != nil || authorized.Principal == "" {
		return Error(c, http.StatusBadGateway, "tenant_unavailable", "Attachment authorization failed")
	}
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, attachment.MaxBytes+(1<<20))
	var reader io.Reader = request.Body
	scratch := s.StateDir
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
			return Error(c, http.StatusBadRequest, "invalid_attachment", "A file is required")
		}
		part, err := parts.NextPart()
		if err != nil || part.FormName() != "file" {
			return Error(c, http.StatusBadRequest, "invalid_attachment", "One file part is required")
		}
		reader, name, media = part, part.FileName(), part.Header.Get(echo.HeaderContentType)
		upload, err := attachment.Prepare(reader, name, media, scratch)
		if err != nil {
			return UploadError(c, err)
		}
		resource = upload
		defer s.closeAttachmentUpload(upload)
		if _, err := parts.NextPart(); !errors.Is(err, io.EOF) {
			return Error(c, http.StatusBadRequest, "invalid_attachment", "An upload carries one file part")
		}
		return s.storeAttachment(c, organization, authorized.Principal, upload)
	}
	upload, err := attachment.Prepare(reader, name, media, scratch)
	if err != nil {
		return UploadError(c, err)
	}
	resource = upload
	defer s.closeAttachmentUpload(upload)
	return s.storeAttachment(c, organization, authorized.Principal, upload)
}

func UploadError(c echo.Context, err error) error {
	var capError *http.MaxBytesError
	if errors.Is(err, attachment.ErrTooLarge) || errors.As(err, &capError) {
		return Error(c, http.StatusRequestEntityTooLarge, "payload_too_large", "A file must be at most 20 MiB")
	}
	if errors.Is(err, attachment.ErrInvalid) {
		return Error(c, http.StatusBadRequest, "invalid_attachment", "The attachment type, content, name or dimensions are invalid")
	}
	return Error(c, http.StatusServiceUnavailable, "attachment_unavailable", "Attachment could not be prepared")
}

func (s *Service) closeAttachmentUpload(upload *attachment.Upload) {
	if err := upload.Close(); err != nil {
		s.Logger.Warn("attachment scratch cleanup failed", "attachment", upload.ID)
	}
}

func (s *Service) storeAttachment(c echo.Context, organization Organization, principal string, upload *attachment.Upload) error {
	evidence, _, err := attachmentEvidenceRequest(c)
	if err != nil {
		return Error(c, http.StatusBadRequest, "invalid_attachment", "Invalid attempt evidence authority")
	}
	unlock := s.Lock(organization.ID, false)
	defer unlock()
	current, err := s.Ready(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return NotFound(c)
	}
	requestID := c.Request().Header.Get("Idempotency-Key")
	if requestID != "" {
		status, raw, err := s.Call(c, organization, http.MethodPost, "/check", map[string]any{"upload": attachment.UploadRequest{Metadata: upload.Metadata, RequestID: requestID, Evidence: evidence}})
		if err != nil || status != http.StatusOK {
			return CallError(c, status, raw, err, false)
		}
		if json.Unmarshal(raw, &upload.Metadata) != nil || upload.Validate() != nil {
			return Error(c, http.StatusBadGateway, "tenant_unavailable", "Attachment receipt is invalid")
		}
		status, raw, err = s.Call(c, organization, http.MethodGet, "/"+upload.ID, nil)
		if err == nil && status == http.StatusOK {
			return attachmentUploadResponse(c, organization.ID, http.StatusCreated, raw)
		}
		if err != nil || status != http.StatusNotFound {
			return CallError(c, status, raw, err, false)
		}
	}
	status, raw, err := s.Call(c, organization, http.MethodPost, "/check", map[string]any{"size": upload.Size, "evidence": evidence})
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, false)
	}
	key, err := attachment.Key(organization.ID, upload.ID)
	if err != nil {
		return NotFound(c)
	}
	ctx := c.Request().Context()
	if err := s.Audit(ctx, principal, organization.ID, "attachment_uploaded"); err != nil {
		return Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err := s.Storage().Put(ctx, key, upload.File, upload.Size, upload.ContentType, upload.SHA256); err != nil && (requestID == "" || !s.attachmentUploadStored(ctx, key, upload.Metadata)) {
		return Error(c, http.StatusBadGateway, "attachment_storage_unavailable", "Attachment storage is unavailable")
	}
	status, raw, err = s.Call(c, organization, http.MethodPost, "", attachment.UploadRequest{Metadata: upload.Metadata, RequestID: requestID, Evidence: evidence})
	if err == nil && status == http.StatusCreated {
		return attachmentUploadResponse(c, organization.ID, status, raw)
	}
	if err != nil || status >= http.StatusInternalServerError {
		readStatus, recorded, readErr := s.Call(c, organization, http.MethodGet, "/"+upload.ID, nil)
		if readErr == nil && readStatus == http.StatusOK {
			return attachmentUploadResponse(c, organization.ID, http.StatusCreated, recorded)
		}
		if readErr != nil || readStatus != http.StatusNotFound {
			return CallError(c, status, raw, err, false)
		}
	}
	if requestID != "" {
		return CallError(c, status, raw, err, false)
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := s.Audit(cleanup, principal, organization.ID, "attachment_deleted"); err != nil {
		return Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err := s.Storage().Delete(cleanup, key); err != nil {
		return Error(c, http.StatusBadGateway, "attachment_storage_unavailable", "Attachment cleanup failed")
	}
	return CallError(c, status, raw, err, false)
}

func (s *Service) attachmentUploadStored(ctx context.Context, key string, record attachment.Metadata) bool {
	body, err := s.Storage().Open(ctx, key)
	if err != nil {
		return false
	}
	defer s.closeAttachmentBody(body, record.ID)
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(body, record.Size+1))
	return err == nil && size == record.Size && hex.EncodeToString(hash.Sum(nil)) == record.SHA256
}

func (s *Service) attachmentMetadata(c echo.Context) (Organization, attachment.Metadata, int, []byte, error) {
	organization, err := s.Ready(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return Organization{}, attachment.Metadata{}, http.StatusNotFound, nil, nil
	}
	if _, err := attachment.Key(organization.ID, c.Param("attachment")); err != nil {
		return Organization{}, attachment.Metadata{}, http.StatusNotFound, nil, nil
	}
	status, raw, err := s.Call(c, organization, http.MethodGet, "/"+c.Param("attachment"), nil)
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
	if s.Storage() == nil {
		return NotFound(c)
	}
	_, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, true)
	}
	if err := s.Audit(c.Request().Context(), record.AuthorizedPrincipal, c.Param("organization"), "attachment_read"); err != nil {
		return Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	record.AuthorizedPrincipal = ""
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, record)
}

func (s *Service) readAttachment(c echo.Context) error {
	if c.QueryParams().Has("offset") || c.QueryParams().Has("length") {
		input := map[string]any{"project_id": c.Param("project"), "attachment_id": c.Param("attachment")}
		for _, field := range []string{"offset", "length"} {
			if values, present := c.QueryParams()[field]; present {
				if len(values) != 1 {
					return Error(c, http.StatusBadRequest, "invalid_request", "Invalid attachment read bounds")
				}
				input[field] = json.Number(values[0])
			}
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return Error(c, http.StatusBadRequest, "invalid_request", "Invalid attachment read bounds")
		}
		bounds, err := operatortool.DecodeAttachmentOperation(operatortool.ReadAttachment, raw)
		if err != nil {
			return Error(c, http.StatusBadRequest, "invalid_request", "Invalid attachment read bounds")
		}
		return s.readAttachmentContent(c, bounds)
	}
	if s.Storage() == nil {
		return NotFound(c)
	}
	organization, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, true)
	}
	return s.streamAttachment(c, organization, record)
}

func (s *Service) readOrganizationLogoImage(c echo.Context) error {
	if s.Storage() == nil {
		return NotFound(c)
	}
	organization, err := s.Ready(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return NotFound(c)
	}
	status, raw, err := s.Call(c, organization, http.MethodGet, "/current-logo", nil)
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, true)
	}
	var record attachment.Metadata
	if json.Unmarshal(raw, &record) != nil || record.ProjectID != "" || record.DeletedAt != nil || record.Validate() != nil || record.AuthorizedPrincipal == "" {
		return NotFound(c)
	}
	return s.streamAttachment(c, organization, record)
}

func (s *Service) streamAttachment(c echo.Context, organization Organization, record attachment.Metadata) error {
	body, err := s.openAttachment(c.Request().Context(), organization, record)
	if errors.Is(err, errAttachmentReadAudit) {
		return Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err != nil {
		return NotFound(c)
	}
	defer s.closeAttachmentBody(body, record.ID)
	header := c.Response().Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Set("Cache-Control", "private, max-age=300")
	if c.Path() == "/organizations/:organization/api/v2/logo-image" {
		header.Set("Cache-Control", "private, no-store")
	}
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
	if err := s.Audit(ctx, record.AuthorizedPrincipal, organization.ID, "attachment_read"); err != nil {
		return nil, errors.Join(errAttachmentReadAudit, err)
	}
	return s.Storage().Open(ctx, key)
}

func (s *Service) closeAttachmentBody(body io.ReadCloser, id string) {
	if err := body.Close(); err != nil {
		s.Logger.Warn("attachment read close failed", "attachment", id)
	}
}

func (s *Service) deleteAttachment(c echo.Context) error {
	unlock := s.Lock(c.Param("organization"), false)
	defer unlock()
	if s.Storage() == nil {
		return NotFound(c)
	}
	organization, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, true)
	}
	status, raw, err = s.Call(c, organization, http.MethodDelete, "/"+record.ID, nil)
	if err != nil || status != http.StatusNoContent {
		return CallError(c, status, raw, err, false)
	}
	if err := s.removeAttachment(c.Request().Context(), organization, record, record.AuthorizedPrincipal); err != nil {
		return Error(c, http.StatusBadGateway, "attachment_storage_unavailable", "Attachment deletion is pending")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) referenceAttachment(c echo.Context) error {
	organization, _, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, true)
	}
	var input struct {
		WorkItemID string `json:"work_item_id"`
		CommentID  string `json:"comment_id"`
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
	if err := c.Bind(&input); err != nil {
		return Error(c, http.StatusBadRequest, "invalid_request", "A work item reference is required")
	}
	status, raw, err = s.Call(c, organization, http.MethodPost, "/"+c.Param("attachment")+"/reference", input)
	if err != nil || status != http.StatusNoContent {
		return CallError(c, status, raw, err, false)
	}
	return c.NoContent(status)
}

func (s *Service) removeAttachment(ctx context.Context, organization Organization, record attachment.Metadata, principal string) error {
	key, err := attachment.Key(organization.ID, record.ID)
	if err != nil {
		return err
	}
	if err := s.Audit(ctx, principal, organization.ID, "attachment_deleted"); err != nil {
		return err
	}
	if err := s.Storage().Delete(ctx, key); err != nil {
		return err
	}
	status, err := s.ServiceRequest(ctx, organization, "/internal/v1/attachments/deleted", map[string]string{"id": record.ID}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return errors.New("attachment deletion metadata was refused")
	}
	return nil
}

func (s *Service) SweepExpired(ctx context.Context, organization Organization, limit int) (int, error) {
	status, raw, err := s.ServiceCall(ctx, organization, "/internal/v1/attachments/expired", struct {
		Limit int `json:"limit"`
	}{limit}, nil)
	if err != nil {
		return limit, err
	}
	if status != http.StatusOK {
		return limit, errors.New("attachment retention metadata unavailable")
	}
	var page attachment.ExpiryPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return limit, err
	}
	if page.Examined > limit || page.Examined < len(page.Records) {
		return limit, errors.New("attachment retention exceeded its page")
	}
	for _, record := range page.Records {
		unlock := s.Lock(organization.ID, true)
		err := s.removeAttachment(ctx, organization, record, "entry")
		unlock()
		if err != nil {
			return page.Examined, err
		}
	}
	return page.Examined, nil
}

func attachmentUploadResponse(c echo.Context, organization string, status int, raw []byte) error {
	var record attachment.Metadata
	if err := json.Unmarshal(raw, &record); err != nil || record.Validate() != nil {
		return Error(c, http.StatusBadGateway, "tenant_unavailable", "Attachment metadata is invalid")
	}
	record.Reference = record.Markdown(organization)
	record.AuthorizedPrincipal = ""
	return c.JSON(status, record)
}
