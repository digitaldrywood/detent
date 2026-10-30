package hubconversation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"math"
	"strings"
	"sync"
	"time"
)

const hubReadTimeout = 15 * time.Second
const storageConversionRows = 100

func (s *Store) PublishPayload(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if s.contents == nil {
		return attachment.ArchiveReference{}, errors.New("conversation content storage is unavailable")
	}
	expected, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, publishErr := s.contents.PublishContent(ctx, organization, content)
	if publishErr != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubReadTimeout)
		defer cancel()
		stored, readErr := s.contents.ReadContent(reconcileCtx, organization, expected)
		if readErr != nil || !bytes.Equal(stored, content) {
			return attachment.ArchiveReference{}, errors.Join(ErrPublicationUncertain, publishErr, readErr)
		}
		ref = expected
	}
	if ref != expected {
		return attachment.ArchiveReference{}, errors.New("conversation archive reference changed during publication")
	}
	return ref, nil
}

func (s *Store) PreparePayload(ctx context.Context, organization string, content []byte) (context.Context, error) {
	return s.PreparePayloads(ctx, organization, content)
}

func (s *Store) PreparePayloads(ctx context.Context, organization string, contents ...[]byte) (context.Context, error) {
	ctx = WithArchiveReads(ctx)
	prepared := PreparedContents{}
	if existing, ok := ctx.Value(ContentsKey{}).(PreparedContents); ok {
		for key, ref := range existing {
			prepared[key] = ref
		}
	}
	reads := ArchiveCache(ctx)
	for _, content := range contents {
		key := organization + "/" + artifact.Digest(content)
		ref, found := prepared[key]
		if !found {
			var err error
			ref, err = s.PublishPayload(ctx, organization, content)
			if err != nil {
				return ctx, err
			}
			prepared[key] = ref
		}
		reads.Store(organization+"/"+ContentArchiveID(organization, ref), content)
	}
	return context.WithValue(ctx, ContentsKey{}, prepared), nil
}

func (s *Store) MessageStorage(ctx context.Context, tx *sql.Tx, message MessageRecord) (string, string, string, error) {
	if s.contents == nil {
		return "", "", "", errors.New("conversation content storage is unavailable")
	}
	content, err := MessageContent(message)
	if err != nil {
		return "", "", "", err
	}
	digest := artifact.Digest(content)
	if message.ContentID != "" && digest == message.ContentHash {
		return "", "{}", message.ContentID, nil
	}
	var organization, project, uploader string
	if err := tx.QueryRowContext(ctx, "SELECT organization_id,project_id,owner_principal_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization, &project, &uploader); err != nil {
		return "", "", "", err
	}
	id, err := s.PreparedRecordStorage(ctx, tx, organization, project, uploader, content, message.CreatedAt)
	if err != nil {
		return "", "", "", err
	}

	return "", "{}", id, nil
}

func (s *Store) ResolveMessageContents(ctx context.Context, query Queryer, messages []*MessageRecord) error {
	cache, ok := ctx.Value(conversationReadContentsKey{}).(conversationContentReads)
	if !ok || cache == nil {
		cache = conversationContentReads{}
	}
	for _, message := range messages {
		if message.ContentID == "" {
			continue
		}
		var organization string
		if err := query.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization); err != nil {
			return err
		}
		key := organization + "/" + message.ContentID
		if cached, ok := cache[key]; ok {
			message.Text, message.Data, message.ContentHash = cached.content.Text, cached.content.Data, cached.hash
			continue
		}
		content, err := s.ReadConversationArchive(ctx, query, organization, message.ContentID)
		if err != nil {
			return err
		}
		var decoded Content
		if err := json.Unmarshal(content, &decoded); err != nil {
			return fmt.Errorf("decode conversation content: %w", err)
		}
		message.Text, message.Data, message.ContentHash = decoded.Text, decoded.Data, artifact.Digest(content)
		cache[key] = readConversationContent{content: decoded, hash: message.ContentHash}
	}
	for _, message := range messages {
		if message.Role != conversation.RoleAssistant || message.TextBytes <= int64(len(message.Text)) {
			continue
		}
		if err := s.ResolveMessageDeltas(ctx, query, message); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ResolveMessageDeltas(ctx context.Context, q Queryer, message *MessageRecord) error {
	batch, err := readConversationMessageDeltas(ctx, q, *message)
	if err != nil {
		return err
	}
	for i := range batch {
		if batch[i].contentID != "" {
			batch[i].raw, err = s.ReadEventStorage(ctx, q, message.ConversationID, batch[i].contentID)
			if err != nil {
				return err
			}
		}
	}
	return applyConversationMessageDeltas(message, batch)
}

func (s *Store) ReadConversationArchive(ctx context.Context, query Queryer, organization, contentID string) ([]byte, error) {
	cache := ArchiveCache(ctx)
	key := organization + "/" + contentID
	if data, ok := cache.Load(key); ok {
		return data, nil
	}
	if _, transactional := query.(*sql.Tx); transactional && s.host.HasPublicationPlan(ctx) {
		return nil, errors.New("conversation archive was not read before the transaction")
	}
	ref, err := ScanArchive(query.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, contentID, organization))
	if err != nil {
		return nil, err
	}
	return s.ReadCapturedConversationArchive(ctx, organization, contentID, ref)
}

