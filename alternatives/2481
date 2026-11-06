package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConversationImportRow struct {
	Table   string
	Columns []string
	Values  []any
	Receipt ConversationImportReceipt
}

type ConversationImportBatch struct {
	OrganizationID tracker.OrganizationID
	ContentObjects []conversationstore.ContentObject
	Attachments    []ConversationImportAttachment
	Rows           []ConversationImportRow
}

type ConversationImportAttachment struct {
	Record  conversationstore.Attachment
	Receipt ConversationImportReceipt
}

type ConversationImportReceipt struct {
	SnapshotID  string
	Table       string
	SourceRow   int64
	RequestHash string
	CreatedAt   time.Time
}

var conversationImportIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var conversationImportTables = map[string]map[string]struct{}{
	"conversations":                importColumns("archived", "content_attachment_id", "conversion_state", "created_at", "creation_hash", "creation_key", "event_seq", "execution_json", "execution_lease_id", "execution_settled", "execution_status", "id", "last_message_at", "linked_at", "origin", "organization_id", "owner_principal_id", "owner_subject", "preferences_json", "project_id", "provider_thread_id", "provider_thread_origin", "provider_thread_runner_id", "query_metadata_version", "read_audience", "revision", "settled_at", "status", "subject_work_item_id", "title", "updated_at", "visibility", "work_item_id"),
	"conversation_messages":        importColumns("actor_json", "actor_kind", "actor_principal_id", "attempt_id", "command_key", "content_attachment_id", "conversation_id", "conversion_state", "created_at", "data_json", "delivery", "id", "kind", "operator_action_agent", "operator_action_id", "operator_action_result", "organization_id", "provider_item_id", "query_metadata_version", "revision", "role", "seq", "stored_text_bytes", "text", "text_bytes", "thread_id", "tool_item_id", "tool_name", "tool_status", "turn_id", "updated_at"),
	"conversation_questions":       importColumns("answered_by", "answers_json", "attempt_id", "content_attachment_id", "conversation_id", "conversion_state", "created_at", "expires_at", "id", "message_id", "organization_id", "prompts_json", "request_id", "status", "thread_id", "turn_id", "updated_at"),
	"conversation_commands":        importColumns("content_attachment_id", "conversation_id", "conversion_state", "created_at", "key", "kind", "organization_id", "query_metadata_version", "receipt_json", "receipt_status", "request_hash", "updated_at"),
	"conversation_events":          importColumns("attempt_id", "body_json", "content_attachment_id", "conversation_id", "conversion_state", "created_at", "delta_bytes", "delta_offset", "execution_status", "message_id", "message_kind", "message_revision", "message_role", "object_id", "organization_id", "query_metadata_version", "seq", "snapshot_access", "snapshot_effort", "snapshot_linked_at", "snapshot_model", "snapshot_preferences_key", "snapshot_status", "snapshot_subject_work_item_id", "snapshot_title", "snapshot_visibility", "snapshot_work_item_id", "thread_id", "turn_id", "type"),
	"conversation_attachments":     importColumns("artifact_ref", "content_attachment_id", "conversation_id", "created_at", "deleted_at", "expires_at", "id", "message_id", "mime", "name", "organization_id", "principal_id", "project_id", "size"),
	"conversation_starts":          importColumns("attempt_id", "conversation_id", "created_at", "organization_id"),
	"conversation_audience_events": importColumns("actor_principal_id", "conversation_id", "created_at", "from_visibility", "id", "organization_id", "to_visibility"),
	"conversation_turn_batches":    importColumns("attempt_id", "batch_key", "created_at", "event_seq", "organization_id"),
	"conversation_tool_revisions":  importColumns("content_attachment_id", "conversation_id", "conversion_state", "item_id", "message_id", "organization_id", "revision", "status", "text", "tool"),
	"message_references":           importColumns("conversation_id", "created_at", "label", "message_id", "organization_id", "position", "target_id", "target_kind"),
}

func importColumns(columns ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		result[column] = struct{}{}
	}
	return result
}

