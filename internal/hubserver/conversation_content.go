package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

var errConversationPublicationUncertain = errors.New("conversation content publication is uncertain")

type conversationContent struct {
	Text string          `json:"text"`
	Data json.RawMessage `json:"data"`
}

func messageContent(message conversationMessageRecord) ([]byte, error) {
	data := message.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return json.Marshal(conversationContent{Text: message.Text, Data: data})
}

type preparedConversationContents map[string]attachment.ArchiveReference
type conversationContentsKey struct{}
type conversationArchiveReadsKey struct{}
type conversationArchiveReads struct {
	mu      sync.Mutex
	content map[string][]byte
}

func (r *conversationArchiveReads) load(key string) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, ok := r.content[key]
	return data, ok
}

func (r *conversationArchiveReads) store(key string, data []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.content[key] = bytes.Clone(data)
}

func withConversationArchiveReads(ctx context.Context) context.Context {
	if conversationArchiveCache(ctx) != nil {
		return ctx
	}
	return newConversationArchiveReads(ctx)
}

func conversationArchiveCache(ctx context.Context) *conversationArchiveReads {
	cache, ok := ctx.Value(conversationArchiveReadsKey{}).(*conversationArchiveReads)
	if !ok {
		return nil
	}
	return cache
}

func newConversationArchiveReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, conversationArchiveReadsKey{}, &conversationArchiveReads{content: map[string][]byte{}})
}

func (s *conversationStore) publishPayload(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
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
			return attachment.ArchiveReference{}, errors.Join(errConversationPublicationUncertain, publishErr, readErr)
		}
		ref = expected
	}
	if ref != expected {
		return attachment.ArchiveReference{}, errors.New("conversation archive reference changed during publication")
	}
	return ref, nil
}

func (s *conversationStore) preparePayload(ctx context.Context, organization string, content []byte) (context.Context, error) {
	return s.preparePayloads(ctx, organization, content)
}

func (s *conversationStore) preparePayloads(ctx context.Context, organization string, contents ...[]byte) (context.Context, error) {
	ctx = withConversationArchiveReads(ctx)
	prepared := preparedConversationContents{}
	if existing, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents); ok {
		for key, ref := range existing {
			prepared[key] = ref
		}
	}
	reads := conversationArchiveCache(ctx)
	for _, content := range contents {
		key := organization + "/" + artifact.Digest(content)
		ref, found := prepared[key]
		if !found {
			var err error
			ref, err = s.publishPayload(ctx, organization, content)
			if err != nil {
				return ctx, err
			}
			prepared[key] = ref
		}
		reads.store(organization+"/"+contentArchiveID(organization, ref), content)
	}
	return context.WithValue(ctx, conversationContentsKey{}, prepared), nil
}

func (c *conversationService) prepareContents(ctx context.Context, organization, project, uploader string, messages ...conversationMessageRecord) (context.Context, error) {
	contents := make([][]byte, 0, len(messages))
	for _, message := range messages {
		content, err := messageContent(message)
		if err != nil {
			return ctx, err
		}
		contents = append(contents, content)
	}
	return c.store.preparePayloads(ctx, organization, contents...)
}

func (s *conversationStore) messageStorage(ctx context.Context, tx *sql.Tx, message conversationMessageRecord) (string, string, string, error) {
	if s.contents == nil {
		return "", "", "", errors.New("conversation content storage is unavailable")
	}
	content, err := messageContent(message)
	if err != nil {
		return "", "", "", err
	}
	digest := artifact.Digest(content)
	if message.ContentID != "" && digest == message.contentHash {
		return "", "{}", message.ContentID, nil
	}
	var organization, project, uploader string
	if err := tx.QueryRowContext(ctx, "SELECT organization_id,project_id,owner_principal_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization, &project, &uploader); err != nil {
		return "", "", "", err
	}
	prepared, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents)
	ref, found := prepared[organization+"/"+digest]
	if !ok || !found {
		return "", "", "", errors.New("conversation content was not published before the transaction")
	}
	if err := ref.Validate(organization); err != nil {
		return "", "", "", err
	}
	id := contentArchiveID(organization, ref)
	if err := s.recordContentArchive(ctx, tx, organization, project, uploader, ref, message.CreatedAt); err != nil {
		return "", "", "", err
	}

	return "", "{}", id, nil
}

