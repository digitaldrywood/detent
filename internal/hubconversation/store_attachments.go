package hubconversation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"time"
)

const conversationEventPage = 200

// readAttachment reads one live attachment of a conversation. A tombstone
// reads as not found: the bytes are gone either way.
func (s *Store) ReadAttachment(ctx context.Context, query Queryer, conversationID, id string) (AttachmentRecord, error) {
	row := query.QueryRowContext(ctx, "SELECT "+attachmentColumns+attachmentFrom+
		"a.id = ? AND a.conversation_id = ? AND a.deleted_at IS NULL", id, conversationID)
	record, err := s.ScanAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record, sql.ErrNoRows
	}
	if err != nil {
		return record, fmt.Errorf("read attachment: %w", err)
	}
	return record, nil
}

// readAttachmentByRef reads the attachment an upload key already created,
// including a tombstone: a replay of a key whose attachment is gone is a
// conflict, not a fresh upload.
func (s *Store) ReadAttachmentByRef(ctx context.Context, query Queryer, ref string) (AttachmentRecord, bool, error) {
	row := query.QueryRowContext(ctx, "SELECT "+attachmentColumns+attachmentFrom+"a.artifact_ref = ?", ref)
	record, err := s.ScanAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, fmt.Errorf("read attachment by reference: %w", err)
	}
	return record, true, nil
}

func (s *Store) InsertAttachment(ctx context.Context, tx *sql.Tx, record AttachmentRecord, content []byte) error {
	contentID, err := s.PreparedRecordStorage(ctx, tx, string(record.OrganizationID), string(record.ProjectID), record.PrincipalID, content, record.CreatedAt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_attachments
 (id, conversation_id, message_id, principal_id, name, mime, size, artifact_ref, created_at, expires_at, deleted_at, content_attachment_id,conversion_state)
 VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, ?,2)`,
		record.ID, record.ConversationID, record.PrincipalID, record.Name, record.MIME, record.Size,
		record.ArtifactRef, Time(record.CreatedAt), NullTime(record.ExpiresAt), contentID); err != nil {
		return fmt.Errorf("insert attachment: %w", err)
	}
	return nil
}

func (s *Store) ReadAttachmentContent(ctx context.Context, query Queryer, ref string) ([]byte, error) {
	var organization, contentID string
	if err := query.QueryRowContext(ctx, `SELECT c.organization_id,a.content_attachment_id FROM conversation_attachments a JOIN conversations c ON c.id=a.conversation_id WHERE a.artifact_ref=? AND a.deleted_at IS NULL`, ref).Scan(&organization, &contentID); err != nil {
		return nil, err
	}
	if contentID != "" {
		return s.ReadConversationArchive(ctx, query, organization, contentID)
	}
	var content []byte
	if err := query.QueryRowContext(ctx, "SELECT content FROM conversation_attachment_blobs WHERE artifact_ref = ?", ref).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("read attachment content: %w", err)
	}
	return content, nil
}

// deleteAttachment tombstones an attachment and drops its bytes.
func (s *Store) DeleteAttachment(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_attachment_blobs WHERE artifact_ref IN
 (SELECT artifact_ref FROM conversation_attachments WHERE id = ?)`, id); err != nil {
		return fmt.Errorf("delete attachment content: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversation_attachments SET deleted_at = ?, expires_at = NULL WHERE id = ?", Time(now), id); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	return nil
}

