package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/labstack/echo/v4"
)

const attachmentMetadataBase = nativeBase + "/attachment-metadata"

func (s *Service) registerCloudAttachmentRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	write := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	e.POST(attachmentMetadataBase+"/check", s.checkCloudAttachment, write)
	e.POST(attachmentMetadataBase, s.recordCloudAttachment, write)
	e.GET(attachmentMetadataBase+"/:attachment", s.getCloudAttachment, read)
	e.DELETE(attachmentMetadataBase+"/:attachment", s.deleteCloudAttachment, write)
	e.POST(attachmentMetadataBase+"/:attachment/reference", s.referenceCloudAttachment, write)
	e.POST("/internal/v1/attachments/expired", s.expiredCloudAttachments)
	e.POST("/internal/v1/attachments/deleted", s.finishCloudAttachmentDeletion)
	e.POST("/internal/v1/attachments/exists", s.cloudAttachmentExists)
}

func (s *Service) checkCloudAttachment(c echo.Context) error {
	var input struct {
		Size int64 `json:"size"`
	}
	if err := c.Bind(&input); err != nil || input.Size < 0 || input.Size > attachment.MaxBytes {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment size"))
	}
	now := s.config.now()
	used, err := s.database.hostedConsumption(c.Request().Context(), s.database.db, now)
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

func (s *Service) recordCloudAttachment(c echo.Context) error {
	var record attachment.Metadata
	if err := c.Bind(&record); err != nil || record.Validate() != nil || record.WorkItemID != "" || record.CommentID != "" || record.DeletedAt != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid attachment metadata"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	now := s.config.now()
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	record.ProjectID, record.Uploader, record.CreatedAt = string(scope.project), scope.credential.ID, now
	_, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,width,height,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.ID, scope.organization, scope.project, record.Uploader, record.Name, record.ContentType, record.Size, record.SHA256, record.Width, record.Height, formatHubTime(now))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, record)
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
	record, err := scanCloudAttachment(s.database.db.QueryRowContext(c.Request().Context(), "SELECT "+cloudAttachmentColumns+" FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL", scope.organization, scope.project, c.Param("attachment")))
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	record.AuthorizedPrincipal = scope.credential.ID
	return c.JSON(http.StatusOK, record)
}

func (s *Service) deleteCloudAttachment(c echo.Context) error {
	scope := nativeRequestScope(c)
	result, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE attachments SET deleted_at=coalesce(deleted_at,?) WHERE organization_id=? AND project_id=? AND id=?", formatHubTime(s.config.now()), scope.organization, scope.project, c.Param("attachment"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if count == 0 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) referenceCloudAttachment(c echo.Context) error {
	var input struct {
		WorkItemID string `json:"work_item_id"`
		CommentID  string `json:"comment_id"`
	}
	if err := c.Bind(&input); err != nil || input.WorkItemID == "" {
		return s.nativeAPIError(c, nativeInvalid("A work item is required"))
	}
	scope, ctx := nativeRequestScope(c), c.Request().Context()
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, _, err := readNativeIssue(ctx, tx, scope, input.WorkItemID); err != nil {
		return s.nativeAPIError(c, err)
	}
	if input.CommentID != "" {
		if _, err := readNativeComment(ctx, tx, scope, input.WorkItemID, input.CommentID); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE attachments SET work_item_id=?,comment_id=nullif(?,'') WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL AND (work_item_id IS NULL OR (work_item_id=? AND coalesce(comment_id,'')=?))`, input.WorkItemID, input.CommentID, scope.organization, scope.project, c.Param("attachment"), input.WorkItemID, input.CommentID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if count != 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) expiredCloudAttachments(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	ctx, now := c.Request().Context(), s.config.now()
	if _, err := s.database.db.ExecContext(ctx, "UPDATE attachments SET deleted_at=? WHERE deleted_at IS NULL AND work_item_id IS NULL AND julianday(created_at)<=julianday(?)", formatHubTime(now), formatHubTime(now.Add(-attachment.OrphanTTL))); err != nil {
		return s.nativeAPIError(c, err)
	}
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