func (s *Store) ReadCapturedConversationArchive(ctx context.Context, organization, contentID string, ref attachment.ArchiveReference) ([]byte, error) {
	cache := ArchiveCache(ctx)
	key := organization + "/" + contentID
	if data, ok := cache.Load(key); ok {
		return data, nil
	}
	if s.contents == nil {
		return nil, errors.New("conversation content storage is unavailable")
	}
	data, err := s.contents.ReadContent(ctx, organization, ref)
	if err != nil {
		return nil, fmt.Errorf("read conversation content: %w", err)
	}
	cache.Store(key, data)
	cache.RememberReference(organization, ref, data)
	return data, nil
}

func (s *Store) ResolveEventContent(ctx context.Context, query Queryer, id string, events []conversation.Event) error {
	ctx = context.WithValue(ctx, conversationReadContentsKey{}, conversationContentReads{})
	for index := range events {
		event := &events[index]
		resolved, err := s.host.ResolveStorageJSONWith(ctx, event.Data, func(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
			if s.contents == nil {
				return nil, errors.New("conversation content storage is unavailable")
			}
			return s.contents.ReadContent(ctx, organization, ref)
		})
		if err != nil {
			return err
		}
		event.Data = resolved
		var body any
		switch event.Type {
		case conversation.EventMessageAccepted, conversation.EventMessageUpdated:
			var fact MessageFact
			if err := json.Unmarshal(event.Data, &fact); err != nil {
				return err
			}
			if fact.MessageID == "" || fact.Revision == 0 {
				continue
			}
			message, err := s.ScanMessage(query.QueryRowContext(ctx, "SELECT "+MessageReadColumns+" FROM conversation_messages WHERE conversation_id=? AND id=?", id, fact.MessageID))
			if err != nil {
				return fmt.Errorf("resolve event message: %w", err)
			}
			if fact.ContentID != "" && message.Role != conversation.RoleAssistant {
				message.ContentID = fact.ContentID
			} else if message.Kind == conversation.MessageTool && message.Actor.Kind == conversation.ActorCoordinator {
				var text, tool, item, status, contentID string
				err := query.QueryRowContext(ctx, `SELECT text,tool,item_id,status,content_attachment_id FROM conversation_tool_revisions WHERE conversation_id=? AND message_id=? AND revision=?`, id, fact.MessageID, fact.Revision).Scan(&text, &tool, &item, &status, &contentID)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("resolve inline tool revision: %w", err)
				}
				if err == nil {
					message.Text, message.ContentID = text, contentID
					message.Data, err = json.Marshal(map[string]string{"tool": tool, "item_id": item, "status": status})
					if err != nil {
						return err
					}
				}
			}
			if err := s.ResolveMessageContents(ctx, query, []*MessageRecord{&message}); err != nil {
				return err
			}
			if fact.TextBytes < 0 || fact.TextBytes > len(message.Text) {
				return errors.New("event message text boundary is invalid")
			}
			message.Text = message.Text[:fact.TextBytes]
			message.Delivery, message.UpdatedAt = fact.Delivery, fact.UpdatedAt
			message.AttemptID = ""
			if fact.AttemptID != nil {
				message.AttemptID = *fact.AttemptID
			}
			message.TurnID = ""
			if fact.TurnID != nil {
				message.TurnID = *fact.TurnID
			}
			if fact.EmptyData {
				message.Data = json.RawMessage("{}")
			}
			messages := []*MessageRecord{&message}
			if err := s.host.ResolveMessageReferences(ctx, query, messages); err != nil {
				return err
			}
			if err := s.host.ResolveMessageAttachments(ctx, query, messages); err != nil {
				return err
			}
			body = s.host.ProjectMessage(message)
		case conversation.EventMessageDelta:
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &fields); err != nil {
				return err
			}
			if _, ok := fields["text_bytes"]; !ok {
				continue
			}
			var fact DeltaFact
			if err := json.Unmarshal(event.Data, &fact); err != nil {
				return err
			}
			if fact.Text != "" {
				if len(fact.Text) != fact.Bytes {
					return errors.New("event delta text length is invalid")
				}
				encoded, err := json.Marshal(s.host.ConversationDeltaBody(fact.MessageID, fact.Offset, fact.Text))
				if err != nil {
					return err
				}
				event.Data = encoded
				continue
			}
			message, err := s.ScanMessage(query.QueryRowContext(ctx, "SELECT "+MessageReadColumns+" FROM conversation_messages WHERE conversation_id=? AND id=?", id, fact.MessageID))
			if err != nil {
				return err
			}
			if err := s.ResolveMessageContents(ctx, query, []*MessageRecord{&message}); err != nil {
				return err
			}
			if fact.Offset < 0 || fact.Bytes < 0 || fact.Offset > int64(len(message.Text))-int64(fact.Bytes) {
				return errors.New("event delta text boundary is invalid")
			}
			text := message.Text[fact.Offset : fact.Offset+int64(fact.Bytes)]
			body = s.host.ConversationDeltaBody(fact.MessageID, fact.Offset, text)
		case conversation.EventQuestionOpened, conversation.EventQuestionUpdated:
			var fact QuestionFact
			if err := json.Unmarshal(event.Data, &fact); err != nil {
				return err
			}
			if fact.QuestionID == "" {
				continue
			}
			question, err := s.ReadQuestion(ctx, query, id, fact.QuestionID)
			if err != nil {
				return fmt.Errorf("resolve event question: %w", err)
			}
			question.Status, question.AnsweredBy, question.UpdatedAt = fact.Status, fact.AnsweredBy, fact.UpdatedAt
			if !fact.Answered {
				question.Answers = nil
			}
			body = question.Question
		default:
			continue
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		event.Data = encoded
	}
	return nil
}

