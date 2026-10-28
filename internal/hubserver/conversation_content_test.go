package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationContentFixture struct {
	mu      sync.Mutex
	content map[string][]byte
	fail    bool
}

func (s *conversationContentFixture) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return attachment.ArchiveReference{}, errors.New("object store unavailable")
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	if s.content == nil {
		s.content = map[string][]byte{}
	}
	s.content[ref.Key] = bytes.Clone(content)
	return ref, nil
}

func (s *conversationContentFixture) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	content, ok := s.content[ref.Key]
	if !ok {
		return nil, errors.New("object missing")
	}
	return bytes.Clone(content), nil
}

func (s *conversationContentFixture) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func TestConversationPayloadBytesStayBounded(t *testing.T) {
	for _, size := range []int{32, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := newConversationFixture(t)
			objects := &conversationContentFixture{}
			f.store.contents = objects
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			record := f.create(t, f.record(f.owner, "payload", now))
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Text: strings.Repeat("x", size), Data: json.RawMessage(`{"value":"` + strings.Repeat("y", size) + `"}`), Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: now, UpdatedAt: now}
			tx := f.tx(t)
			if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			body := map[string]string{"text": strings.Repeat("z", size)}
			if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventHeartbeat, body, now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			var messageBytes, eventBytes int
			if err := f.store.reader.QueryRowContext(t.Context(), `SELECT length(CAST(text AS BLOB))+length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB)) FROM conversation_messages WHERE id=?`, message.ID).Scan(&messageBytes); err != nil {
				t.Fatal(err)
			}
			if err := f.store.reader.QueryRowContext(t.Context(), `SELECT length(CAST(body_json AS BLOB)) FROM conversation_events WHERE conversation_id=?`, record.ID).Scan(&eventBytes); err != nil {
				t.Fatal(err)
			}
			if messageBytes != 4 || eventBytes != 2 {
				t.Fatalf("inline payload bytes: message=%d event=%d; want 4 and 2", messageBytes, eventBytes)
			}
			got, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(got) != 1 || got[0].Text != message.Text || !bytes.Equal(got[0].Data, message.Data) || got[0].Actor != message.Actor {
				t.Fatalf("message round trip: messages=%d err=%v", len(got), err)
			}
			events, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(events) != 1 {
				t.Fatalf("event round trip: events=%d err=%v", len(events), err)
			}
			var decoded map[string]string
			if err := json.Unmarshal(events[0].Data, &decoded); err != nil || !reflect.DeepEqual(decoded, body) {
				t.Fatalf("event body changed: err=%v", err)
			}
		})
	}
}

func TestConversationStreamCompactsIntoFinalContent(t *testing.T) {
	for _, chunk := range []string{"small delta", strings.Repeat("delta ", 8000)} {
		t.Run(strconv.Itoa(len(chunk)), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			f.store.contents = &conversationContentFixture{}
			record := f.create(t, f.record(f.owner, "stream", f.now))
			chat := f.service.conversations
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
			tx := f.tx(t)
			if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventMessageAccepted, projectMessage(message), f.now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			for range 2 {
				tx := f.tx(t)
				if err := chat.appendDelta(t.Context(), tx, &message, chunk, f.now); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
			}
			before, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(before) != 1 || before[0].Text != chunk+chunk {
				t.Fatalf("active snapshot: messages=%d err=%v", len(before), err)
			}
			message = before[0]
			message.Delivery = conversation.DeliveryCompleted
			tx = f.tx(t)
			if err := chat.updateMessage(t.Context(), tx, message, f.now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			events, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(events) != 4 {
				t.Fatalf("stream replay: events=%d err=%v", len(events), err)
			}
			for _, event := range events[1:3] {
				var delta struct{ Text string }
				if err := json.Unmarshal(event.Data, &delta); err != nil || delta.Text != chunk {
					t.Fatalf("delta replay changed: err=%v", err)
				}
			}
			if err := f.service.compactRetainedEvents(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
			var deltas, inline int
			if err := f.store.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_events WHERE conversation_id=? AND type='message.delta'`, record.ID).Scan(&deltas); err != nil || deltas != 0 {
				t.Fatalf("completed delta rows=%d err=%v", deltas, err)
			}
			if err := f.store.reader.QueryRowContext(t.Context(), `SELECT length(CAST(text AS BLOB))+length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB)) FROM conversation_messages WHERE id=?`, message.ID).Scan(&inline); err != nil || inline != 4 {
				t.Fatalf("final inline bytes=%d err=%v", inline, err)
			}
			after, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(after) != 1 || after[0].Text != message.Text {
				t.Fatalf("final content changed after compaction: err=%v", err)
			}
			events, err = f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(events) != 2 {
				t.Fatalf("compacted replay: events=%d err=%v", len(events), err)
			}
		})
	}
}
