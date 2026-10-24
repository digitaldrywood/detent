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
	"github.com/digitaldrywood/detent/internal/operatortool"
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
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attachment_references(attachment_id,work_item_id,comment_id) SELECT id,?,? FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND deleted_at IS NULL`, itemID, commentID, scope.organization, scope.project, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) registerCloudAttachmentRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	s.registerAttachmentArchiveRoutes(e)
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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var input struct {
		Size     int64                       `json:"size"`
		Upload   *attachment.UploadRequest   `json:"upload,omitempty"`
		Evidence *attachment.EvidenceRequest `json:"evidence,omitempty"`
	}
	if err := c.Bind(&input); err != nil || input.Size < 0 || input.Size > attachment.MaxBytes {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment size"))
	}
	if input.Upload != nil {
		return s.prepareCloudAttachment(c, *input.Upload)
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}

	now := s.config.now()
	used, err := s.database.hostedConsumption(ctx, s.database.reader, now, "collaboration_bytes")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if s.database.plans() != nil {
		entitlement, err := s.database.hostedEntitlement(ctx, s.database.reader, now)
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
	ctx := c.Request().Context()
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	input.ID = conversation.NewAttachmentID()
	if input.Validate() != nil || strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment upload"))
	}
	scope := nativeRequestScope(c)
	scope.action = operatortool.UploadAttachment
	identity := input.Metadata
	identity.ID, identity.ProjectID, identity.Uploader = "", "", ""
	identity.CreatedAt = time.Time{}
	options, command := attachmentEvidenceCommand(scope, "attachment.prepare", input)
	var requestIdentity any = identity
	if input.Evidence != nil {
		if input.Evidence.IdempotencyKey != input.RequestID {
			return s.nativeAPIError(c, nativePublicInvalid("Evidence retry identity does not match"))
		}
		requestIdentity = struct {
			Metadata attachment.Metadata
			Evidence *attachment.EvidenceRequest
		}{identity, input.Evidence}
	}
	result, err := s.executeNativeMutation(ctx, scope, options, command, requestIdentity, func(_ context.Context, _ *sql.Tx, _ nativeScope, _ time.Time) (any, error) {
		return input.Metadata, nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var prepared attachment.Metadata
	if json.Unmarshal(result, &prepared) != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment receipt"))
	}
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	var deleted bool
	if err := s.database.reader.QueryRowContext(readCtx, "SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NOT NULL)", scope.organization, scope.project, prepared.ID).Scan(&deleted); err != nil {
		return s.nativeAPIError(c, err)
	}
	if deleted {
		return s.nativeAPIError(c, &nativeError{Code: "idempotency_conflict", Message: "The uploaded attachment was deleted", status: http.StatusConflict, publicMessage: true})
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) recordCloudAttachment(c echo.Context) error {
	var input attachment.UploadRequest
	if err := c.Bind(&input); err != nil || input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment metadata"))
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	key := input.RequestID
	if key == "" {
		key = input.ID
	}
	scope := nativeRequestScope(c)
	scope.action = operatortool.UploadAttachment
	input.RequestID = key
	options, command := attachmentEvidenceCommand(scope, "attachment.record", input)
	var requestIdentity any = input.Metadata
	if input.Evidence != nil {
		if input.Evidence.IdempotencyKey != input.RequestID {
			return s.nativeAPIError(c, nativePublicInvalid("Evidence retry identity does not match"))
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
		if scope.credential.HostedRole == "reporter" {
			record.Uploader = scope.actor().PrincipalID
		}
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	return readCloudAttachment(ctx, s.database.reader, scope, id)
}

func readCloudAttachment(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (attachment.Metadata, error) {
	record, err := scanCloudAttachment(query.QueryRowContext(ctx, "SELECT "+cloudAttachmentColumns+" FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND deleted_at IS NULL", scope.organization, scope.project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, err
	}
	if scope.credential.HostedRole == "reporter" {
		visibility, args := scope.issueVisibilitySQL("i")
		args = append([]any{record.ID, scope.organization, scope.project}, args...)
		var visible bool
		if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attachment_references r JOIN issues i ON i.native_id=r.work_item_id WHERE r.attachment_id=? AND i.organization_id=? AND i.project_id=? AND ("+visibility+"))", args...).Scan(&visible); err != nil {
			return attachment.Metadata{}, err
		}
		ownsUpload := record.Uploader == scope.actor().PrincipalID || record.Uploader == scope.credential.ID
		if !visible {
			if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM release_audit_parts WHERE organization_id=? AND project_id=? AND attachment_id=?)", scope.organization, scope.project, record.ID).Scan(&visible); err != nil {
				return attachment.Metadata{}, err
			}
		}
		if !visible && !ownsUpload {
			if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM api_tokens t JOIN hosted_members m ON m.user_id=t.hosted_user_id AND m.membership_id=t.hosted_membership_id WHERE t.id=? AND t.hosted_organization_id=? AND t.key_kind='personal' AND m.principal_id=?)", record.Uploader, scope.organization, scope.actor().PrincipalID).Scan(&ownsUpload); err != nil {
				return attachment.Metadata{}, err
			}
		}
		if !visible && !ownsUpload {
			return attachment.Metadata{}, nativeNotFound()
		}
		if record.WorkItemID != "" {
			if err := requireReporterItem(ctx, query, scope, record.WorkItemID); errors.Is(err, sql.ErrNoRows) || isNativeNotFound(err) {
				record.WorkItemID, record.CommentID = "", ""
			} else if err != nil {
				return attachment.Metadata{}, err
			}
		}
	}
	references := "SELECT work_item_id,comment_id FROM attachment_references WHERE attachment_id=?"
	args := []any{record.ID}
	if scope.credential.HostedRole == "reporter" {
		visibility, visibilityArgs := scope.issueVisibilitySQL("i")
		references += " AND work_item_id IN (SELECT i.native_id FROM issues i WHERE i.organization_id=? AND i.project_id=? AND (" + visibility + "))"
		args = append(args, scope.organization, scope.project)
		args = append(args, visibilityArgs...)
	}
	rows, err := query.QueryContext(ctx, references+" ORDER BY work_item_id,comment_id", args...)
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
	scope.action = operatortool.DeleteAttachment
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return err
	}
	if err := s.deleteCloudAttachmentMetadata(ctx, tx, scope, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) deleteCloudAttachmentMetadata(ctx context.Context, query nativeExecer, scope nativeScope, id string) error {
	result, err := query.ExecContext(ctx, "UPDATE attachments SET deleted_at=coalesce(deleted_at,?) WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment'", formatHubTime(s.config.now()), scope.organization, scope.project, id)
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
		return s.nativeAPIError(c, nativePublicInvalid("A work item is required"))
	}
	if err := s.referenceCloudAttachmentCommand(c.Request().Context(), nativeRequestScope(c), c.Param("attachment"), attachment.SourceReference{WorkItemID: input.WorkItemID, CommentID: input.CommentID}); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) referenceCloudAttachmentCommand(ctx context.Context, scope nativeScope, id string, input attachment.SourceReference) error {
	scope.action = operatortool.ReferenceAttachment
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return err
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, input.WorkItemID)
	if err != nil {
		return err
	}
	if input.CommentID == "" {
		if err := requireReporterTriage(scope, issue); err != nil {
			return err
		}
	}
	if _, err := readCloudAttachment(ctx, tx, scope, id); err != nil {
		return err
	}
	if input.CommentID != "" {
		if _, err := readNativeComment(ctx, tx, scope, input.WorkItemID, input.CommentID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO attachment_references(attachment_id,work_item_id,comment_id) SELECT id,?,? FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND deleted_at IS NULL ON CONFLICT(attachment_id,work_item_id,comment_id) DO UPDATE SET attachment_id=excluded.attachment_id`, input.WorkItemID, input.CommentID, scope.organization, scope.project, id)
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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	rows, err := s.database.reader.QueryContext(ctx, "SELECT "+cloudAttachmentColumns+" FROM attachments WHERE kind='attachment' AND deleted_at IS NOT NULL AND object_deleted_at IS NULL ORDER BY created_at,id LIMIT 100")
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
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment id"))
	}
	_, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE attachments SET object_deleted_at=? WHERE id=? AND deleted_at IS NOT NULL", formatHubTime(s.config.now()), input.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) cloudAttachmentExists(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := c.Bind(&input); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment id"))
	}
	var exists bool
	if input.Key != "" {
		digest := input.Key[strings.LastIndex(input.Key, "/")+1:]
		key, err := attachment.ArchiveKey(claims.Audience, digest)
		if err != nil || key != input.Key {
			return s.nativeAPIError(c, nativeInvalid("Invalid archive key"))
		}
		if err := s.database.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND object_deleted_at IS NULL)", claims.Audience, key).Scan(&exists); err != nil {
			return s.nativeAPIError(c, err)
		}
	} else if err := s.database.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attachments WHERE id=? AND object_deleted_at IS NULL)", input.ID).Scan(&exists); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"exists": exists})
}

func bindCloudAttachmentReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, item, comment, body string) error {
	for _, ref := range attachment.References(body, string(scope.organization), string(scope.project)) {
		if scope.credential.HostedRole == "reporter" {
			if _, err := readCloudAttachment(ctx, tx, scope, ref.ID); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO attachment_references(attachment_id,work_item_id,comment_id) SELECT id,?,? FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND deleted_at IS NULL ON CONFLICT(attachment_id,work_item_id,comment_id) DO UPDATE SET attachment_id=excluded.attachment_id`, item, comment, scope.organization, scope.project, ref.ID)
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