func (s *Store) EventStorage(ctx context.Context, tx *sql.Tx, id string, data []byte, now time.Time) (string, string, error) {
	if s.contents == nil {
		return "", "", errors.New("conversation content storage is unavailable")
	}
	var organization, project, uploader string
	if err := tx.QueryRowContext(ctx, "SELECT organization_id,project_id,owner_principal_id FROM conversations WHERE id=?", id).Scan(&organization, &project, &uploader); err != nil {
		return "", "", err
	}
	prepared, ok := ctx.Value(ContentsKey{}).(PreparedContents)
	ref, found := prepared[organization+"/"+artifact.Digest(data)]
	if !ok || !found {
		return "", "", errors.New("conversation event was not published before the transaction")
	}
	if err := ref.Validate(organization); err != nil {
		return "", "", err
	}
	if err := s.host.RecordContentArchive(ctx, tx, organization, project, uploader, ref, now); err != nil {
		return "", "", err
	}
	return "{}", ContentArchiveID(organization, ref), nil
}

func (s *Store) ReadEventStorage(ctx context.Context, q Queryer, id, contentID string) ([]byte, error) {
	var organization string
	if err := q.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", id).Scan(&organization); err != nil {
		return nil, err
	}
	return s.ReadConversationArchive(ctx, q, organization, contentID)
}

func (s *Store) PayloadStorage(ctx context.Context, tx *sql.Tx, id string, raw []byte, now time.Time) (string, error) {
	_, _, contentID, err := s.MessageStorage(ctx, tx, MessageRecord{ConversationID: id, Data: raw, CreatedAt: now})
	return contentID, err
}

func (s *Store) PreparedRecordStorage(ctx context.Context, tx *sql.Tx, organization, project, uploader string, content []byte, now time.Time) (string, error) {
	key := organization + "/" + artifact.Digest(content)
	prepared, ok := ctx.Value(ContentsKey{}).(PreparedContents)
	ref, found := prepared[key]
	if !ok || !found {
		ref, found = ArchiveCache(ctx).Reference(key)
	}
	if !found {
		return "", errors.New("conversation payload was not published before the transaction")
	}
	if err := ref.Validate(organization); err != nil {
		return "", err
	}
	if err := s.host.RecordContentArchive(ctx, tx, organization, project, uploader, ref, now); err != nil {
		return "", err
	}
	return ContentArchiveID(organization, ref), nil
}

