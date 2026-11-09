package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const attachmentMetadataBase = nativeBase + "/attachment-metadata"

var cloudAttachmentReference = regexp.MustCompile(`\]\(\s*<?attachment:(att_[A-Za-z0-9_-]+)>?(?:\s+"[^"]*")?\s*\)`)

type cloudContentField struct {
	name  string
	value string
}

func validateCloudVisualReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, fields ...cloudContentField) error {
	if len(fields) == 0 || scope.importedContent {
		return nil
	}
	settings, err := projectsettings.ReadContent(ctx, tx, scope.organization, scope.project)
	if err != nil || !settings.RequireVisualAttachments {
		return err
	}
	for _, field := range fields {
		err := attachment.ValidateVisualReferences(field.value, string(scope.organization), string(scope.project), func(id string) (attachment.Metadata, error) {
			record, err := readCloudAttachmentMetadata(ctx, tx, scope, id)
			if isNativeNotFound(err) {
				return attachment.Metadata{}, nil
			}
			return record, err
		})
		var refusal *attachment.VisualReferenceError
		if errors.As(err, &refusal) {
			return nativePublicInvalid(fmt.Sprintf("%s block %d: %s", field.name, refusal.Block, attachment.VisualReferenceMessage))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func validateCloudChangeReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, fields []cloudContentField, artifacts []tracker.ChangeArtifact, artifactName string) error {
	settings, err := projectsettings.ReadContent(ctx, tx, scope.organization, scope.project)
	if err != nil || !settings.RequireVisualAttachments || scope.importedContent {
		return err
	}
	if err := validateCloudVisualReferences(ctx, tx, scope, fields...); err != nil {
		return err
	}
	err = changerequest.ValidateVisualArtifacts(artifacts, string(scope.organization), string(scope.project), func(id string) (attachment.Metadata, error) {
		record, err := readCloudAttachmentMetadata(ctx, tx, scope, id)
		if isNativeNotFound(err) {
			return attachment.Metadata{}, nil
		}
		return record, err
	})
	var refusal *attachment.VisualReferenceError
	if errors.As(err, &refusal) {
		return nativePublicInvalid(fmt.Sprintf("%s[%d]: %s", artifactName, refusal.Block-1, attachment.VisualReferenceMessage))
	}
	return err
}

func bindCloudChangeReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, fields []cloudContentField, artifacts []tracker.ChangeArtifact, artifactName string) error {
	settings, err := projectsettings.ReadContent(ctx, tx, scope.organization, scope.project)
	if err != nil || !settings.RequireVisualAttachments || scope.importedContent {
		return err
	}
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		if ref, ok := attachment.URIReference(artifact.URI, string(scope.organization), string(scope.project)); ok {
			fields = append(fields, cloudContentField{name: artifactName, value: "[upload](" + ref.URL + ")"})
		}
	}
	for _, field := range fields {
		references, err := cloudContentAttachmentReferences(field.value, scope)
		if err != nil {
			return err
		}
		for _, ref := range references {
			if seen[ref.ID] {
				continue
			}
			seen[ref.ID] = true
			if err := bindCloudAttachmentReference(ctx, tx, scope, item, "", ref.ID, true); err != nil {
				if isNativeNotFound(err) {
					return nativePublicInvalid(field.name + ": " + attachment.VisualReferenceMessage)
				}
				return err
			}
		}
	}
	return nil
}

