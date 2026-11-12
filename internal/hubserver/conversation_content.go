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

func (c *conversationService) prepareConversationContents(ctx context.Context, id string, messages ...conversationMessageRecord) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	record, err := c.readPublicationConversation(ctx, id)
	if err != nil {
		return ctx, err
	}
	return c.prepareContents(ctx, string(record.OrganizationID), string(record.ProjectID), record.OwnerPrincipalID, messages...)
}

func contentArchiveID(organization string, ref attachment.ArchiveReference) string {
	return "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32]
}

type conversationReadDelta struct {
	raw       []byte
	contentID string
	offset    int64
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