func (s *Store) ConversationRecordStorage(ctx context.Context, tx *sql.Tx, record Record) (string, error) {
	raw, err := RecordPayload(record)
	if err != nil {
		return "", err
	}
	content, err := MessageContent(MessageRecord{Data: raw})
	if err != nil {
		return "", err
	}
	return s.PreparedRecordStorage(ctx, tx, string(record.OrganizationID), string(record.ProjectID), record.OwnerPrincipalID, content, record.CreatedAt)
}

func (s *Store) QuestionStorage(ctx context.Context, tx *sql.Tx, record QuestionRecord) (string, error) {
	raw, err := QuestionPayload(record)
	if err != nil {
		return "", err
	}
	return s.PayloadStorage(ctx, tx, record.ConversationID, raw, record.CreatedAt)
}

func (s *Store) ResolveConversationRecord(ctx context.Context, query Queryer, record *Record) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := s.ReadPayload(ctx, query, record.ID, record.ContentID)
	if err != nil {
		return err
	}
	var payload RecordContent
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	record.Execution, record.Preferences = payload.Execution, payload.Preferences.Normalized()
	return nil
}

func (s *Store) ReadConversationRecord(ctx context.Context, query Queryer, row Scanner) (Record, error) {
	record, err := s.ScanConversation(row)
	if err != nil {
		return record, err
	}
	err = s.ResolveConversationRecord(ctx, query, &record)
	return record, err
}

func (s *Store) DecodeReceipt(ctx context.Context, query Queryer, id, raw, contentID string) (*conversation.Receipt, error) {
	if contentID != "" {
		content, err := s.ReadPayload(ctx, query, id, contentID)
		if err != nil {
			return nil, err
		}
		raw = string(content)
	}
	return DecodeReceipt(raw)
}

func (s *Store) ResolveQuestion(ctx context.Context, query Queryer, record *QuestionRecord) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := s.ReadPayload(ctx, query, record.ConversationID, record.ContentID)
	if err != nil {
		return err
	}
	var payload QuestionContent
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	record.Prompts, record.Answers = payload.Prompts, payload.Answers
	return nil
}

func (s *Store) ReadPayload(ctx context.Context, query Queryer, id, contentID string) ([]byte, error) {
	var organization string
	if err := query.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", id).Scan(&organization); err != nil {
		return nil, err
	}
	content, err := s.ReadConversationArchive(ctx, query, organization, contentID)
	if err != nil {
		return nil, err
	}
	var payload Content
	if err := json.Unmarshal(content, &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

func (s *Store) MaterializeRestartedMessages(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	cursor := ""
	for {
		ctx := NewArchiveReads(ctx)
		message, err := s.ScanMessage(s.reader.QueryRowContext(ctx, "SELECT "+MessageReadColumns+" FROM conversation_messages WHERE id>? AND role='assistant' AND delivery='responding' AND conversation_id IN (SELECT id FROM conversations WHERE work_item_id IS NULL) ORDER BY id LIMIT 1", cursor))
		if errors.Is(err, sql.ErrNoRows) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		cursor = message.ID
		if err := s.ResolveMessageContents(ctx, s.reader, []*MessageRecord{&message}); err != nil {
			return count, err
		}
		var organization string
		if err := s.reader.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization); err != nil {
			return count, err
		}
		content, err := MessageContent(message)
		if err != nil {
			return count, err
		}
		ctx, err = s.PreparePayload(ctx, organization, content)
		if err != nil {
			return count, err
		}
		message.Delivery, message.UpdatedAt = conversation.DeliveryUnknown, now
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return count, err
		}
		err = func() error {
			var current bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE id=? AND revision=? AND delivery='responding')", message.ID, message.Revision).Scan(&current); err != nil {
				return err
			}
			if !current {
				return s.host.ConversationStale("Message changed during restart content preparation")
			}
			if err := s.UpdateMessage(ctx, tx, message); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			count++
			return nil
		}()
		if err := errors.Join(err, RollbackPreparation(tx)); err != nil {
			return count, err
		}
	}
}

func (s *Store) NormalizeRestartedRecords(ctx context.Context, now time.Time) (int64, int64, error) {
	conversations, err := s.NormalizeRestartedExecutions(ctx, now)
	if err != nil {
		return conversations, 0, err
	}
	receipts, err := s.NormalizeRestartedReceipts(ctx, now)
	return conversations, receipts, err
}

