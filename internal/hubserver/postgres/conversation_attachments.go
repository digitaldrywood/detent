package postgres

import (
	"context"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const pgConversationAttachmentColumns = `organization_id,project_id,conversation_id,id,coalesce(message_id,''),principal_id,name,mime,size,artifact_ref,coalesce(content_attachment_id,''),created_at,expires_at,deleted_at`

func scanPGConversationAttachment(row interface{ Scan(...any) error }) (conversationstore.Attachment, error) {
	var a conversationstore.Attachment
	err := row.Scan(&a.OrganizationID, &a.ProjectID, &a.ConversationID, &a.ID, &a.MessageID, &a.PrincipalID, &a.Name, &a.MIME, &a.Size, &a.ArtifactRef, &a.ContentID, &a.CreatedAt, &a.ExpiresAt, &a.DeletedAt)
	return a, err
}

func (s *session) ConversationAttachment(ctx context.Context, project tracker.ProjectID, conversationID, id string) (conversationstore.Attachment, error) {
	a, err := scanPGConversationAttachment(s.db.QueryRowContext(ctx, `SELECT `+pgConversationAttachmentColumns+` FROM conversation_attachments WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 AND id=$4`, s.organization, project, conversationID, id))
	return a, mapError(ctx, err)
}

func (s *session) ConversationAttachmentByRef(ctx context.Context, project tracker.ProjectID, conversationID, ref string) (conversationstore.Attachment, error) {
	a, err := scanPGConversationAttachment(s.db.QueryRowContext(ctx, `SELECT `+pgConversationAttachmentColumns+` FROM conversation_attachments WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 AND artifact_ref=$4`, s.organization, project, conversationID, ref))
	return a, mapError(ctx, err)
}

func (s *session) CreateConversationAttachment(ctx context.Context, a conversationstore.Attachment, object conversationstore.ContentObject) error {
	if err := s.checkOrganization(a.OrganizationID); err != nil {
		return err
	}
	if a.OrganizationID != object.OrganizationID || a.ContentID != object.ID || object.ID != pgConversationContentID(string(s.organization), object.Reference) || object.Reference.Validate(string(s.organization)) != nil || object.Reference.UncompressedSize != a.Size || a.CreatedAt.IsZero() || object.CreatedAt.IsZero() || a.ProjectID == "" || a.PrincipalID == "" || a.ArtifactRef == "" || a.Name == "" || a.MIME == "" || a.MessageID != "" || a.DeletedAt != nil || a.ExpiresAt == nil || !a.ExpiresAt.After(a.CreatedAt) || conversation.ValidateAttachmentID(a.ID) != nil || conversation.ValidateAttachmentBytes(a.Size) != nil {
		return corestore.ErrInvalidPage
	}
	current, err := s.AttachmentConversation(ctx, a.ProjectID, a.ConversationID)
	if err != nil {
		return err
	}
	if current.OrganizationID != a.OrganizationID {
		return corestore.ErrTenantMismatch
	}
	if _, err := s.AccessCredential(ctx, a.PrincipalID); err != nil {
		return err
	}
	archive, err := s.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: s.organization, ID: object.ID, Reference: object.Reference, CreatedAt: object.CreatedAt})
	if err != nil {
		return err
	}
	ref := object.Reference
	_, err = s.db.ExecContext(ctx, `INSERT INTO conversation_content_objects (organization_id,id,archive_id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (organization_id,id) DO NOTHING`, s.organization, object.ID, archive.ID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, ref.BindingID, ref.CustomerOwned, object.CreatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	stored, err := s.ConversationContent(ctx, object.ID)
	if err != nil {
		return err
	}
	if stored.Reference != ref || stored.ArchiveID != archive.ID {
		return corestore.ErrUniqueViolation
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO conversation_attachments (organization_id,project_id,conversation_id,id,principal_id,name,mime,size,artifact_ref,content_attachment_id,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, s.organization, a.ProjectID, a.ConversationID, a.ID, a.PrincipalID, a.Name, a.MIME, a.Size, a.ArtifactRef, a.ContentID, a.CreatedAt, a.ExpiresAt)
	if err != nil {
		return mapError(ctx, err)
	}
	return s.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: archive.ID, Kind: "conversation-attachment", ID: a.ID, CreatedAt: a.CreatedAt})
}

