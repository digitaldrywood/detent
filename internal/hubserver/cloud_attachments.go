package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
)

const attachmentMetadataBase = nativeBase + "/attachment-metadata"

var cloudAttachmentReference = regexp.MustCompile(`\]\(\s*<?attachment:(att_[A-Za-z0-9_-]+)>?(?:\s+"[^"]*")?\s*\)`)

func syncCloudAttachmentReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, itemID, commentID, body string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_references WHERE work_item_id=? AND comment_id=? AND attachment_id IN (SELECT id FROM attachments WHERE organization_id=? AND project_id=?)`, itemID, commentID, scope.organization, scope.project); err != nil {
		return err
	}
	var ids []string
	for _, match := range cloudAttachmentReference.FindAllStringSubmatch(body, -1) {
		ids = append(ids, match[1])
	}
	for _, reference := range attachment.References(body, string(scope.organization), string(scope.project)) {
		ids = append(ids, reference.ID)
	}
	for _, id := range ids {
		if conversation.ValidateAttachmentID(id) != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attachment_references(attachment_id,work_item_id,comment_id) SELECT id,?,? FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL`, itemID, commentID, scope.organization, scope.project, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) registerCloudAttachmentRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	write := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	e.POST(attachmentMetadataBase+"/check", s.checkCloudAttachment, write)
	e.POST(attachmentMetadataBase, s.recordCloudAttachment, write)
	e.POST(nativeBase+"/attempts/:attempt/evidence/check", s.checkCloudAttachment, s.requireNativeScope(apiScopeWorker))
	e.POST(nativeBase+"/attempts/:attempt/evidence", s.recordCloudAttachment, s.requireNativeScope(apiScopeWorker))
	e.GET(attachmentMetadataBase+"/:attachment", s.getCloudAttachment, read)
	e.DELETE(attachmentMetadataBase+"/:attachment", s.deleteCloudAttachment, write)
	e.POST(attachmentMetadataBase+"/:attachment/reference", s.referenceCloudAttachment, write)
	e.POST("/internal/v1/attachments/expired", s.expiredCloudAttachments)
	e.POST("/internal/v1/attachments/deleted", s.finishCloudAttachmentDeletion)
	e.POST("/internal/v1/attachments/exists", s.cloudAttachmentExists)
}

func (s *Service) checkCloudAttachment(c echo.Context) error {
	var input struct {
		Size     int64                       `json:"size"`
		Upload   *attachment.UploadRequest   `json:"upload,omitempty"`
		Evidence *attachment.EvidenceRequest `json:"evidence,omitempty"`
	}
	if err := c.Bind(&input); err != nil || input.Size < 0 || input.Size > attachment.MaxBytes {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment size"))
	}
	if input.Upload != nil {
		return s.prepareCloudAttachment(c, *input.Upload)
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}

	now := s.config.now()
	used, err := s.database.hostedConsumption(c.Request().Context(), s.database.db, now, "collaboration_bytes")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if s.database.hostedPlans != nil {
		entitlement, err := s.database.hostedEntitlement(c.Request().Context(), s.database.db, now)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if limit, limited := entitlement.Allowances["collaboration_bytes"]; limited && input.Size > limit-used["collaboration_bytes"] {
			return s.nativeAPIError(c, &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used["collaboration_bytes"]})
		}
	}
	return c.JSON(http.StatusOK, map[string]string{"principal_id": nativeRequestScope(c).credential.ID})
}