// bindAttachment binds one unsent attachment to a message. The WHERE clause
// carries the whole rule, so two commands racing for the same attachment
// cannot both win: the second updates no row.
func (s *Store) BindAttachment(ctx context.Context, tx *sql.Tx, conversationID, principalID, attachmentID, messageID string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE conversation_attachments SET message_id = ?, expires_at = NULL
 WHERE id = ? AND conversation_id = ? AND principal_id = ? AND message_id IS NULL AND deleted_at IS NULL`,
		messageID, attachmentID, conversationID, principalID)
	if err != nil {
		return false, fmt.Errorf("bind attachment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("bind attachment: %w", err)
	}
	return affected == 1, nil
}

// listExpiredAttachments returns abandoned uploads: unsent, not deleted and
// past their expiry.
func (s *Store) ListExpiredAttachments(ctx context.Context, query Queryer, now time.Time, limit int) ([]AttachmentRecord, error) {
	rows, err := query.QueryContext(ctx, "SELECT "+attachmentColumns+attachmentFrom+
		"a.message_id IS NULL AND a.deleted_at IS NULL AND a.expires_at IS NOT NULL AND a.expires_at <= ? ORDER BY a.expires_at LIMIT ?",
		Time(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	expired := make([]AttachmentRecord, 0, limit)
	for rows.Next() {
		record, err := s.ScanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("list expired attachments: %w", err)
		}
		expired = append(expired, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired attachments: %w", err)
	}
	return expired, nil
}

func (s *Store) PrepareLegacyToolRevision(ctx context.Context, id, messageID string) (context.Context, error) {
	prepared, ok := ctx.Value(LegacyToolRevisionKey{}).(map[string]LegacyToolRevision)
	if !ok || prepared == nil {
		prepared = map[string]LegacyToolRevision{}
	}
	if _, ready := prepared[messageID]; ready {
		return ctx, nil
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT id,data_json,actor_json,text,revision,query_metadata_version,actor_kind,tool_name,tool_item_id,tool_status FROM conversation_messages WHERE conversation_id=? AND id=? AND kind='tool' AND content_attachment_id=''`, id, messageID)
	if err != nil {
		return ctx, err
	}
	defer rows.Close()
	var pending []LegacyToolRevision
	for rows.Next() {
		var id string
		var c LegacyToolRevision
		var version int
		var actorKind, tool, item, status sql.NullString
		if err := rows.Scan(&id, &c.raw, &c.actor, &c.text, &c.revision, &version, &actorKind, &tool, &item, &status); err != nil {
			return ctx, err
		}
		if err := json.Unmarshal([]byte(c.raw), &c.data); err != nil {
			return ctx, err
		}
		var actor conversation.Actor
		if err := json.Unmarshal([]byte(c.actor), &actor); err != nil {
			return ctx, err
		}
		c.actorKind = actor.Kind
		if version == 1 {
			c.actorKind = conversation.ActorKind(actorKind.String)
			c.data.Tool, c.data.ItemID, c.data.Status = &tool.String, &item.String, &status.String
		}
		value := func(value *string) string {
			if value == nil {
				return ""
			}
			return *value
		}
		c.payload, err = json.Marshal(map[string]string{"tool": value(c.data.Tool), "item_id": value(c.data.ItemID), "status": value(c.data.Status)})
		if err != nil {
			return ctx, err
		}
		prepared[id] = c
		pending = append(pending, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return ctx, err
	}
	if len(pending) > 0 {
		var organization string
		if err := s.reader.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", id).Scan(&organization); err != nil {
			return ctx, err
		}
		var contents [][]byte
		for _, old := range pending {
			if old.actorKind != conversation.ActorCoordinator {
				continue
			}
			content, err := MessageContent(MessageRecord{Text: old.text, Data: old.payload})
			if err != nil {
				return ctx, err
			}
			contents = append(contents, content)
		}
		ctx, err = s.PreparePayloads(ctx, organization, contents...)
		if err != nil {
			return ctx, err
		}
	}
	return context.WithValue(ctx, LegacyToolRevisionKey{}, prepared), nil
}

