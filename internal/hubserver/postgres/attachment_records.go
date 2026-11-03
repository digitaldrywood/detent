package postgres

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const attachmentRecordColumns = `organization_id,project_id,id,uploader,name,content_type,size,sha256,width,height,created_at,work_item_id,comment_id,evidence_attempt_id,evidence_caption,deleted_at,object_deleted_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version,storage_binding_id,customer_owned,pending_content`

func scanAttachmentRecord(row interface{ Scan(...any) error }) (attachmentstore.Record, error) {
	var r attachmentstore.Record
	var item, comment, attempt sql.NullString
	err := row.Scan(&r.OrganizationID, &r.Metadata.ProjectID, &r.Metadata.ID, &r.Metadata.Uploader, &r.Metadata.Name, &r.Metadata.ContentType, &r.Metadata.Size, &r.Metadata.SHA256, &r.Metadata.Width, &r.Metadata.Height, &r.Metadata.CreatedAt, &item, &comment, &attempt, &r.EvidenceCaption, &r.Metadata.DeletedAt, &r.ObjectDeletedAt, &r.Kind, &r.Retention, &r.ObjectKey, &r.UncompressedSize, &r.Codec, &r.SchemaVersion, &r.StorageBindingID, &r.CustomerOwned, &r.PendingContent)
	r.Metadata.WorkItemID, r.Metadata.CommentID, r.EvidenceAttemptID = item.String, comment.String, attempt.String
	return r, err
}
func (s *session) Attachment(ctx context.Context, project tracker.ProjectID, id string) (attachmentstore.Record, error) {
	r, err := scanAttachmentRecord(s.db.QueryRowContext(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND id=$3`, s.organization, project, id))
	return r, mapError(ctx, err)
}
func (s *session) AttachmentEvidence(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, attempt string, limit int) ([]attachmentstore.Record, error) {
	if limit < 1 || limit > 201 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND evidence_attempt_id=$4 ORDER BY created_at,id LIMIT $5`, s.organization, project, item, attempt, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []attachmentstore.Record{}
	for rows.Next() {
		r, err := scanAttachmentRecord(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, r)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) CreateAttachment(ctx context.Context, r attachmentstore.Record) error {
	if err := s.checkOrganization(r.OrganizationID); err != nil {
		return err
	}
	if r.Metadata.Validate() != nil || r.Metadata.ProjectID == "" || r.Metadata.Uploader == "" || r.Metadata.CreatedAt.IsZero() || r.Kind != "attachment" || r.PendingContent != nil {
		return attachment.ErrInvalid
	}
	if r.Metadata.WorkItemID != "" {
		item, err := s.WorkItem(ctx, tracker.ProjectID(r.Metadata.ProjectID), tracker.NativeWorkItemID(r.Metadata.WorkItemID))
		if err != nil {
			return err
		}
		if r.Metadata.CommentID != "" {
			if _, err := s.Comment(ctx, item.ProjectID, item.NativeID, r.Metadata.CommentID); err != nil {
				return err
			}
		}
		if r.EvidenceAttemptID != "" {
			attempt, err := s.Attempt(ctx, r.EvidenceAttemptID)
			if err != nil {
				return err
			}
			if attempt.IssueID != item.ID {
				return corestore.ErrTenantMismatch
			}
		}
	} else if r.Metadata.CommentID != "" || r.EvidenceAttemptID != "" {
		return attachment.ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO attachments (`+attachmentRecordColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),NULLIF($13,''),NULLIF($14,''),$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`, s.organization, r.Metadata.ProjectID, r.Metadata.ID, r.Metadata.Uploader, r.Metadata.Name, r.Metadata.ContentType, r.Metadata.Size, r.Metadata.SHA256, r.Metadata.Width, r.Metadata.Height, r.Metadata.CreatedAt, r.Metadata.WorkItemID, r.Metadata.CommentID, r.EvidenceAttemptID, r.EvidenceCaption, r.Metadata.DeletedAt, r.ObjectDeletedAt, r.Kind, r.Retention, r.ObjectKey, r.UncompressedSize, r.Codec, r.SchemaVersion, r.StorageBindingID, r.CustomerOwned, r.PendingContent)
	return mapError(ctx, err)
}
func (s *session) AttachmentReferences(ctx context.Context, project tracker.ProjectID, id string, limit int) ([]attachmentstore.Reference, error) {
	if limit < 1 || limit > 201 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project_id,attachment_id,work_item_id,comment_id,retain_with_work_item FROM attachment_item_references WHERE organization_id=$1 AND project_id=$2 AND attachment_id=$3 ORDER BY work_item_id,comment_id LIMIT $4`, s.organization, project, id, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []attachmentstore.Reference{}
	for rows.Next() {
		var r attachmentstore.Reference
		if err := rows.Scan(&r.ProjectID, &r.AttachmentID, &r.WorkItemID, &r.CommentID, &r.RetainWithWorkItem); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, r)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) BindAttachment(ctx context.Context, reference attachmentstore.Reference) error {
	project, id, item, comment := reference.ProjectID, reference.AttachmentID, reference.WorkItemID, reference.CommentID
	record, err := s.Attachment(ctx, project, id)
	if err != nil {
		return err
	}
	if record.Metadata.DeletedAt != nil || record.ObjectDeletedAt != nil || record.PendingContent != nil {
		return corestore.ErrNotFound
	}
	if _, err := s.WorkItem(ctx, project, item); err != nil {
		return err
	}
	if comment != "" {
		if _, err := s.Comment(ctx, project, item, comment); err != nil {
			return err
		}
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO attachment_item_references (organization_id,project_id,attachment_id,work_item_id,comment_id,retain_with_work_item) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (organization_id,attachment_id,work_item_id,comment_id) DO UPDATE SET retain_with_work_item=attachment_item_references.retain_with_work_item OR EXCLUDED.retain_with_work_item WHERE attachment_item_references.project_id=EXCLUDED.project_id`, s.organization, project, id, item, comment, reference.RetainWithWorkItem)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