func (s *Store) NormalizeRestartedExecutions(ctx context.Context, now time.Time) (int64, error) {
	var cursor, changed int64
	for {
		type candidate struct {
			row, revision                         int64
			id, execution, preferences, contentID string
		}
		batch, err := func() ([]candidate, error) {
			rows, err := s.reader.QueryContext(ctx, `SELECT rowid,id,revision,execution_json,preferences_json,content_attachment_id FROM conversations WHERE rowid>? AND work_item_id IS NULL AND (execution_status IS NULL OR execution_status IN ('starting','running','waiting_input','interrupting')) ORDER BY rowid LIMIT ?`, cursor, storageConversionRows)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var batch []candidate
			for rows.Next() {
				var item candidate
				if err := rows.Scan(&item.row, &item.id, &item.revision, &item.execution, &item.preferences, &item.contentID); err != nil {
					rows.Close()
					return nil, err
				}
				batch = append(batch, item)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			return batch, nil
		}()
		if err != nil {
			return changed, err
		}
		if len(batch) == 0 {
			return changed, nil
		}
		for _, item := range batch {
			cursor = item.row
			record, err := s.ReadConversationByID(ctx, s.reader, item.id)
			if err != nil {
				return changed, err
			}
			if record.Revision != item.revision || record.ContentID != item.contentID {
				continue
			}
			switch record.Execution.Status {
			case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput, conversation.ExecutionInterrupting:
			default:
				continue
			}
			record.Execution = conversation.NormalizeExecutionValue(record.Execution)
			record.Execution.UpdatedAt = now.UTC()
			raw, err := RecordPayload(record)
			if err != nil {
				return changed, err
			}
			content, err := MessageContent(MessageRecord{Data: raw})
			if err != nil {
				return changed, err
			}
			prepared, err := s.PreparePayload(ctx, string(record.OrganizationID), content)
			if err != nil {
				return changed, err
			}
			count, err := func() (int64, error) {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return 0, err
				}
				defer tx.Rollback()
				contentID, err := s.ConversationRecordStorage(prepared, tx, record)
				if err != nil {
					return 0, err
				}
				result, err := tx.ExecContext(ctx, `UPDATE conversations SET execution_json='{}',preferences_json='{}',conversion_state=2,content_attachment_id=?,execution_status=?,execution_lease_id=?,execution_settled=?,query_metadata_version=1,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND content_attachment_id=? AND execution_json=? AND preferences_json=?`, contentID, record.Execution.Status, record.Execution.Owner.LeaseID, record.Execution.Settled, Time(now), item.id, item.revision, item.contentID, item.execution, item.preferences)
				if err != nil {
					return 0, err
				}
				count, err := result.RowsAffected()
				if err != nil || count == 0 {
					return 0, err
				}
				return count, tx.Commit()
			}()
			if err != nil {
				return changed, err
			}
			changed += count
		}
	}
}

func (s *Store) NormalizeRestartedReceipts(ctx context.Context, now time.Time) (int64, error) {
	var cursor, changed int64
	for {
		type candidate struct {
			row                               int64
			conversation, key, raw, contentID string
		}
		batch, err := func() ([]candidate, error) {
			rows, err := s.reader.QueryContext(ctx, `SELECT rowid,conversation_id,key,receipt_json,content_attachment_id FROM conversation_commands WHERE rowid>? AND conversation_id IN (SELECT id FROM conversations WHERE work_item_id IS NULL) AND (receipt_status IS NULL OR receipt_status IN ('sending','sent','responding')) ORDER BY rowid LIMIT ?`, cursor, storageConversionRows)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var batch []candidate
			for rows.Next() {
				var item candidate
				if err := rows.Scan(&item.row, &item.conversation, &item.key, &item.raw, &item.contentID); err != nil {
					rows.Close()
					return nil, err
				}
				batch = append(batch, item)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			return batch, nil
		}()
		if err != nil {
			return changed, err
		}
		if len(batch) == 0 {
			return changed, nil
		}
		for _, item := range batch {
			cursor = item.row
			receipt, err := s.DecodeReceipt(ctx, s.reader, item.conversation, item.raw, item.contentID)
			if err != nil {
				return changed, err
			}
			switch receipt.Status {
			case conversation.DeliverySending, conversation.DeliverySent, conversation.DeliveryResponding:
			default:
				continue
			}
			receipt.Status, receipt.UpdatedAt = conversation.DeliveryUnknown, now.UTC()
			raw, err := json.Marshal(receipt)
			if err != nil {
				return changed, err
			}
			content, err := MessageContent(MessageRecord{Data: raw})
			if err != nil {
				return changed, err
			}
			var organization string
			if err := s.reader.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", item.conversation).Scan(&organization); err != nil {
				return changed, err
			}
			prepared, err := s.PreparePayload(ctx, organization, content)
			if err != nil {
				return changed, err
			}
			count, err := func() (int64, error) {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return 0, err
				}
				defer tx.Rollback()
				contentID, err := s.PayloadStorage(prepared, tx, item.conversation, raw, now)
				if err != nil {
					return 0, err
				}
				result, err := tx.ExecContext(ctx, `UPDATE conversation_commands SET receipt_json='{}',conversion_state=2,content_attachment_id=?,receipt_status=?,query_metadata_version=1,updated_at=? WHERE rowid=? AND receipt_json=? AND content_attachment_id=?`, contentID, receipt.Status, Time(now), item.row, item.raw, item.contentID)
				if err != nil {
					return 0, err
				}
				count, err := result.RowsAffected()
				if err != nil || count == 0 {
					return 0, err
				}
				return count, tx.Commit()
			}()
			if err != nil {
				return changed, err
			}
			changed += count
		}
	}
}