func (s *Store) PreserveLegacyToolRevision(ctx context.Context, tx *sql.Tx, message MessageRecord) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE conversation_id=? AND id=? AND kind='tool' AND content_attachment_id='')`, message.ConversationID, message.ID).Scan(&pending); err != nil || !pending {
		return err
	}
	prepared, ready := ctx.Value(LegacyToolRevisionKey{}).(map[string]LegacyToolRevision)
	if !ready {
		return errors.New("conversation tool revision was not prepared")
	}
	old, ok := prepared[message.ID]
	if !ok {
		return errors.New("conversation tool revision was not prepared")
	}
	var unchanged bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE conversation_id=? AND id=? AND data_json=? AND actor_json=? AND text=? AND revision=?)`, message.ConversationID, message.ID, old.raw, old.actor, old.text, old.revision).Scan(&unchanged); err != nil {
		return err
	}
	if !unchanged {
		return errors.New("conversation tool revision changed after preparation")
	}
	if old.actorKind != conversation.ActorCoordinator {
		return nil
	}
	value := func(p *string) string {
		if p != nil {
			return *p
		}
		return ""
	}
	_, _, contentID, err := s.MessageStorage(ctx, tx, MessageRecord{ConversationID: message.ConversationID, Text: old.text, Data: old.payload, CreatedAt: message.CreatedAt})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_tool_revisions(conversation_id,message_id,revision,text,tool,item_id,status,content_attachment_id) VALUES(?,?,?,'',?,?,?,?)`, message.ConversationID, message.ID, old.revision, value(old.data.Tool), value(old.data.ItemID), value(old.data.Status), contentID)
	return err
}

func (s *Store) DeleteConversation(ctx context.Context, tx *sql.Tx, record Record) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_references WHERE conversation_id = ? OR (target_kind = 'conversation' AND target_id = ?)`, record.ID, record.ID); err != nil {
		return fmt.Errorf("delete conversation references: %w", err)
	}
	statements := []string{
		`DELETE FROM conversation_attachment_blobs WHERE artifact_ref IN (SELECT artifact_ref FROM conversation_attachments WHERE conversation_id = ?)`,
		`DELETE FROM conversation_attachments WHERE conversation_id = ?`,
		`DELETE FROM conversation_turn_batches WHERE attempt_id IN (SELECT attempt_id FROM conversation_starts WHERE conversation_id = ?)`,
		`DELETE FROM conversation_starts WHERE conversation_id = ?`,
		`DELETE FROM conversation_audience_events WHERE conversation_id = ?`,
		`DELETE FROM conversation_events WHERE conversation_id = ?`,
		`DELETE FROM conversation_commands WHERE conversation_id = ?`,
		`DELETE FROM conversation_questions WHERE conversation_id = ?`,
		`DELETE FROM conversation_messages WHERE conversation_id = ?`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, record.ID); err != nil {
			return fmt.Errorf("delete conversation history: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND organization_id = ? AND project_id = ?`, record.ID, record.OrganizationID, record.ProjectID); err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	return nil
}

func (s *Store) ReplayEvents(ctx context.Context, reader *sql.DB, id string, after int64) ([]conversation.Event, error) {
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var through sql.NullInt64
	var raw sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT s.through_sequence,s.summary_json FROM conversations c LEFT JOIN event_compactions s ON s.aggregate_kind='conversation' AND s.aggregate_id=c.id AND s.organization_id=c.organization_id AND s.project_id=c.project_id WHERE c.id=?`, id).Scan(&through, &raw); err != nil {
		return nil, err
	}
	if !through.Valid && raw.Valid {
		var summary tracker.EventCompaction
		if err := json.Unmarshal([]byte(raw.String), &summary); err != nil {
			return nil, err
		}
		through.Int64 = summary.ThroughSequence
	}
	if after < through.Int64 {
		return nil, s.host.CursorExpired()
	}
	return s.ListEvents(ctx, tx, id, after, conversationEventPage)
}

// AttachmentRef is the storage key of an upload. It is derived
// from the conversation, the uploading principal and the idempotency key, so
// a retried upload lands on the row it already created instead of a second
// copy, and the unique constraint on the column is the idempotency record.
func AttachmentRef(conversationID, principalID, key string) string {
	sum := sha256.Sum256([]byte(conversationID + "\x00" + principalID + "\x00" + key))
	return "conv:" + conversationID + ":" + hex.EncodeToString(sum[:])
}

const attachmentColumns = `a.id, a.conversation_id, c.organization_id, c.project_id, coalesce(a.message_id, ''), a.principal_id,
 a.name, a.mime, a.size, a.artifact_ref, a.created_at, a.expires_at, a.deleted_at, a.content_attachment_id`

const attachmentFrom = ` FROM conversation_attachments a JOIN conversations c ON c.id = a.conversation_id WHERE `

func (s *Store) ScanAttachment(row Scanner) (AttachmentRecord, error) {
	var record AttachmentRecord
	var created string
	var expires, deleted sql.NullString
	if err := row.Scan(&record.ID, &record.ConversationID, &record.OrganizationID, &record.ProjectID, &record.MessageID,
		&record.PrincipalID, &record.Name, &record.MIME, &record.Size, &record.ArtifactRef, &created, &expires, &deleted, &record.ContentID); err != nil {
		return record, err
	}
	var err error
	if record.CreatedAt, err = s.host.ParseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode attachment %s created_at: %w", record.ID, err)
	}
	if record.ExpiresAt, err = s.ParseNullTime(expires); err != nil {
		return record, fmt.Errorf("decode attachment %s expires_at: %w", record.ID, err)
	}
	if record.DeletedAt, err = s.ParseNullTime(deleted); err != nil {
		return record, fmt.Errorf("decode attachment %s deleted_at: %w", record.ID, err)
	}
	return record, nil
}

// resolveMessageAttachments fills the attachments of every message in one
// query, the way resolveMessageReferences fills references. The hub pool
// holds a single connection, so the caller must have drained its own rows.
func (s *Store) ResolveMessageAttachments(ctx context.Context, query Queryer, messages []*MessageRecord) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := s.host.StringList(ids)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	rows, err := s.host.QuerySQL(ctx, query, "SELECT "+attachmentColumns+attachmentFrom+
		"a.message_id IN (SELECT value FROM sql_rows(?)) AND a.deleted_at IS NULL ORDER BY a.message_id, a.created_at, a.id", encoded)
	if err != nil {
		return fmt.Errorf("read message attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byMessage := map[string][]AttachmentRecord{}
	for rows.Next() {
		record, err := s.ScanAttachment(rows)
		if err != nil {
			return fmt.Errorf("read message attachments: %w", err)
		}
		byMessage[record.MessageID] = append(byMessage[record.MessageID], record)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read message attachments: %w", err)
	}
	for _, message := range messages {
		message.Attachments = byMessage[message.ID]
	}
	return nil
}

type LegacyToolRevisionKey struct{}

type LegacyToolRevision struct {
	payload          json.RawMessage
	raw, actor, text string
	revision         int64
	data             QueryMessageData
	actorKind        conversation.ActorKind
}

// DeltaBody is the message.delta event body. seq orders the
// deltas of one message and identifies a re-delivered one: it is the byte
// offset the text was appended at, so it is monotonic per message and stable
// across a retry.
func DeltaBody(messageID string, seq int64, text string) map[string]any {
	return map[string]any{"message_id": messageID, "seq": seq, "text": text}
}

// resolveMessageReferences fills the references of every message in one
// query. The hub pool holds a single connection, so the caller must have
// drained its own rows first. The rows come back in insertion order, which
// is the order the tokens appeared in the message text: replaceMessageReferences
// rewrites a message's rows as a set, so rowid order is extraction order.
func (s *Store) ResolveMessageReferences(ctx context.Context, query Queryer, messages []*MessageRecord) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := s.host.StringList(ids)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	rows, err := s.host.QuerySQL(ctx, query, `SELECT r.message_id, r.target_kind, r.target_id, r.label,
CASE WHEN r.target_kind='issue' THEN '/' || o.slug || '/' || p.slug || '/issues/' || i.number ELSE '/chat/c/' || r.target_id END
FROM message_references r LEFT JOIN issues i ON r.target_kind='issue' AND i.native_id=r.target_id LEFT JOIN projects p ON p.id=i.project_id LEFT JOIN organizations o ON o.id=i.organization_id
WHERE r.message_id IN (SELECT value FROM sql_rows(?)) ORDER BY r.message_id, r.rowid`, encoded)
	if err != nil {
		return fmt.Errorf("read message references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byMessage := map[string][]MessageReference{}
	for rows.Next() {
		var messageID string
		var reference MessageReference
		if err := rows.Scan(&messageID, &reference.Kind, &reference.ID, &reference.Label, &reference.URL); err != nil {
			return fmt.Errorf("read message references: %w", err)
		}
		byMessage[messageID] = append(byMessage[messageID], reference)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read message references: %w", err)
	}
	for _, message := range messages {
		message.References = byMessage[message.ID]
	}
	return nil
}