func (s *Service) prepareCloudAttachment(c echo.Context, input attachment.UploadRequest) error {
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	input.ID = conversation.NewAttachmentID()
	if input.Validate() != nil || strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment upload"))
	}
	scope := nativeRequestScope(c)
	identity := input.Metadata
	identity.ID, identity.ProjectID, identity.Uploader = "", "", ""
	identity.CreatedAt = time.Time{}
	options, command := attachmentEvidenceCommand(scope, "attachment.prepare", input)
	var requestIdentity any = identity
	if input.Evidence != nil {
		if input.Evidence.IdempotencyKey != input.RequestID {
			return s.nativeAPIError(c, nativeInvalid("Evidence retry identity does not match"))
		}
		requestIdentity = struct {
			Metadata attachment.Metadata
			Evidence *attachment.EvidenceRequest
		}{identity, input.Evidence}
	}
	result, err := s.executeNativeMutation(c.Request().Context(), scope, options, command, requestIdentity, func(_ context.Context, _ *sql.Tx, _ nativeScope, _ time.Time) (any, error) {
		return input.Metadata, nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var prepared attachment.Metadata
	if json.Unmarshal(result, &prepared) != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment receipt"))
	}
	var deleted bool
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NOT NULL)", scope.organization, scope.project, prepared.ID).Scan(&deleted); err != nil {
		return s.nativeAPIError(c, err)
	}
	if deleted {
		return s.nativeAPIError(c, &nativeError{Code: "idempotency_conflict", Message: "The uploaded attachment was deleted", status: http.StatusConflict})
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) recordCloudAttachment(c echo.Context) error {
	var input attachment.UploadRequest
	if err := c.Bind(&input); err != nil || input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment metadata"))
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	key := input.RequestID
	if key == "" {
		key = input.ID
	}
	scope := nativeRequestScope(c)
	input.RequestID = key
	options, command := attachmentEvidenceCommand(scope, "attachment.record", input)
	var requestIdentity any = input.Metadata
	if input.Evidence != nil {
		if input.Evidence.IdempotencyKey != input.RequestID {
			return s.nativeAPIError(c, nativeInvalid("Evidence retry identity does not match"))
		}
		requestIdentity = input
	}
	result, err := s.executeNativeMutation(c.Request().Context(), scope, options, command, requestIdentity, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		record := input.Metadata
		before, err := s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes", "usage_windows")
		if err != nil {
			return nil, err
		}
		record.ProjectID, record.Uploader, record.CreatedAt = string(scope.project), scope.credential.ID, now
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,width,height,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.ID, scope.organization, scope.project, record.Uploader, record.Name, record.ContentType, record.Size, record.SHA256, record.Width, record.Height, formatHubTime(now)); err != nil {
			return nil, err
		}
		if err := s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes", "usage_windows"); err != nil {
			return nil, err
		}
		if input.Evidence != nil {
			if err := recordAttachmentEvidence(ctx, tx, scope, *input.Evidence, record, now); err != nil {
				return nil, err
			}
		}
		return record, nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, result)
}

func scanCloudAttachment(row interface{ Scan(...any) error }) (attachment.Metadata, error) {
	var record attachment.Metadata
	var created string
	var deleted sql.NullString
	err := row.Scan(&record.ID, &record.ProjectID, &record.Uploader, &record.Name, &record.ContentType, &record.Size, &record.SHA256, &record.Width, &record.Height, &created, &record.WorkItemID, &record.CommentID, &deleted)
	if err != nil {
		return record, err
	}
	record.CreatedAt, err = parseTimeValue(created)
	if err != nil {
		return record, err
	}
	if deleted.Valid {
		stamp, err := parseTimeValue(deleted.String)
		if err != nil {
			return record, err
		}
		record.DeletedAt = &stamp
	}
	return record, nil
}

const cloudAttachmentColumns = "id,project_id,uploader,name,content_type,size,sha256,width,height,created_at,coalesce(work_item_id,''),coalesce(comment_id,''),deleted_at"

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
	return readCloudAttachment(ctx, s.database.db, scope, id)
}

