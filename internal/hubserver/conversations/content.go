package conversations

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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

var errConversationPublicationUncertain = errors.New("conversation content publication is uncertain")

type ConversationContent struct {
	Text string          `json:"text"`
	Data json.RawMessage `json:"data"`
}

func MessageContent(message ConversationMessageRecord) ([]byte, error) {
	data := message.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return json.Marshal(ConversationContent{Text: message.Text, Data: data})
}

type PreparedConversationContents map[string]attachment.ArchiveReference
type ConversationContentsKey struct{}
type conversationArchiveReadsKey struct{}
type ConversationArchiveReads struct {
	mu         sync.Mutex
	Content    map[string][]byte
	references map[string]attachment.ArchiveReference
}

func (r *ConversationArchiveReads) rememberReference(organization string, ref attachment.ArchiveReference, content []byte) {
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

func (r *ConversationArchiveReads) Load(key string) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, ok := r.Content[key]
	return data, ok
}

func (r *ConversationArchiveReads) store(key string, data []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Content[key] = bytes.Clone(data)
}

func withConversationArchiveReads(ctx context.Context) context.Context {
	if ConversationArchiveCache(ctx) != nil {
		return ctx
	}
	return NewConversationArchiveReads(ctx)
}

func ConversationArchiveCache(ctx context.Context) *ConversationArchiveReads {
	cache, ok := ctx.Value(conversationArchiveReadsKey{}).(*ConversationArchiveReads)
	if !ok {
		return nil
	}
	return cache
}

func NewConversationArchiveReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, conversationArchiveReadsKey{}, &ConversationArchiveReads{Content: map[string][]byte{}})
}

func (s *ConversationStore) PublishPayload(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if s.Contents == nil {
		return attachment.ArchiveReference{}, errors.New("conversation content storage is unavailable")
	}
	expected, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, publishErr := s.Contents.PublishContent(ctx, organization, content)
	if publishErr != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.DefaultCleanupBudget)
		defer cancel()
		stored, readErr := s.Contents.ReadContent(reconcileCtx, organization, expected)
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

func (s *ConversationStore) PreparePayloads(ctx context.Context, organization string, contents ...[]byte) (context.Context, error) {
	ctx = withConversationArchiveReads(ctx)
	prepared := PreparedConversationContents{}
	if existing, ok := ctx.Value(ConversationContentsKey{}).(PreparedConversationContents); ok {
		for key, ref := range existing {
			prepared[key] = ref
		}
	}
	reads := ConversationArchiveCache(ctx)
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
		reads.store(organization+"/"+ContentArchiveID(organization, ref), content)
	}
	return context.WithValue(ctx, ConversationContentsKey{}, prepared), nil
}

func (c *ConversationService) prepareContents(ctx context.Context, organization, project, uploader string, messages ...ConversationMessageRecord) (context.Context, error) {
	contents := make([][]byte, 0, len(messages))
	for _, message := range messages {
		content, err := MessageContent(message)
		if err != nil {
			return ctx, err
		}
		contents = append(contents, content)
	}
	return c.Store.PreparePayloads(ctx, organization, contents...)
}

func (c *ConversationService) PrepareConversationContents(ctx context.Context, id string, messages ...ConversationMessageRecord) (context.Context, error) {
	if c.Store.Contents == nil {
		return ctx, nil
	}
	record, err := c.ReadPublicationConversation(ctx, id)
	if err != nil {
		return ctx, err
	}
	return c.prepareContents(ctx, string(record.OrganizationID), string(record.ProjectID), record.OwnerPrincipalID, messages...)
}

func ContentArchiveID(organization string, ref attachment.ArchiveReference) string {
	return "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32]
}

type conversationReadDelta struct {
	raw       []byte
	contentID string
	offset    int64
}

func applyConversationMessageDeltas(message *ConversationMessageRecord, batch []conversationReadDelta) error {
	var text strings.Builder
	text.WriteString(message.Text)
	for _, d := range batch {
		if d.offset < int64(len(message.Text)) {
			continue
		}
		var fact ConversationDeltaFact
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

func (s *ConversationStore) readConversationArchive(ctx context.Context, query Queryer, organization, contentID string) ([]byte, error) {
	cache := ConversationArchiveCache(ctx)
	key := organization + "/" + contentID
	if data, ok := cache.Load(key); ok {
		return data, nil
	}
	if _, transactional := query.(*sql.Tx); transactional && ctx.Value(ConversationPublicationKey{}) != nil {
		return nil, errors.New("conversation archive was not read before the transaction")
	}
	var ref attachment.ArchiveReference
	err := query.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, contentID, organization).Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned)
	if err != nil {
		return nil, err
	}
	return s.readCapturedConversationArchive(ctx, organization, contentID, ref)
}

func (s *ConversationStore) readCapturedConversationArchive(ctx context.Context, organization, contentID string, ref attachment.ArchiveReference) ([]byte, error) {
	cache := ConversationArchiveCache(ctx)
	key := organization + "/" + contentID
	if data, ok := cache.Load(key); ok {
		return data, nil
	}
	if s.Contents == nil {
		return nil, errors.New("conversation content storage is unavailable")
	}
	data, err := s.Contents.ReadContent(ctx, organization, ref)
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

func (b *conversationReadBatch) read(ctx context.Context, store *ConversationStore, contentID string) ([]byte, error) {
	ref, ok := b.archives[contentID]
	if !ok {
		return nil, errors.New("conversation archive was not captured before hydration")
	}
	return store.readCapturedConversationArchive(ctx, b.organization, contentID, ref)
}

func (b *conversationReadBatch) payload(ctx context.Context, store *ConversationStore, contentID string) ([]byte, error) {
	raw, err := b.read(ctx, store, contentID)
	if err != nil {
		return nil, err
	}
	var content ConversationContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, err
	}
	return content.Data, nil
}

func (b *conversationReadBatch) hydrateRecord(ctx context.Context, store *ConversationStore, record *ConversationRecord) error {
	if record.ContentID == "" {
		return nil
	}
	raw, err := b.payload(ctx, store, record.ContentID)
	if err != nil {
		return err
	}
	var content ConversationRecordContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return err
	}
	record.Execution, record.Preferences = content.Execution, content.Preferences.Normalized()
	return nil
}

func (b *conversationReadBatch) hydrateMessages(ctx context.Context, store *ConversationStore, messages []ConversationMessageRecord) error {
	for i := range messages {
		message := &messages[i]
		if message.ContentID != "" {
			raw, err := b.read(ctx, store, message.ContentID)
			if err != nil {
				return err
			}
			var content ConversationContent
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