func (r *ArchiveReads) RememberReference(organization string, ref attachment.ArchiveReference, content []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.references == nil {
		r.references = map[string]attachment.ArchiveReference{}
	}
	r.references[organization+"/"+artifact.Digest(content)] = ref
}

func (r *ArchiveReads) Reference(key string) (attachment.ArchiveReference, bool) {
	if r == nil {
		return attachment.ArchiveReference{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.references[key]
	return ref, ok
}

func (r *ArchiveReads) Load(key string) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, ok := r.Content[key]
	return data, ok
}

func (r *ArchiveReads) Store(key string, data []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Content[key] = bytes.Clone(data)
}

func (b *ReadBatch) Capture(ctx context.Context, query Queryer, contentID string) error {
	if contentID == "" {
		return nil
	}
	if _, ok := b.archives[contentID]; ok {
		return nil
	}
	ref, err := ScanArchive(query.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, contentID, b.organization))
	if err != nil {
		return err
	}
	b.archives[contentID] = ref
	return nil
}

func (b *ReadBatch) CaptureMessages(ctx context.Context, query Queryer, messages []MessageRecord) error {
	for _, message := range messages {
		if err := b.Capture(ctx, query, message.ContentID); err != nil {
			return err
		}
		if message.Role != conversation.RoleAssistant {
			continue
		}
		offset := int64(len(message.Text))
		if message.ContentID != "" {
			if !message.StoredTextBytes.Valid {
				continue
			}
			offset = message.StoredTextBytes.Int64
		}
		if err := b.captureMessageDeltaSuffix(ctx, query, message, offset); err != nil {
			return err
		}
	}
	return nil
}

func (b *ReadBatch) captureMessageDeltaSuffix(ctx context.Context, query Queryer, message MessageRecord, offset int64) error {
	deltas, err := readConversationMessageDeltaSuffix(ctx, query, message, offset)
	if err != nil {
		return err
	}
	for _, delta := range deltas {
		if err := b.Capture(ctx, query, delta.contentID); err != nil {
			return err
		}
	}
	b.deltas[message.ID] = deltas
	return nil
}

func (b *ReadBatch) captureLegacyMessageDeltas(ctx context.Context, store *Store, message MessageRecord) error {
	tx, err := store.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := b.captureMessageDeltaSuffix(ctx, tx, message, int64(len(message.Text))); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *ReadBatch) read(ctx context.Context, store *Store, contentID string) ([]byte, error) {
	ref, ok := b.archives[contentID]
	if !ok {
		return nil, errors.New("conversation archive was not captured before hydration")
	}
	return store.ReadCapturedConversationArchive(ctx, b.organization, contentID, ref)
}

func (b *ReadBatch) payload(ctx context.Context, store *Store, contentID string) ([]byte, error) {
	raw, err := b.read(ctx, store, contentID)
	if err != nil {
		return nil, err
	}
	var content Content
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, err
	}
	return content.Data, nil
}

func (b *ReadBatch) HydrateRecord(ctx context.Context, store *Store, record *Record) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := b.payload(ctx, store, record.ContentID)
	if err != nil {
		return err
	}
	var content RecordContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return err
	}
	record.Execution, record.Preferences = content.Execution, content.Preferences.Normalized()
	return nil
}