func readCloudAttachment(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (attachment.Metadata, error) {
	record, err := scanCloudAttachment(query.QueryRowContext(ctx, "SELECT "+cloudAttachmentColumns+" FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL", scope.organization, scope.project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, err
	}
	rows, err := query.QueryContext(ctx, "SELECT work_item_id,comment_id FROM attachment_references WHERE attachment_id=? ORDER BY work_item_id,comment_id", record.ID)
	if err != nil {
		return record, err
	}
	defer rows.Close()
	for rows.Next() {
		var reference attachment.SourceReference
		if err := rows.Scan(&reference.WorkItemID, &reference.CommentID); err != nil {
			return record, err
		}
		record.ReferencedBy = append(record.ReferencedBy, reference)
	}
	if err := rows.Err(); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Service) deleteCloudAttachment(c echo.Context) error {
	scope := nativeRequestScope(c)
	if err := s.deleteCloudAttachmentCommand(c.Request().Context(), scope, c.Param("attachment")); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) deleteCloudAttachmentCommand(ctx context.Context, scope nativeScope, id string) error {
	return s.deleteCloudAttachmentMetadata(ctx, s.database.db, scope, id)
}

func (s *Service) deleteCloudAttachmentMetadata(ctx context.Context, query nativeExecer, scope nativeScope, id string) error {
	result, err := query.ExecContext(ctx, "UPDATE attachments SET deleted_at=coalesce(deleted_at,?) WHERE organization_id=? AND project_id=? AND id=?", formatHubTime(s.config.now()), scope.organization, scope.project, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) referenceCloudAttachment(c echo.Context) error {
	var input struct {
		WorkItemID string `json:"work_item_id"`
		CommentID  string `json:"comment_id"`
	}
	if err := c.Bind(&input); err != nil || input.WorkItemID == "" {
		return s.nativeAPIError(c, nativeInvalid("A work item is required"))
	}
	if err := s.referenceCloudAttachmentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("attachment"), attachment.SourceReference{WorkItemID: input.WorkItemID, CommentID: input.CommentID}); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) referenceCloudAttachmentCommand(ctx context.Context, scope nativeScope, id string, input attachment.SourceReference) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return err
	}
	if _, _, err := readNativeIssue(ctx, tx, scope, input.WorkItemID); err != nil {
		return err
	}
	if input.CommentID != "" {
		if _, err := readNativeComment(ctx, tx, scope, input.WorkItemID, input.CommentID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO attachment_references(attachment_id,work_item_id,comment_id) SELECT id,?,? FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL ON CONFLICT(attachment_id,work_item_id,comment_id) DO UPDATE SET attachment_id=excluded.attachment_id`, input.WorkItemID, input.CommentID, scope.organization, scope.project, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return nativeNotFound()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Service) expiredCloudAttachments(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	ctx := c.Request().Context()
	rows, err := s.database.db.QueryContext(ctx, "SELECT "+cloudAttachmentColumns+" FROM attachments WHERE deleted_at IS NOT NULL AND object_deleted_at IS NULL ORDER BY created_at,id LIMIT 100")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	items := []attachment.Metadata{}
	for rows.Next() {
		item, err := scanCloudAttachment(rows)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, items)
}

func (s *Service) finishCloudAttachmentDeletion(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&input); err != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment id"))
	}
	_, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE attachments SET object_deleted_at=? WHERE id=? AND deleted_at IS NOT NULL", formatHubTime(s.config.now()), input.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) cloudAttachmentExists(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&input); err != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment id"))
	}
	var count int
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM attachments WHERE id=? AND object_deleted_at IS NULL", input.ID).Scan(&count); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"exists": count == 1})
}

func bindCloudAttachmentReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, item, comment, body string) error {
	for _, ref := range attachment.References(body, string(scope.organization), string(scope.project)) {
		result, err := tx.ExecContext(ctx, `UPDATE attachments SET work_item_id=?,comment_id=nullif(?,'') WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL AND (work_item_id IS NULL OR (work_item_id=? AND coalesce(comment_id,'')=?))`, item, comment, scope.organization, scope.project, ref.ID, item, comment)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return nativeNotFound()
		}
	}
	return nil
}