func (s *session) DeleteConversationAttachment(ctx context.Context, project tracker.ProjectID, conversationID, id string, at time.Time) error {
	a, err := s.ConversationAttachment(ctx, project, conversationID, id)
	if err != nil {
		return err
	}
	if a.DeletedAt != nil || a.MessageID != "" || a.ContentID == "" || at.IsZero() {
		return corestore.ErrNotFound
	}
	if _, err := s.ConversationContent(ctx, a.ContentID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE conversation_attachments SET deleted_at=$5 WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 AND id=$4 AND message_id IS NULL AND deleted_at IS NULL`, s.organization, project, conversationID, id, at)
	if err != nil {
		return mapError(ctx, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if n != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) AttachmentConversation(ctx context.Context, project tracker.ProjectID, id string) (conversationstore.Conversation, error) {
	return s.attachmentConversation(ctx, project, id, "")
}

func (s *session) LockAttachmentConversation(ctx context.Context, project tracker.ProjectID, id string) (conversationstore.Conversation, error) {
	return s.attachmentConversation(ctx, project, id, " FOR UPDATE")
}

func (s *session) attachmentConversation(ctx context.Context, project tracker.ProjectID, id, lock string) (conversationstore.Conversation, error) {
	var c conversationstore.Conversation
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,project_id,id,owner_principal_id,visibility,read_audience,coalesce(work_item_id,''),archived FROM conversations WHERE organization_id=$1 AND project_id=$2 AND id=$3`+lock, s.organization, project, id).Scan(&c.OrganizationID, &c.ProjectID, &c.ID, &c.OwnerPrincipalID, &c.Visibility, &c.ReadAudience, &c.WorkItemID, &c.Archived)
	return c, mapError(ctx, err)
}

func (s *session) BindConversationAttachment(ctx context.Context, project tracker.ProjectID, conversationID, id, principal, message string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE conversation_attachments SET message_id=$6,expires_at=NULL WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 AND id=$4 AND principal_id=$5 AND message_id IS NULL AND deleted_at IS NULL AND expires_at>$7 AND content_attachment_id IS NOT NULL`, s.organization, project, conversationID, id, principal, message, now)
	if err != nil {
		return mapError(ctx, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return mapError(ctx, err)
	}
	if n != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) MessageAttachments(ctx context.Context, project tracker.ProjectID, conversationID string, messages []string) (map[string][]conversationstore.Attachment, error) {
	if len(messages) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	items := make(map[string][]conversationstore.Attachment, len(messages))
	if len(messages) == 0 {
		return items, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.* FROM unnest($4::text[]) AS selected(message) CROSS JOIN LATERAL (SELECT `+pgConversationAttachmentColumns+` FROM conversation_attachments WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 AND message_id=selected.message AND deleted_at IS NULL ORDER BY created_at,id LIMIT $5) a`, s.organization, project, conversationID, pq.Array(messages), conversation.MaxMessageAttachments+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanPGConversationAttachment(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		items[a.MessageID] = append(items[a.MessageID], a)
		if len(items[a.MessageID]) > conversation.MaxMessageAttachments {
			return nil, corestore.ErrInvalidPage
		}
	}
	return items, mapError(ctx, rows.Err())
}

func (s *session) MessageSequences(ctx context.Context, conversationID string, messages []string) (map[string]int64, error) {
	if len(messages) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	items := make(map[string]int64, len(messages))
	if len(messages) == 0 {
		return items, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,seq FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND id=ANY($3::text[]) LIMIT $4`, s.organization, conversationID, pq.Array(messages), len(messages))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var sequence int64
		if err := rows.Scan(&id, &sequence); err != nil {
			return nil, mapError(ctx, err)
		}
		items[id] = sequence
	}
	return items, mapError(ctx, rows.Err())
}

func (s *session) ExpiredConversationAttachments(ctx context.Context, now time.Time, limit int) ([]conversationstore.Attachment, error) {
	if now.IsZero() || limit < 1 || limit > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+pgConversationAttachmentColumns+` FROM conversation_attachments WHERE organization_id=$1 AND message_id IS NULL AND deleted_at IS NULL AND expires_at<=$2 ORDER BY expires_at,id LIMIT $3`, s.organization, now, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var items []conversationstore.Attachment
	for rows.Next() {
		a, err := scanPGConversationAttachment(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		items = append(items, a)
	}
	return items, mapError(ctx, rows.Err())
}