type conversationReadContentsKey struct{}
type readConversationContent struct {
	content conversationContent
	hash    string
}
type conversationContentReads map[string]readConversationContent

func (s *conversationStore) resolveMessageContents(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
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
			message.Text, message.Data, message.contentHash = cached.content.Text, cached.content.Data, cached.hash
			continue
		}
		content, err := s.readConversationArchive(ctx, query, organization, message.ContentID)
		if err != nil {
			return err
		}
		var decoded conversationContent
		if err := json.Unmarshal(content, &decoded); err != nil {
			return fmt.Errorf("decode conversation content: %w", err)
		}
		message.Text, message.Data, message.contentHash = decoded.Text, decoded.Data, artifact.Digest(content)
		cache[key] = readConversationContent{content: decoded, hash: message.contentHash}
	}
	for _, message := range messages {
		if message.Role != conversation.RoleAssistant || message.TextBytes <= int64(len(message.Text)) {
			continue
		}
		if err := s.resolveMessageDeltas(ctx, query, message); err != nil {
			return err
		}
	}
	return nil
}

func (c *conversationService) prepareTurnContents(ctx context.Context, organization, id string, events []conversationTurnEvent, finishing bool) (context.Context, error) {
	var err error
	ctx, err = c.store.prepareLegacyToolRevisions(ctx, id)
	if err != nil {
		return ctx, err
	}
	if c.store.contents == nil {
		return ctx, nil
	}
	var candidates []conversationMessageRecord
	for _, event := range events {
		if event.Type == "turn_completed" {
			finishing = true
		}
		if event.Type == "item" {
			data, err := conversationItemData(event.Data)
			if err != nil {
				return ctx, err
			}
			candidates = append(candidates, conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageKind(event.Kind), Text: conversationTruncate(event.Summary, conversationMaxSummaryBytes), Data: data, Delivery: conversation.DeliveryCompleted})
		}
	}
	if finishing {
		messages, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id=? AND delivery='responding' ORDER BY seq", id)
		if err != nil {
			return ctx, err
		}
		byItem, byID := map[string]*conversationMessageRecord{}, map[string]*conversationMessageRecord{}
		tracked := make([]*conversationMessageRecord, 0, len(messages))
		for index := range messages {
			message := &messages[index]
			byItem[message.ProviderItemID] = message
			byID[message.ID] = message
			tracked = append(tracked, message)
		}
		turnID := ""
		for _, event := range events {
			switch event.Type {
			case "turn_started":
				turnID = event.TurnID
			case "delta":
				message := byItem[event.ProviderItemID]
				if event.MessageID != "" {
					message = byID[event.MessageID]
				}
				if message == nil {
					message = &conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, TurnID: turnID}
					byItem[event.ProviderItemID] = message
					tracked = append(tracked, message)
				}
				message.Text += event.Text
			case "turn_completed":
				for _, message := range tracked {
					if message.TurnID == event.TurnID && message.Delivery == conversation.DeliveryResponding {
						message.Delivery = conversation.DeliveryCompleted
						candidates = append(candidates, *message)
					}
				}
			}
		}
		for _, message := range tracked {
			if message.Delivery == conversation.DeliveryResponding {
				message.Delivery = conversation.DeliveryCompleted
				candidates = append(candidates, *message)
			}
		}

	}

	var project, uploader string
	if err := c.store.reader.QueryRowContext(ctx, "SELECT project_id,owner_principal_id FROM conversations WHERE id=?", id).Scan(&project, &uploader); err != nil {
		return ctx, err
	}
	return c.prepareContents(ctx, organization, project, uploader, candidates...)
}