func (b *ReadBatch) HydrateMessages(ctx context.Context, store *Store, messages []MessageRecord) error {
	for i := range messages {
		message := &messages[i]
		if message.ContentID != "" {
			raw, err := b.read(ctx, store, message.ContentID)
			if err != nil {
				return err
			}
			var content Content
			if err := json.Unmarshal(raw, &content); err != nil {
				return err
			}
			message.Text, message.Data, message.ContentHash = content.Text, content.Data, artifact.Digest(raw)
			if message.StoredTextBytes.Valid && message.StoredTextBytes.Int64 != int64(len(message.Text)) {
				return errors.New("message stored text length does not match content")
			}
		}
		if message.Role != conversation.RoleAssistant || message.TextBytes <= int64(len(message.Text)) {
			continue
		}
		if message.ContentID != "" && !message.StoredTextBytes.Valid {
			if err := b.captureLegacyMessageDeltas(ctx, store, *message); err != nil {
				return err
			}
		}
		deltas := b.deltas[message.ID]
		for j := range deltas {
			if deltas[j].offset < int64(len(message.Text)) || deltas[j].contentID == "" {
				continue
			}
			var err error
			deltas[j].raw, err = b.read(ctx, store, deltas[j].contentID)
			if err != nil {
				return err
			}
		}
		if err := applyConversationMessageDeltas(message, deltas); err != nil {
			return err
		}
	}
	return nil
}

func (b *ReadBatch) HydrateQuestions(ctx context.Context, store *Store, questions []QuestionRecord) error {
	for i := range questions {
		if questions[i].ContentID == "" {
			continue
		}
		raw, err := b.payload(ctx, store, questions[i].ContentID)
		if err != nil {
			return err
		}
		var content QuestionContent
		if err := json.Unmarshal(raw, &content); err != nil {
			return err
		}
		questions[i].Prompts, questions[i].Answers = content.Prompts, content.Answers
	}
	return nil
}

func ScanArchive(row interface{ Scan(...any) error }) (attachment.ArchiveReference, error) {
	var ref attachment.ArchiveReference
	err := row.Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned)
	return ref, err
}

var ErrPublicationUncertain = errors.New("conversation content publication is uncertain")

type Content struct {
	Text string          `json:"text"`
	Data json.RawMessage `json:"data"`
}

func MessageContent(message MessageRecord) ([]byte, error) {
	data := message.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return json.Marshal(Content{Text: message.Text, Data: data})
}

type PreparedContents map[string]attachment.ArchiveReference

type ContentsKey struct{}

type ArchiveReadsKey struct{}

type ArchiveReads struct {
	mu         sync.Mutex
	Content    map[string][]byte
	references map[string]attachment.ArchiveReference
}

func WithArchiveReads(ctx context.Context) context.Context {
	if ArchiveCache(ctx) != nil {
		return ctx
	}
	return NewArchiveReads(ctx)
}

func ArchiveCache(ctx context.Context) *ArchiveReads {
	cache, ok := ctx.Value(ArchiveReadsKey{}).(*ArchiveReads)
	if !ok {
		return nil
	}
	return cache
}

func NewArchiveReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, ArchiveReadsKey{}, &ArchiveReads{Content: map[string][]byte{}})
}

type conversationReadContentsKey struct{}

type readConversationContent struct {
	content Content
	hash    string
}

type conversationContentReads map[string]readConversationContent

func ContentArchiveID(organization string, ref attachment.ArchiveReference) string {
	return "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32]
}

type conversationReadDelta struct {
	raw       []byte
	contentID string
	offset    int64
}

func readConversationMessageDeltas(ctx context.Context, q Queryer, message MessageRecord) ([]conversationReadDelta, error) {
	return readConversationMessageDeltaSuffix(ctx, q, message, int64(len(message.Text)))
}

