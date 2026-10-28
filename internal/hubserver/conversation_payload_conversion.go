package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func (s *Service) convertConversationPayload(ctx context.Context, scope nativeScope, family storageConversionFamily, id string, now time.Time) error {
	if s.conversations == nil || s.conversations.store.contents == nil {
		return errors.New("conversation content storage is unavailable")
	}
	store := s.conversations.store
	stamp, safe, err := storageConversionStamp(ctx, s.database.reader, scope, family, id, now)
	if err != nil || !safe {
		return err
	}
	var row int64
	err = s.database.reader.QueryRowContext(ctx, "SELECT r.rowid FROM "+family.from+" WHERE c.organization_id=? AND c.project_id=? AND r.conversation_id=? AND "+family.where+" ORDER BY r.rowid LIMIT 1", scope.organization, scope.project, id).Scan(&row)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.database.db.ExecContext(ctx, `UPDATE storage_conversion_progress SET complete=1,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, formatHubTime(now), scope.organization, scope.project, family.name, id)
		return err
	}
	if err != nil {
		return err
	}
	if family.name == "tool_revisions" {
		var messageID string
		if err := s.database.reader.QueryRowContext(ctx, "SELECT message_id FROM conversation_tool_revisions WHERE rowid=?", row).Scan(&messageID); err != nil {
			return err
		}
		_, err := s.convertConversationToolPayload(ctx, scope, family, id, messageID, stamp, now)
		return err
	}
	var data []byte
	var original, oldContent, actor string
	var message conversationMessageRecord
	var metadata conversationQueryMetadata
	if family.name == "messages" {
		message, err = scanConversationMessage(s.database.reader.QueryRowContext(ctx, "SELECT "+conversationMessageReadColumns+" FROM conversation_messages WHERE rowid=?", row))
		if err != nil {
			return err
		}
		converted, err := s.convertConversationToolPayload(ctx, scope, family, id, message.ID, stamp, now)
		if err != nil || converted {
			return err
		}
		original, actor, oldContent = message.Text, string(message.Data), message.ContentID
		if err := store.resolveMessageContents(ctx, s.database.reader, []*conversationMessageRecord{&message}); err != nil {
			return err
		}
		data, err = messageContent(message)
		if err == nil {
			metadata, err = messageQueryMetadata(message.Data, message.Actor.Kind)
		}
		metadata.add("actor_principal_id", message.Actor.PrincipalID)
		metadata.add("text_bytes", len(message.Text))
	} else {
		if err := s.database.reader.QueryRowContext(ctx, "SELECT body_json,content_attachment_id FROM conversation_events WHERE rowid=?", row).Scan(&original, &oldContent); err != nil {
			return err
		}
		data, err = resolveStorageJSONWith(ctx, []byte(original), store.contents.ReadContent)
		if err == nil && oldContent != "" {
			data, err = store.readEventStorage(ctx, s.database.reader, id, oldContent)
		}
		if err == nil {
			metadata, err = eventQueryMetadata(data)
		}
	}
	if err != nil {
		return err
	}
	ref, err := store.contents.PublishContent(ctx, string(scope.organization), data)
	if err != nil {
		return fmt.Errorf("publish legacy conversation payload: %w", err)
	}
	if err := ref.Validate(string(scope.organization)); err != nil {
		return err
	}
	verified, err := store.contents.ReadContent(ctx, string(scope.organization), ref)
	if err != nil {
		return err
	}
	if !bytes.Equal(verified, data) {
		return artifact.ErrIntegrity
	}
	var uploader string
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
	if err := store.recordContentArchive(ctx, tx, string(scope.organization), string(scope.project), uploader, ref, now); err != nil {
		return err
	}
	contentID := contentArchiveID(string(scope.organization), ref)
	var result sql.Result
	if family.name == "messages" {
		result, err = tx.ExecContext(ctx, `UPDATE conversation_messages SET text='',data_json='{}',actor_json='{}',content_attachment_id=?,conversion_state=2 WHERE rowid=? AND revision=? AND text=? AND data_json=? AND content_attachment_id=? AND actor_principal_id IS NULL`, contentID, row, message.Revision, original, actor, oldContent)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE conversation_events SET body_json='{}',content_attachment_id=?,conversion_state=2 WHERE rowid=? AND body_json=? AND content_attachment_id=? AND content_attachment_id=''`, contentID, row, original, oldContent)
	}
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if err := metadata.write(ctx, tx, family.table(), "rowid=?", row); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET cursor_rowid=?,converted_rows=converted_rows+1,reclaimed_bytes=reclaimed_bytes+?,complete=NOT EXISTS(SELECT 1 FROM `+family.table()+` WHERE conversation_id=? AND `+family.pending()+`),updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, row, max(0, len(original)+len(actor)-4), id, formatHubTime(now), scope.organization, scope.project, family.name, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE attempt_diff_body_migration SET vacuum_pending=1 WHERE singleton=1"); err != nil {
		return err
	}
	return s.database.commit(ctx, tx)
}

func (s *Service) convertConversationToolPayload(ctx context.Context, scope nativeScope, family storageConversionFamily, id, messageID, stamp string, now time.Time) (bool, error) {
	var row, revision int64
	var text, tool, item, status string
	err := s.database.reader.QueryRowContext(ctx, `SELECT rowid,revision,text,tool,item_id,status FROM conversation_tool_revisions WHERE conversation_id=? AND message_id=? AND content_attachment_id='' ORDER BY revision LIMIT 1`, id, messageID).Scan(&row, &revision, &text, &tool, &item, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := json.Marshal(map[string]string{"tool": tool, "item_id": item, "status": status})
	if err != nil {
		return false, err
	}
	content, err := json.Marshal(conversationContent{Text: text, Data: data})
	if err != nil {
		return false, err
	}
	store := s.conversations.store
	ref, err := store.contents.PublishContent(ctx, string(scope.organization), content)
	if err != nil {
		return false, err
	}
	if err := ref.Validate(string(scope.organization)); err != nil {
		return false, err
	}
	verified, err := store.contents.ReadContent(ctx, string(scope.organization), ref)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(verified, content) {
		return false, artifact.ErrIntegrity
	}
	var uploader string
	if err := s.database.reader.QueryRowContext(ctx, "SELECT owner_principal_id FROM conversations WHERE id=?", id).Scan(&uploader); err != nil {
		return false, err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, safe, err := storageConversionStamp(ctx, tx, scope, family, id, now)
	if err != nil || !safe || current != stamp {
		return false, err
	}
	if err := store.recordContentArchive(ctx, tx, string(scope.organization), string(scope.project), uploader, ref, now); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversation_tool_revisions SET text='',content_attachment_id=?,conversion_state=2 WHERE conversation_id=? AND message_id=? AND revision=? AND text=? AND tool=? AND item_id=? AND status=? AND content_attachment_id=''`, contentArchiveID(string(scope.organization), ref), id, messageID, revision, text, tool, item, status)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	if family.name == "tool_revisions" {
		if _, err := tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET cursor_rowid=?,converted_rows=converted_rows+1,reclaimed_bytes=reclaimed_bytes+?,complete=NOT EXISTS(SELECT 1 FROM conversation_tool_revisions WHERE conversation_id=? AND content_attachment_id=''),updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, row, len(text), id, formatHubTime(now), scope.organization, scope.project, family.name, id); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE attempt_diff_body_migration SET vacuum_pending=1 WHERE singleton=1"); err != nil {
		return false, err
	}
	return true, s.database.commit(ctx, tx)
}