func (c *conversationService) prepareCommandContents(ctx context.Context, organization, project, uploader, id string, command conversation.Command) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	message := conversationMessageRecord{Role: conversation.RoleUser, Text: command.Text, Delivery: conversation.DeliverySaved}
	switch command.Kind {
	case conversation.CommandMessage:
		message.Data = coordinatorConnectionData(ctx)
	case conversation.CommandContinue:
	case conversation.CommandAnswer:
		question, err := c.store.readQuestion(ctx, c.store.reader, id, command.QuestionID)
		if err != nil {
			return ctx, err
		}
		data, err := json.Marshal(command.Answers)
		if err != nil {
			return ctx, err
		}
		message.Text, message.Data = renderAnswer(question.Question, command.Answers), data
	default:
		return ctx, nil
	}
	return c.prepareContents(ctx, organization, project, uploader, message)
}

func (c *conversationService) prepareConversationContents(ctx context.Context, id string, messages ...conversationMessageRecord) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	var organization, project, uploader string
	if err := c.store.reader.QueryRowContext(ctx, "SELECT organization_id,project_id,owner_principal_id FROM conversations WHERE id=?", id).Scan(&organization, &project, &uploader); err != nil {
		return ctx, err
	}
	return c.prepareContents(ctx, organization, project, uploader, messages...)
}

func contentArchiveID(organization string, ref attachment.ArchiveReference) string {
	return "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32]
}
func (s *conversationStore) recordContentArchive(ctx context.Context, tx *sql.Tx, organization, project, uploader string, ref attachment.ArchiveReference, now time.Time) error {
	var before map[string]int64
	var err error
	if s.database != nil {
		before, err = s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes")
		if err != nil {
			return err
		}
	}
	id := contentArchiveID(organization, ref)
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version)
 VALUES(?,?,?,?,'archive.gz','application/gzip',?,?,?,'archive',?,?,?,?,?) ON CONFLICT DO NOTHING`, id, organization, project, uploader, ref.CompressedSize, ref.SHA256, conversationTime(now), ref.Retention, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion); err != nil {
		return err
	}
	stored, err := scanAttachmentArchive(tx.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, id, organization))
	if err != nil {
		return err
	}
	if stored != ref {
		return errors.New("conversation archive reference conflicts with stored content")
	}
	if s.database != nil {
		return s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes")
	}
	return nil
}

func (s *conversationStore) resolveMessageDeltas(ctx context.Context, q nativeQueryer, message *conversationMessageRecord) error {
	rows, err := q.QueryContext(ctx, `SELECT body_json,content_attachment_id FROM conversation_events WHERE conversation_id=? AND message_id=? AND type='message.delta' AND delta_offset>=? AND delta_offset<? ORDER BY seq`, message.ConversationID, message.ID, len(message.Text), message.TextBytes)
	if err != nil {
		return err
	}
	defer rows.Close()
	type delta struct{ raw, content string }
	var batch []delta
	for rows.Next() {
		var d delta
		if err := rows.Scan(&d.raw, &d.content); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var text strings.Builder
	text.WriteString(message.Text)
	for _, d := range batch {
		raw := []byte(d.raw)
		if d.content != "" {
			raw, err = s.readEventStorage(ctx, q, message.ConversationID, d.content)
			if err != nil {
				return err
			}
		}
		var fact conversationDeltaFact
		if err := json.Unmarshal(raw, &fact); err != nil {
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

func (s *conversationStore) readConversationArchive(ctx context.Context, query nativeQueryer, organization, contentID string) ([]byte, error) {
	cache := conversationArchiveCache(ctx)
	key := organization + "/" + contentID
	if data, ok := cache.load(key); ok {
		return data, nil
	}
	if _, transactional := query.(*sql.Tx); transactional && ctx.Value(conversationPublicationKey{}) != nil {
		return nil, errors.New("conversation archive was not read before the transaction")
	}
	ref, err := scanAttachmentArchive(query.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, contentID, organization))
	if err != nil {
		return nil, err
	}
	if s.contents == nil {
		return nil, errors.New("conversation content storage is unavailable")
	}
	data, err := s.contents.ReadContent(ctx, organization, ref)
	if err != nil {
		return nil, fmt.Errorf("read conversation content: %w", err)
	}
	cache.store(key, data)
	return data, nil
}
