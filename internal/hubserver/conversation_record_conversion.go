package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func (s *Service) convertConversationRecordPayload(ctx context.Context, scope nativeScope, family storageConversionFamily, id string, now time.Time) error {
	if s.conversations == nil || s.conversations.store.contents == nil {
		return errors.New("conversation content storage is unavailable")
	}
	store := s.conversations.store
	stamp, safe, err := storageConversionStamp(ctx, s.database.reader, scope, family, id, now)
	if err != nil || !safe {
		return err
	}
	var row int64
	err = s.database.reader.QueryRowContext(ctx, "SELECT r.rowid FROM "+family.from+" WHERE "+family.aggregate+"=? AND r.conversion_state=1 AND "+family.where+" ORDER BY r.rowid LIMIT 1", id).Scan(&row)
	if selected, ok := ctx.Value(storageSelectedRowKey{}).(int64); ok {
		row = selected
		err = nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.database.db.ExecContext(ctx, `UPDATE storage_conversion_progress SET complete=1,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, formatHubTime(now), scope.organization, scope.project, family.name, id)
		return err
	}
	if err != nil {
		return err
	}
	var raw, content []byte
	var reclaimed int
	var first, second, key, uploader string
	var update string
	var args []any
	var metadata conversationQueryMetadata
	switch family.name {
	case "conversation_records":
		record, err := scanConversation(s.database.reader.QueryRowContext(ctx, "SELECT "+conversationColumns+",0,read_audience FROM conversations WHERE rowid=?", row))
		if err != nil {
			return err
		}
		if err := s.database.reader.QueryRowContext(ctx, "SELECT execution_json,preferences_json FROM conversations WHERE rowid=?", row).Scan(&first, &second); err != nil {
			return err
		}
		raw, err = recordPayload(record)
		if err != nil {
			return err
		}
		metadata = executionQueryMetadata(record.Execution)
		reclaimed = max(0, len(first)+len(second)-4)
		update = `UPDATE conversations SET execution_json='{}',preferences_json='{}',content_attachment_id=?,conversion_state=2 WHERE rowid=? AND content_attachment_id='' AND revision=? AND execution_json=? AND preferences_json=?`
		args = []any{row, record.Revision, first, second}
	case "conversation_questions":
		record, err := scanConversationQuestion(s.database.reader.QueryRowContext(ctx, "SELECT "+conversationQuestionColumns+" FROM conversation_questions WHERE rowid=?", row))
		if err != nil {
			return err
		}
		if err := s.database.reader.QueryRowContext(ctx, "SELECT prompts_json,answers_json FROM conversation_questions WHERE rowid=?", row).Scan(&first, &second); err != nil {
			return err
		}
		raw, err = questionPayload(record)
		if err != nil {
			return err
		}
		reclaimed = max(0, len(first)+len(second)-4)
		update = `UPDATE conversation_questions SET prompts_json='[]',answers_json='{}',content_attachment_id=?,conversion_state=2 WHERE rowid=? AND content_attachment_id='' AND prompts_json=? AND answers_json=?`
		args = []any{row, first, second}
	case "conversation_commands":
		if err := s.database.reader.QueryRowContext(ctx, "SELECT receipt_json FROM conversation_commands WHERE rowid=?", row).Scan(&first); err != nil {
			return err
		}
		raw = []byte(first)
		metadata, err = decodeConversationQueryMetadata("conversation_commands", first, "")
		if err != nil {
			return err
		}
		reclaimed = max(0, len(first)-2)
		update = `UPDATE conversation_commands SET receipt_json='{}',content_attachment_id=?,conversion_state=2 WHERE rowid=? AND content_attachment_id='' AND receipt_json=?`
		args = []any{row, first}
	case "conversation_attachments":
		if err := s.database.reader.QueryRowContext(ctx, `SELECT a.artifact_ref,b.content FROM conversation_attachments a JOIN conversation_attachment_blobs b ON b.artifact_ref=a.artifact_ref WHERE a.rowid=?`, row).Scan(&key, &content); err != nil {
			return err
		}
		reclaimed = len(content)
		update = `UPDATE conversation_attachments SET content_attachment_id=?,conversion_state=2 WHERE rowid=? AND content_attachment_id='' AND deleted_at IS NULL AND EXISTS(SELECT 1 FROM conversation_attachment_blobs WHERE artifact_ref=? AND content=?)`
		args = []any{row, key, content}
	default:
		return errors.New("unknown conversation payload family")
	}
	if family.name != "conversation_attachments" {
		content, err = messageContent(conversationMessageRecord{Data: raw})
		if err != nil {
			return err
		}
	}
	if err := expandStorageConversion(ctx, len(content)); err != nil {
		return err
	}
	ctx, err = store.preparePayload(ctx, string(scope.organization), content)
	if err != nil {
		return err
	}
	prepared, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents)
	if !ok {
		return errors.New("conversation conversion payload was not prepared")
	}
	ref, found := prepared[string(scope.organization)+"/"+artifact.Digest(content)]
	if !found {
		return errors.New("conversation conversion payload was not prepared")
	}
	verified, err := store.contents.ReadContent(ctx, string(scope.organization), ref)
	if err != nil {
		return err
	}
	if !bytes.Equal(verified, content) {
		return artifact.ErrIntegrity
	}
	if err := s.database.reader.QueryRowContext(ctx, "SELECT owner_principal_id FROM conversations WHERE id=?", id).Scan(&uploader); err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, safe, err := storageConversionStamp(ctx, tx, scope, family, id, now)
	if err != nil || !safe || current != stamp {
		return err
	}
	contentID, err := store.preparedRecordStorage(ctx, tx, string(scope.organization), string(scope.project), uploader, content, now)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, update, append([]any{contentID}, args...)...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if len(metadata.Columns) > 0 {
		if err := metadata.Write(ctx, tx, family.table(), "rowid=?", row); err != nil {
			return err
		}
	}
	if family.name == "conversation_attachments" {
		if _, err := tx.ExecContext(ctx, "DELETE FROM conversation_attachment_blobs WHERE artifact_ref=?", key); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, family.conversationPayloadCompletion(), row, reclaimed, id, formatHubTime(now), scope.organization, scope.project, family.name, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE attempt_diff_body_migration SET vacuum_pending=1 WHERE singleton=1"); err != nil {
		return err
	}
	return s.database.commit(ctx, tx)
}

func (f storageConversionFamily) conversationPayloadCompletion() string {
	const prefix = `UPDATE storage_conversion_progress SET cursor_rowid=?,converted_rows=converted_rows+1,reclaimed_bytes=reclaimed_bytes+?,complete=NOT EXISTS(`
	const suffix = `),updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`
	switch f.name {
	case "messages":
		return prefix + `SELECT 1 FROM conversation_messages WHERE conversation_id=? AND actor_principal_id IS NULL` + suffix
	case "conversation_events":
		return prefix + `SELECT 1 FROM conversation_events WHERE conversation_id=? AND content_attachment_id=''` + suffix
	case "conversation_records":
		return prefix + `SELECT 1 FROM conversations WHERE id=? AND conversion_state=1 AND content_attachment_id=''` + suffix
	case "conversation_questions":
		return prefix + `SELECT 1 FROM conversation_questions WHERE conversation_id=? AND conversion_state=1 AND content_attachment_id=''` + suffix
	case "conversation_commands":
		return prefix + `SELECT 1 FROM conversation_commands WHERE conversation_id=? AND conversion_state=1 AND content_attachment_id=''` + suffix
	case "conversation_attachments":
		return prefix + `SELECT 1 FROM conversation_attachments WHERE conversation_id=? AND conversion_state=1 AND content_attachment_id='' AND deleted_at IS NULL` + suffix
	default:
		return ""
	}
}