func readConversationMessageDeltaSuffix(ctx context.Context, q Queryer, message MessageRecord, offset int64) ([]conversationReadDelta, error) {
	if offset < 0 || offset > message.TextBytes {
		return nil, errors.New("message stored text boundary is invalid")
	}
	remaining := message.TextBytes - offset
	if remaining == 0 {
		return nil, nil
	}
	limit := remaining
	if limit < math.MaxInt64 {
		limit++
	}
	rows, err := q.QueryContext(ctx, `SELECT body_json,content_attachment_id,delta_offset FROM conversation_events WHERE conversation_id=? AND message_id=? AND type='message.delta' AND delta_offset>=? AND delta_offset<? ORDER BY delta_offset,seq LIMIT ?`, message.ConversationID, message.ID, offset, message.TextBytes, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []conversationReadDelta
	for rows.Next() {
		var d conversationReadDelta
		if err := rows.Scan(&d.raw, &d.contentID, &d.offset); err != nil {
			return nil, err
		}
		batch = append(batch, d)
		if int64(len(batch)) > remaining {
			return nil, errors.New("message delta rows exceed missing text bytes")
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return batch, nil
}

func applyConversationMessageDeltas(message *MessageRecord, batch []conversationReadDelta) error {
	var text strings.Builder
	text.WriteString(message.Text)
	for _, d := range batch {
		if d.offset < int64(len(message.Text)) {
			continue
		}
		var fact DeltaFact
		if err := json.Unmarshal(d.raw, &fact); err != nil {
			return err
		}
		if fact.Offset != int64(text.Len()) || len(fact.Text) != fact.Bytes {
			return errors.New("message delta boundary is invalid")
		}
		text.WriteString(fact.Text)
	}
	if int64(text.Len()) != message.TextBytes {
		return errors.New("message delta content is incomplete")
	}
	message.Text = text.String()
	return nil
}

type ReadBatch struct {
	organization string
	archives     map[string]attachment.ArchiveReference
	deltas       map[string][]conversationReadDelta
}

func NewReadBatch(organization string) *ReadBatch {
	return &ReadBatch{organization: organization, archives: map[string]attachment.ArchiveReference{}, deltas: map[string][]conversationReadDelta{}}
}

type MessageFact struct {
	TurnID    *string                  `json:"turn_id"`
	EmptyData bool                     `json:"empty_data"`
	ID        string                   `json:"id"`
	Role      conversation.Role        `json:"role"`
	Kind      conversation.MessageKind `json:"kind"`
	AttemptID *string                  `json:"attempt_id"`

	MessageID string                `json:"message_id"`
	ContentID string                `json:"content_id,omitempty"`
	Revision  int64                 `json:"message_revision"`
	Delivery  conversation.Delivery `json:"delivery"`
	TextBytes int                   `json:"text_bytes"`
	UpdatedAt time.Time             `json:"updated_at"`
}

type DeltaFact struct {
	Text      string `json:"text,omitempty"`
	Revision  int64  `json:"message_revision"`
	MessageID string `json:"message_id"`
	Offset    int64  `json:"seq"`
	Bytes     int    `json:"text_bytes"`
}

type QuestionFact struct {
	QuestionID string                      `json:"question_id"`
	Status     conversation.QuestionStatus `json:"status"`
	AnsweredBy string                      `json:"answered_by"`
	UpdatedAt  time.Time                   `json:"updated_at"`
	Answered   bool                        `json:"answered"`
}

func CompactEvent(body any) any {
	switch value := body.(type) {
	case MessageResource:
		raw, ok := value.Data.(json.RawMessage)
		empty := !ok || len(raw) == 0 || string(raw) == "{}" || string(raw) == "null"
		return MessageFact{ID: value.ID, TurnID: value.TurnID, EmptyData: empty, Role: value.Role, Kind: value.Kind, AttemptID: value.AttemptID, MessageID: value.ID, ContentID: value.ContentID, Revision: value.Revision, Delivery: value.Delivery, TextBytes: len(value.Text), UpdatedAt: value.UpdatedAt}
	case conversation.Question:
		return QuestionFact{QuestionID: value.ID, Status: value.Status, AnsweredBy: value.AnsweredBy, UpdatedAt: value.UpdatedAt, Answered: value.Answers != nil}
	default:
		return body
	}
}

type RecordContent struct {
	Execution   conversation.Execution   `json:"execution"`
	Preferences conversation.Preferences `json:"preferences"`
}

type QuestionContent struct {
	Prompts []conversation.Prompt `json:"prompts"`
	Answers map[string][]string   `json:"answers"`
}

func RecordPayload(record Record) ([]byte, error) {
	return json.Marshal(RecordContent{Execution: record.Execution, Preferences: record.Preferences.Normalized()})
}

func QuestionPayload(record QuestionRecord) ([]byte, error) {
	prompts, answers := record.Prompts, record.Answers
	if prompts == nil {
		prompts = []conversation.Prompt{}
	}
	if answers == nil {
		answers = map[string][]string{}
	}
	return json.Marshal(QuestionContent{Prompts: prompts, Answers: answers})
}

func RollbackPreparation(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}
