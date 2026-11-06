package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func archiveReference(row attachmentstore.Record) attachment.ArchiveReference {
	return attachment.ArchiveReference{Key: row.ObjectKey, SHA256: row.Metadata.SHA256, CompressedSize: row.Metadata.Size, UncompressedSize: row.UncompressedSize, Codec: row.Codec, SchemaVersion: int(row.SchemaVersion), Kind: row.Kind, Retention: row.Retention, BindingID: row.StorageBindingID, CustomerOwned: row.CustomerOwned}
}

func (s *session) PrepareAttachmentArchive(ctx context.Context, r attachmentstore.ArchiveRecord) (bool, error) {
	if err := s.checkOrganization(r.OrganizationID); err != nil {
		return false, err
	}
	ref := r.Reference
	if ref.Validate(string(s.organization)) != nil || r.ID == "" || r.ProjectID == "" || r.Uploader == "" || r.CreatedAt.IsZero() {
		return false, attachment.ErrInvalid
	}
	result, err := s.db.Exec(ctx, `INSERT INTO attachments (organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version,storage_binding_id,customer_owned)
 VALUES ($1,$2,$3,$4,'archive.gz','application/gzip',$5,$6,$7,'archive',$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (organization_id,id) DO NOTHING`, s.organization, r.ProjectID, r.ID, r.Uploader, ref.CompressedSize, ref.SHA256, r.CreatedAt, ref.Retention, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.BindingID, ref.CustomerOwned)
	if err != nil {
		return false, mapError(ctx, err)
	}
	n := result.RowsAffected()
	row, err := scanAttachmentRecord(s.db.QueryRow(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, r.ID))
	if err != nil {
		return false, mapError(ctx, err)
	}
	if archiveReference(row) != ref || row.Metadata.DeletedAt != nil || row.ObjectDeletedAt != nil {
		return false, corestore.ErrRevisionConflict
	}
	return n == 1, nil
}

func (s *session) PublishAttachmentArchive(ctx context.Context, r attachmentstore.ArchiveRecord, item tracker.NativeWorkItemID) error {
	if err := s.checkOrganization(r.OrganizationID); err != nil {
		return err
	}
	row, err := scanAttachmentRecord(s.db.QueryRow(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, r.ID))
	if err != nil {
		return mapError(ctx, err)
	}
	if archiveReference(row) != r.Reference || row.Metadata.DeletedAt != nil || row.ObjectDeletedAt != nil {
		return corestore.ErrRevisionConflict
	}
	_, err = s.db.Exec(ctx, `INSERT INTO attachment_archive_references (organization_id,project_id,work_item_id,attachment_id) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.organization, r.ProjectID, item, r.ID)
	return mapError(ctx, err)
}

func (s *session) AttachmentArchive(ctx context.Context, q attachmentstore.ArchiveQuery) (attachment.ArchiveReference, error) {
	row, err := scanAttachmentRecord(s.db.QueryRow(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments a WHERE organization_id=$1 AND kind='archive' AND sha256=$4 AND deleted_at IS NULL AND object_deleted_at IS NULL
 AND ($5::text IS NULL OR storage_binding_id=$5)
 AND EXISTS (SELECT 1 FROM attachment_archive_references r WHERE r.organization_id=a.organization_id AND r.project_id=$2 AND r.work_item_id=$3 AND r.attachment_id=a.id)
 AND (($6='' AND NOT EXISTS (SELECT 1 FROM issue_archive_sections r JOIN attachments b ON b.organization_id=r.organization_id AND b.id=r.attachment_id WHERE r.organization_id=$1 AND r.project_id=$2 AND r.work_item_id=$3 AND b.sha256=$4))
 OR ($6<>'' AND EXISTS (SELECT 1 FROM issue_archive_sections r JOIN attachments b ON b.organization_id=r.organization_id AND b.id=r.attachment_id WHERE r.organization_id=$1 AND r.project_id=$2 AND r.work_item_id=$3 AND r.section_id=$6 AND b.sha256=a.sha256 AND r.archive_revision=$7 AND r.archive_revision=(SELECT MAX(archive_revision) FROM issue_archive_sections WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3))))
 ORDER BY created_at DESC,id LIMIT 1`, s.organization, q.ProjectID, q.WorkItemID, q.Digest, q.Binding, q.Section, q.Revision))
	if err != nil {
		return attachment.ArchiveReference{}, mapError(ctx, err)
	}
	ref := archiveReference(row)
	if err := ref.Validate(string(s.organization)); err != nil {
		return ref, corestore.ErrNotFound
	}
	return ref, nil
}