func cloudIssueAttachmentContent(ctx context.Context, tx *sql.Tx, scope nativeScope, title, body string) (string, error) {
	settings, err := projectsettings.ReadContent(ctx, tx, scope.organization, scope.project)
	if err != nil {
		return "", err
	}
	if settings.RequireVisualAttachments {
		return title + "\n\n" + body, nil
	}
	return body, nil
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

func syncCloudAttachmentReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, itemID, commentID, body string, retainedContent ...string) error {
	refs, err := cloudContentAttachmentReferences(body, scope)
	if err != nil {
		return err
	}
	ids := make(map[string]bool, len(refs))
	for _, reference := range refs {
		ids[reference.ID] = true
	}
	for _, content := range retainedContent {
		references, err := cloudContentAttachmentReferences(content, scope)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if ids[reference.ID] {
				continue
			}
			var bound bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attachment_references WHERE attachment_id=? AND work_item_id=? AND comment_id=?)", reference.ID, itemID, commentID).Scan(&bound); err != nil {
				return err
			}
			if bound {
				ids[reference.ID] = true
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_references WHERE work_item_id=? AND comment_id=? AND retain_with_work_item=0 AND attachment_id IN (SELECT id FROM attachments WHERE organization_id=? AND project_id=?)`, itemID, commentID, scope.organization, scope.project); err != nil {
		return err
	}
	for id := range ids {
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
	e.DELETE(attachmentMetadataBase+"/:attachment", s.deleteCloudAttachment, s.requireNativeScope(apiScopeOperator))
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
	return s.prepareCloudAttachmentTenant(c, input)
}

func (s *Service) recordCloudAttachment(c echo.Context) error {
	var input attachment.UploadRequest
	if err := c.Bind(&input); err != nil || input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment metadata"))
	}
	return s.recordCloudAttachmentTenant(c, input)
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

const cloudAttachmentColumns = "id,coalesce(project_id,''),uploader,name,content_type,size,sha256,width,height,created_at,coalesce(work_item_id,''),coalesce(comment_id,''),deleted_at"

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

func readCloudAttachmentMetadata(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (attachment.Metadata, error) {
	if scope.credential.HostedRole == "reporter" {
		pageQuery, pageErr := prepareNativePageQuery(ctx, query, nativeScope{organization: scope.organization}, "", "native_issue_actors")
		if pageErr != nil {
			return attachment.Metadata{}, pageErr
		}
		query = pageQuery
	}
	record, err := scanCloudAttachment(query.QueryRowContext(ctx, "SELECT "+cloudAttachmentColumns+" FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND deleted_at IS NULL", scope.organization, scope.project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, err
	}
	var projectIcon bool
	if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE organization_id=? AND id=? AND icon_kind='image' AND icon_image_key=? AND deleted_at IS NULL)", scope.organization, scope.project, id).Scan(&projectIcon); err != nil {
		return attachment.Metadata{}, err
	}
	if scope.credential.HostedRole == "reporter" && !projectIcon {
		visibility, args := scope.issueVisibilitySQL("i")
		args = append([]any{record.ID, scope.organization, scope.project}, args...)
		var visible bool
		if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attachment_references r JOIN issues i ON i.native_id=r.work_item_id WHERE r.attachment_id=? AND i.organization_id=? AND i.project_id=? AND ("+visibility+"))", args...).Scan(&visible); err != nil {
			return attachment.Metadata{}, err
		}
		ownsUpload := record.Uploader == scope.actor().PrincipalID || record.Uploader == scope.credential.ID
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
	return record, nil
}

func readCloudAttachment(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (attachment.Metadata, error) {
	if scope.credential.HostedRole == "reporter" {
		pageQuery, pageErr := prepareNativePageQuery(ctx, query, nativeScope{organization: scope.organization}, "", "native_issue_actors")
		if pageErr != nil {
			return attachment.Metadata{}, pageErr
		}
		query = pageQuery
	}
	record, err := readCloudAttachmentMetadata(ctx, query, scope, id)
	if err != nil {
		return record, err
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
	if scope.credential.Scope == apiScopeWorker {
		return nativeNotFound()
	}
	return s.deleteCloudAttachmentTenant(ctx, scope, id)
}

func (s *Service) deleteCloudAttachmentMetadata(ctx context.Context, query nativeExecer, scope nativeScope, id string) error {
	result, err := query.ExecContext(ctx, "UPDATE attachments SET deleted_at=coalesce(deleted_at,?) WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND NOT EXISTS(SELECT 1 FROM projects p WHERE p.id=attachments.project_id AND p.organization_id=attachments.organization_id AND p.icon_kind='image' AND p.icon_image_key=attachments.id AND p.deleted_at IS NULL)", formatHubTime(s.config.now()), scope.organization, scope.project, id)
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
	return s.referenceCloudAttachmentTenant(ctx, scope, id, input)
}

func (s *Service) expiredCloudAttachments(c echo.Context) error {
	return s.expiredCloudAttachmentsTenant(c)
}

func (s *Service) finishCloudAttachmentDeletion(c echo.Context) error {
	return s.finishCloudAttachmentDeletionTenant(c)
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

func bindCloudAttachmentReferences(ctx context.Context, tx *sql.Tx, scope nativeScope, item, comment, body string) error {
	refs, err := cloudContentAttachmentReferences(body, scope)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref.URL, "attachment:") || seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		if err := bindCloudAttachmentReference(ctx, tx, scope, item, comment, ref.ID, false); err != nil {
			return err
		}
	}
	return nil
}

func bindCloudAttachmentReference(ctx context.Context, tx *sql.Tx, scope nativeScope, item, comment, id string, retain bool) error {
	if scope.credential.HostedRole == "reporter" {
		if _, err := readCloudAttachmentMetadata(ctx, tx, scope, id); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO attachment_references(attachment_id,work_item_id,comment_id,retain_with_work_item) SELECT id,?,?,? FROM attachments WHERE organization_id=? AND project_id=? AND id=? AND kind='attachment' AND deleted_at IS NULL ON CONFLICT(attachment_id,work_item_id,comment_id) DO UPDATE SET retain_with_work_item=max(attachment_references.retain_with_work_item,excluded.retain_with_work_item)`, item, comment, retain, scope.organization, scope.project, id)
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
	return nil
}
