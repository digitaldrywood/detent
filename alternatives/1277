package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	mu         sync.Mutex
	content    map[string][]byte
	references map[string]attachment.ArchiveReference
}

func (r *conversationArchiveReads) rememberReference(organization string, ref attachment.ArchiveReference, content []byte) {
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

func (r *conversationArchiveReads) reference(key string) (attachment.ArchiveReference, bool) {
	if r == nil {
		return attachment.ArchiveReference{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.references[key]
	return ref, ok
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
	id, err := s.preparedRecordStorage(ctx, tx, organization, project, uploader, content, message.CreatedAt)
	if err != nil {
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

type conversationReadDelta struct {
	raw       []byte
	contentID string
	offset    int64
}

func readConversationMessageDeltas(ctx context.Context, q nativeQueryer, message conversationMessageRecord) ([]conversationReadDelta, error) {
	return readConversationMessageDeltaSuffix(ctx, q, message, int64(len(message.Text)))
}

func readConversationMessageDeltaSuffix(ctx context.Context, q nativeQueryer, message conversationMessageRecord, offset int64) ([]conversationReadDelta, error) {
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

func (s *conversationStore) resolveMessageDeltas(ctx context.Context, q nativeQueryer, message *conversationMessageRecord) error {
	batch, err := readConversationMessageDeltas(ctx, q, *message)
	if err != nil {
		return err
	}
	for i := range batch {
		if batch[i].contentID != "" {
			batch[i].raw, err = s.readEventStorage(ctx, q, message.ConversationID, batch[i].contentID)
			if err != nil {
				return err
			}
		}
	}
	return applyConversationMessageDeltas(message, batch)
}

func applyConversationMessageDeltas(message *conversationMessageRecord, batch []conversationReadDelta) error {
	var text strings.Builder
	text.WriteString(message.Text)
	for _, d := range batch {
		if d.offset < int64(len(message.Text)) {
			continue
		}
		var fact conversationDeltaFact
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
	return s.readCapturedConversationArchive(ctx, organization, contentID, ref)
}

func (s *conversationStore) readCapturedConversationArchive(ctx context.Context, organization, contentID string, ref attachment.ArchiveReference) ([]byte, error) {
	cache := conversationArchiveCache(ctx)
	key := organization + "/" + contentID
	if data, ok := cache.load(key); ok {
		return data, nil
	}
	if s.contents == nil {
		return nil, errors.New("conversation content storage is unavailable")
	}
	data, err := s.contents.ReadContent(ctx, organization, ref)
	if err != nil {
		return nil, fmt.Errorf("read conversation content: %w", err)
	}
	cache.store(key, data)
	cache.rememberReference(organization, ref, data)
	return data, nil
}

type conversationReadBatch struct {
	organization string
	archives     map[string]attachment.ArchiveReference
	deltas       map[string][]conversationReadDelta
}

func newConversationReadBatch(organization string) *conversationReadBatch {
	return &conversationReadBatch{organization: organization, archives: map[string]attachment.ArchiveReference{}, deltas: map[string][]conversationReadDelta{}}
}

func (b *conversationReadBatch) capture(ctx context.Context, query nativeQueryer, contentID string) error {
	if contentID == "" {
		return nil
	}
	if _, ok := b.archives[contentID]; ok {
		return nil
	}
	ref, err := scanAttachmentArchive(query.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, contentID, b.organization))
	if err != nil {
		return err
	}
	b.archives[contentID] = ref
	return nil
}

func (b *conversationReadBatch) captureMessages(ctx context.Context, query nativeQueryer, messages []conversationMessageRecord) error {
	for _, message := range messages {
		if err := b.capture(ctx, query, message.ContentID); err != nil {
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

func (b *conversationReadBatch) captureMessageDeltaSuffix(ctx context.Context, query nativeQueryer, message conversationMessageRecord, offset int64) error {
	deltas, err := readConversationMessageDeltaSuffix(ctx, query, message, offset)
	if err != nil {
		return err
	}
	for _, delta := range deltas {
		if err := b.capture(ctx, query, delta.contentID); err != nil {
			return err
		}
	}
	b.deltas[message.ID] = deltas
	return nil
}

func (b *conversationReadBatch) captureLegacyMessageDeltas(ctx context.Context, store *conversationStore, message conversationMessageRecord) error {
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

func (b *conversationReadBatch) read(ctx context.Context, store *conversationStore, contentID string) ([]byte, error) {
	ref, ok := b.archives[contentID]
	if !ok {
		return nil, errors.New("conversation archive was not captured before hydration")
	}
	return store.readCapturedConversationArchive(ctx, b.organization, contentID, ref)
}

func (b *conversationReadBatch) payload(ctx context.Context, store *conversationStore, contentID string) ([]byte, error) {
	raw, err := b.read(ctx, store, contentID)
	if err != nil {
		return nil, err
	}
	var content conversationContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, err
	}
	return content.Data, nil
}

func (b *conversationReadBatch) hydrateRecord(ctx context.Context, store *conversationStore, record *conversationRecord) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := b.payload(ctx, store, record.ContentID)
	if err != nil {
		return err
	}
	var content conversationRecordContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return err
	}
	record.Execution, record.Preferences = content.Execution, content.Preferences.Normalized()
	return nil
}

func (b *conversationReadBatch) hydrateMessages(ctx context.Context, store *conversationStore, messages []conversationMessageRecord) error {
	for i := range messages {
		message := &messages[i]
		if message.ContentID != "" {
			raw, err := b.read(ctx, store, message.ContentID)
			if err != nil {
				return err
			}
			var content conversationContent
			if err := json.Unmarshal(raw, &content); err != nil {
				return err
			}
			message.Text, message.Data, message.contentHash = content.Text, content.Data, artifact.Digest(raw)
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

func (b *conversationReadBatch) hydrateQuestions(ctx context.Context, store *conversationStore, questions []conversationQuestionRecord) error {
	for i := range questions {
		if questions[i].ContentID == "" {
			continue
		}
		raw, err := b.payload(ctx, store, questions[i].ContentID)
		if err != nil {
			return err
		}
		var content conversationQuestionContent
		if err := json.Unmarshal(raw, &content); err != nil {
			return err
		}
		questions[i].Prompts, questions[i].Answers = content.Prompts, content.Answers
	}
	return nil
}