func (r *Repository) ImportConversationBatch(ctx context.Context, batch ConversationImportBatch) error {
	if batch.OrganizationID == "" || len(batch.Rows) == 0 && len(batch.ContentObjects) == 0 && len(batch.Attachments) == 0 {
		return errors.New("invalid conversation import batch")
	}
	for _, row := range batch.Rows {
		allowed, ok := conversationImportTables[row.Table]
		if !ok || len(row.Columns) == 0 || len(row.Columns) != len(row.Values) || row.Receipt.Table != row.Table || row.Receipt.SourceRow < 1 || !artifact.ValidHash(row.Receipt.SnapshotID, 64) || !artifact.ValidHash(row.Receipt.RequestHash, 64) || row.Receipt.CreatedAt.IsZero() {
			return corestore.ErrInvalidPage
		}
		seen := make(map[string]struct{}, len(row.Columns))
		for _, column := range row.Columns {
			if !conversationImportIdentifier.MatchString(column) {
				return corestore.ErrInvalidPage
			}
			if _, ok := allowed[column]; !ok {
				return corestore.ErrInvalidPage
			}
			if _, duplicate := seen[column]; duplicate {
				return corestore.ErrInvalidPage
			}
			seen[column] = struct{}{}
		}
	}
	for _, object := range batch.ContentObjects {
		if object.OrganizationID != batch.OrganizationID {
			return corestore.ErrTenantMismatch
		}
	}
	for _, attachment := range batch.Attachments {
		if attachment.Record.OrganizationID != batch.OrganizationID || attachment.Receipt.Table != "conversation_attachments" || attachment.Receipt.SourceRow < 1 || !artifact.ValidHash(attachment.Receipt.SnapshotID, 64) || !artifact.ValidHash(attachment.Receipt.RequestHash, 64) {
			return corestore.ErrTenantMismatch
		}
	}
	return r.transaction(ctx, batch.OrganizationID, corestore.Background, false, func(s *session) error {
		for _, object := range batch.ContentObjects {
			if err := s.PutConversationContent(ctx, object); err != nil {
				return err
			}
		}
		for _, row := range batch.Rows {
			if err := s.importConversationRow(ctx, row); err != nil {
				return err
			}
		}
		for _, attachment := range batch.Attachments {
			if err := s.importConversationAttachment(ctx, attachment); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *session) importConversationRow(ctx context.Context, row ConversationImportRow) error {
	var previousSnapshot, previousHash string
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_id,request_hash FROM conversation_history_import_rows WHERE organization_id=$1 AND source_table=$2 AND source_row_id=$3`, s.organization, row.Table, row.Receipt.SourceRow).Scan(&previousSnapshot, &previousHash)
	if err == nil {
		if previousSnapshot != row.Receipt.SnapshotID || previousHash != row.Receipt.RequestHash {
			return mutation.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	columns := make([]string, len(row.Columns))
	placeholders := make([]string, len(row.Columns))
	for n, column := range row.Columns {
		columns[n] = `"` + column + `"`
		placeholders[n] = fmt.Sprintf("$%d", n+1)
	}
	query := `INSERT INTO "` + row.Table + `" (` + strings.Join(columns, ",") + `) VALUES (` + strings.Join(placeholders, ",") + `) ON CONFLICT DO NOTHING`
	result, err := s.db.ExecContext(ctx, query, row.Values...)
	if err != nil {
		return mapError(ctx, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 1 {
		return mutation.ErrConflict
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO conversation_history_import_rows(organization_id,snapshot_id,source_table,source_row_id,request_hash,created_at) VALUES($1,$2,$3,$4,$5,$6)`, s.organization, row.Receipt.SnapshotID, row.Receipt.Table, row.Receipt.SourceRow, row.Receipt.RequestHash, row.Receipt.CreatedAt)
	return mapError(ctx, err)
}

func (s *session) importConversationAttachment(ctx context.Context, imported ConversationImportAttachment) error {
	row := imported.Record
	if row.ID == "" || row.ConversationID == "" || row.ProjectID == "" || row.PrincipalID == "" || row.ArtifactRef == "" || row.Size <= 0 || row.CreatedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	var messageID, contentID any
	if row.MessageID != "" {
		messageID = row.MessageID
	}
	if row.ContentID != "" {
		contentID = row.ContentID
	}
	if imported.Receipt.Table != "conversation_attachments" || imported.Receipt.SourceRow < 1 || !artifact.ValidHash(imported.Receipt.SnapshotID, 64) || !artifact.ValidHash(imported.Receipt.RequestHash, 64) || imported.Receipt.CreatedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	if err := s.importConversationRow(ctx, ConversationImportRow{Table: "conversation_attachments", Columns: []string{"organization_id", "conversation_id", "project_id", "id", "message_id", "principal_id", "name", "mime", "size", "artifact_ref", "content_attachment_id", "created_at", "expires_at", "deleted_at"}, Values: []any{row.OrganizationID, row.ConversationID, row.ProjectID, row.ID, messageID, row.PrincipalID, row.Name, row.MIME, row.Size, row.ArtifactRef, contentID, row.CreatedAt, row.ExpiresAt, row.DeletedAt}, Receipt: imported.Receipt}); err != nil {
		return err
	}
	if row.ContentID == "" {
		return nil
	}
	object, err := s.ConversationContent(ctx, row.ContentID)
	if err != nil {
		return err
	}
	return s.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: row.OrganizationID, ArchiveID: object.ArchiveID, Kind: "conversation-attachment", ID: row.ID, CreatedAt: row.CreatedAt})
}
