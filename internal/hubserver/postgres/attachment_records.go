package postgres

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

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
func (s *session) LockAttachment(ctx context.Context, project tracker.ProjectID, id string) (attachmentstore.Record, error) {
	r, err := scanAttachmentRecord(s.db.QueryRowContext(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR SHARE`, s.organization, project, id))
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
	record, err := scanAttachmentRecord(s.db.QueryRowContext(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, s.organization, project, id))
	if err != nil {
		return mapError(ctx, err)
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
	references, err := s.AttachmentReferences(ctx, project, id, 200)
	if err != nil {
		return err
	}
	if len(references) == 200 {
		existing := false
		for _, current := range references {
			if current.WorkItemID == item && current.CommentID == comment {
				existing = true
				break
			}
		}
		if !existing {
			return attachment.ErrInvalid
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
	_, err = s.db.ExecContext(ctx, `UPDATE attachments SET retention_class='work_item' WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND retention_class IN ('','expiring')`, s.organization, project, id)
	return mapError(ctx, err)
}

func (s *session) DeleteAttachment(ctx context.Context, project tracker.ProjectID, id string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE attachments SET deleted_at=COALESCE(deleted_at,$4) WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND kind='attachment' AND retention_class<>'organization' AND NOT customer_owned AND storage_binding_id='' AND pending_content IS NULL AND object_key IN ('','orgs/' || organization_id || '/attachments/' || id)`, s.organization, project, id, at)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) FinishAttachmentDeletion(ctx context.Context, id string, at time.Time) error {
	var size int64
	err := s.db.QueryRowContext(ctx, `UPDATE attachments SET object_deleted_at=$3 WHERE organization_id=$1 AND id=$2 AND kind='attachment' AND deleted_at IS NOT NULL AND object_deleted_at IS NULL AND NOT customer_owned AND storage_binding_id='' AND pending_content IS NULL AND object_key IN ('','orgs/' || organization_id || '/attachments/' || id) RETURNING size`, s.organization, id, at).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		var completed bool
		err = s.db.QueryRowContext(ctx, `SELECT object_deleted_at IS NOT NULL FROM attachments WHERE organization_id=$1 AND id=$2 AND deleted_at IS NOT NULL`, s.organization, id).Scan(&completed)
		if err == nil && !completed {
			return corestore.ErrNotFound
		}
		return mapError(ctx, err)
	}
	if err != nil {
		return mapError(ctx, err)
	}
	_, err = s.AdjustCollaborationBytes(ctx, -size, math.MaxInt64)
	return err
}

const managedAttachmentPredicate = `kind='attachment' AND NOT customer_owned AND storage_binding_id='' AND pending_content IS NULL AND object_key IN ('','orgs/' || organization_id || '/attachments/' || id)`
const orphanAttachmentPredicate = `deleted_at IS NULL AND object_deleted_at IS NULL AND retention_class IN ('','expiring') AND work_item_id IS NULL AND created_at<=$2`
const attachmentHasReferences = `EXISTS(SELECT 1 FROM attachment_item_references r WHERE r.organization_id=attachments.organization_id AND r.attachment_id=attachments.id)`

func (s *session) AttachmentDeletionCandidates(ctx context.Context, cutoff time.Time, limit int) ([]attachmentstore.Record, error) {
	if cutoff.IsZero() || limit < 1 || limit > 100 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
 (SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND `+managedAttachmentPredicate+` AND deleted_at IS NOT NULL AND object_deleted_at IS NULL ORDER BY created_at,id LIMIT $3)
 UNION ALL
 (SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND `+managedAttachmentPredicate+` AND `+orphanAttachmentPredicate+` ORDER BY created_at,id LIMIT $3)
 ) candidates ORDER BY created_at,id LIMIT $3`, s.organization, cutoff, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	records := []attachmentstore.Record{}
	for rows.Next() {
		record, err := scanAttachmentRecord(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		records = append(records, record)
	}
	return records, mapError(ctx, rows.Err())
}

func (s *session) ExpireAttachment(ctx context.Context, project tracker.ProjectID, id string, cutoff, at time.Time) (bool, error) {
	var locked tracker.ProjectID
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, project).Scan(&locked); err != nil {
		return false, mapError(ctx, err)
	}
	var expired bool
	err := s.db.QueryRowContext(ctx, `UPDATE attachments SET
 retention_class=CASE WHEN `+attachmentHasReferences+` THEN 'work_item' ELSE retention_class END,
 deleted_at=CASE WHEN `+attachmentHasReferences+` THEN NULL ELSE $5::timestamptz END
 WHERE organization_id=$1 AND project_id=$3 AND id=$4 AND `+managedAttachmentPredicate+` AND `+orphanAttachmentPredicate+` RETURNING deleted_at IS NOT NULL`, s.organization, cutoff, project, id, at).Scan(&expired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return expired, mapError(ctx, err)
}
