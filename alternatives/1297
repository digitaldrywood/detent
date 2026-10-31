package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

type conversationContentFixture struct {
	mu             sync.Mutex
	content        map[string][]byte
	fail           bool
	commitThenFail bool
	onIO           func() error
	publications   int
}

func (s *conversationContentFixture) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if s.onIO != nil {
		if err := s.onIO(); err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publications++
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
	if s.commitThenFail {
		return attachment.ArchiveReference{}, errors.New("upload response lost")
	}
	return ref, nil
}

func (s *conversationContentFixture) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.onIO != nil {
		if err := s.onIO(); err != nil {
			return nil, err
		}
	}
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
			if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			body := map[string]string{"text": strings.Repeat("z", size)}
			if _, err := f.appendEvent(t, t.Context(), tx, record.ID, conversation.EventHeartbeat, body, now); err != nil {
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
			if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			if _, err := f.appendEvent(t, t.Context(), tx, record.ID, conversation.EventMessageAccepted, projectMessage(message), f.now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			for range 2 {
				tx := f.tx(t)
				if err := f.appendDelta(t, chat, t.Context(), tx, &message, chunk, f.now); err != nil {
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
			if err := f.updateChatMessage(t, chat, t.Context(), tx, message, f.now); err != nil {
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

func TestConversationPayloadRequiresPublication(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			f := newConversationFixture(t)
			f.store.contents = nil
			if configured {
				f.store.contents = &conversationContentFixture{}
			}
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			record := f.createLegacyConversation(t, f.record(f.owner, "publication", now))
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "private content", Data: json.RawMessage(`{"key":"value"}`), Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CreatedAt: now, UpdatedAt: now}
			tx := f.tx(t)
			if err := f.store.appendMessage(t.Context(), tx, &message); err == nil {
				t.Fatal("unpublished message was accepted")
			}
			if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventHeartbeat, map[string]string{"text": "private event"}, now); err == nil {
				t.Fatal("unpublished event was accepted")
			}
			if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			chat := &conversationService{store: f.store}
			if err := chat.appendDeltaAt(t.Context(), tx, record.ID, message.ID, int64(len(message.Text)), "more private text", now); err == nil {
				t.Fatal("unpublished delta was accepted")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"conversation_messages", "conversation_events", "attachments"} {
				var count int
				if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s rows=%d error=%v", table, count, err)
				}
			}
		})
	}
}

type conversationDeltaQueryRecorder struct {
	nativeQueryer
	statements []string
	arguments  [][]any
}

func (q *conversationDeltaQueryRecorder) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	if strings.Contains(statement, "type='message.delta'") {
		q.statements = append(q.statements, statement)
		q.arguments = append(q.arguments, args)
	}
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}

func TestConversationSnapshotIgnoresStoredDeltas(t *testing.T) {
	for _, endpoint := range []string{"snapshot", "messages"} {
		for _, tt := range []struct {
			name      string
			completed bool
			legacy    bool
		}{
			{name: "active"},
			{name: "completed", completed: true},
			{name: "legacy active", legacy: true},
			{name: "legacy completed", completed: true, legacy: true},
		} {
			t.Run(endpoint+"/"+tt.name, func(t *testing.T) {
				f := newStorageArchiveFixture(t)
				objects := &conversationContentFixture{}
				f.store.contents = objects
				record := f.create(t, f.record(f.owner, "retained deltas", f.now))
				message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
				tx := f.tx(t)
				if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
				for _, text := range []string{"stored ", "prefix 🌍"} {
					tx = f.tx(t)
					if err := f.appendDelta(t, f.service.conversations, t.Context(), tx, &message, text, f.now); err != nil {
						t.Fatal(err)
					}
					f.commit(t, tx)
				}
				storedBytes := len(message.Text)
				if tt.completed {
					message.Delivery = conversation.DeliveryCompleted
				}
				tx = f.tx(t)
				if err := f.updateChatMessage(t, f.service.conversations, t.Context(), tx, message, f.now); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
				if !tt.completed {
					tx = f.tx(t)
					if err := f.appendDelta(t, f.service.conversations, t.Context(), tx, &message, " and suffix", f.now); err != nil {
						t.Fatal(err)
					}
					f.commit(t, tx)
				}
				if tt.legacy {
					if _, err := f.store.db.ExecContext(t.Context(), "UPDATE conversation_messages SET stored_text_bytes=NULL WHERE id=?", message.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					messages, err := f.store.listMessageRows(t.Context(), f.store.reader, record.ID, 0, 10)
					if err != nil {
						t.Fatal(err)
					}
					query := &conversationDeltaQueryRecorder{nativeQueryer: f.store.reader}
					batch := newConversationReadBatch(string(f.organization))
					if err := batch.captureMessages(t.Context(), query, messages); err != nil {
						t.Fatal(err)
					}
					if tt.completed {
						if len(query.statements) != 0 {
							t.Fatalf("completed message queried retained delta history %d times", len(query.statements))
						}
					} else {
						if len(query.statements) != 1 || len(query.arguments[0]) != 5 || query.arguments[0][2] != int64(storedBytes) || query.arguments[0][4] != int64(len(" and suffix")+1) {
							t.Fatalf("delta query did not bound the missing suffix: %+v", query.arguments)
						}
						rows, err := f.store.reader.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.statements[0], query.arguments[0]...)
						if err != nil {
							t.Fatal(err)
						}
						defer rows.Close()
						var plan strings.Builder
						for rows.Next() {
							var id, parent, unused int
							var detail string
							if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
								t.Fatal(err)
							}
							plan.WriteString(detail)
						}
						if err := errors.Join(rows.Err(), rows.Close()); err != nil {
							t.Fatal(err)
						}
						if !strings.Contains(plan.String(), "delta_offset>? AND delta_offset<?") || strings.Contains(plan.String(), "TEMP B-TREE") {
							t.Fatalf("delta suffix query scans or sorts retained history: %s", plan.String())
						}
					}
				}
				if _, err := f.store.db.ExecContext(t.Context(), `DELETE FROM attachments WHERE id IN
(SELECT content_attachment_id FROM conversation_events WHERE conversation_id=? AND message_id=? AND type='message.delta' AND delta_offset<?)`, record.ID, message.ID, storedBytes); err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: f.organization, project: f.project.ID, credential: apiCredential{ID: f.owner, Scope: apiScopeOperator}}
				objects.onIO = func() error {
					if occupied := f.store.reader.Stats().InUse + f.store.db.Stats().InUse; occupied != 0 {
						t.Errorf("object read holds %d database connections", occupied)
					}
					return nil
				}
				var raw json.RawMessage
				var err error
				if endpoint == "snapshot" {
					raw, err = f.service.readConversationSnapshot(t.Context(), scope, record.ID)
				} else {
					raw, err = f.service.readConversationMessages(t.Context(), scope, record.ID, 0, 10)
				}
				if err != nil {
					t.Fatalf("read touched retained deltas already in the archive: %v", err)
				}
				var response conversationSnapshot
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Messages) != 1 || response.Messages[0].Text != message.Text {
					t.Fatalf("message content changed: %+v", response.Messages)
				}
			})
		}
	}
}

func TestConversationDeltaReadRejectsRowsBeyondMissingBytes(t *testing.T) {
	f := newStorageArchiveFixture(t)
	f.store.contents = &conversationContentFixture{}
	record := f.create(t, f.record(f.owner, "invalid delta history", f.now))
	message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
	tx := f.tx(t)
	if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := f.appendEvent(t, t.Context(), tx, record.ID, conversation.EventMessageDelta, conversationDeltaFact{MessageID: message.ID, Text: "x", Offset: 0, Bytes: 1}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	message.TextBytes = 1
	if _, err := readConversationMessageDeltas(t.Context(), f.store.reader, message); err == nil {
		t.Fatal("delta rows exceeding the missing byte count were accepted")
	}
}

func TestConversationObjectReadsReleaseDatabaseTransactions(t *testing.T) {
	for _, endpoint := range []string{"snapshot", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			f.service.outbox.stop()
			objects := &conversationContentFixture{}
			f.store.contents = objects
			record := f.create(t, f.record(f.owner, "read transaction", f.now))
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
			tx := f.tx(t)
			if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			saved := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "saved text", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
			if err := f.appendMessage(t, t.Context(), tx, &saved); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			tx = f.tx(t)
			if err := f.appendDelta(t, f.service.conversations, t.Context(), tx, &message, "streamed text", f.now); err != nil {
				t.Fatal(err)
			}
			question := conversationQuestionRecord{Question: conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, MessageID: message.ID, Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "prompt", Question: "Continue?"}}, CreatedAt: f.now, UpdatedAt: f.now}}
			if err := f.upsertQuestion(t, t.Context(), tx, question); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			reads := 0
			read := func(scope nativeScope) (json.RawMessage, error) {
				if endpoint == "snapshot" {
					return f.service.readConversationSnapshot(t.Context(), scope, record.ID)
				}
				return f.service.readConversationMessages(t.Context(), scope, record.ID, 0, 10)
			}
			scope := nativeScope{organization: f.organization, project: f.project.ID, credential: apiCredential{ID: f.owner, Scope: apiScopeOperator}}
			deniedScope := scope
			deniedScope.credential.ID = f.other
			objects.onIO = func() error {
				reads++
				return nil
			}
			_, err := read(deniedScope)
			var denied *nativeError
			if !errors.As(err, &denied) || denied.Code != "not_found" {
				t.Fatalf("unauthorized read result=%v", err)
			}
			if reads != 0 {
				t.Errorf("unauthorized read fetched %d objects", reads)
			}
			reads = 0
			objects.onIO = func() error {
				reads++
				if occupied := f.store.reader.Stats().InUse; occupied != 0 {
					t.Errorf("object read holds %d reader connections", occupied)
				}
				if occupied := f.store.db.Stats().InUse; occupied != 0 {
					t.Errorf("object read holds %d writer connections", occupied)
				}
				if reads == 1 {
					if _, err := f.store.db.ExecContext(t.Context(), `DELETE FROM attachments
WHERE id IN (SELECT content_attachment_id FROM conversation_messages WHERE conversation_id=?
UNION SELECT content_attachment_id FROM conversation_events WHERE conversation_id=? AND type='message.delta')`, record.ID, record.ID); err != nil {
						return err
					}
					if _, err := f.store.db.ExecContext(t.Context(), "DELETE FROM conversation_events WHERE conversation_id=? AND type='message.delta'", record.ID); err != nil {
						return err
					}
				}
				return nil
			}
			raw, err := read(scope)
			if err != nil {
				t.Fatal(err)
			}
			var response conversationSnapshot
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Messages) != 2 || response.Messages[0].Text != "streamed text" || response.Messages[1].Text != saved.Text {
				t.Fatalf("streamed message changed: %+v", response.Messages)
			}
			if endpoint == "snapshot" {
				var snapshot struct {
					Questions []struct {
						ID      string                `json:"id"`
						Prompts []conversation.Prompt `json:"questions"`
					} `json:"questions"`
				}
				if err := json.Unmarshal(raw, &snapshot); err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Questions) != 1 || snapshot.Questions[0].ID != question.ID || !reflect.DeepEqual(snapshot.Questions[0].Prompts, question.Prompts) {
					t.Fatalf("question changed: %+v", snapshot.Questions)
				}
			}
			if reads < 2 {
				t.Fatalf("expected message and delta object reads, got %d", reads)
			}
		})
	}
}

func TestConversationPublicationReconcilesOutsideWriter(t *testing.T) {
	for _, outcome := range []string{"committed", "uncertain committed", "uncertain absent"} {
		t.Run(outcome, func(t *testing.T) {
			f := newConversationFixture(t)
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			record := f.create(t, f.record(f.owner, "publication", now))
			var initialReferences int
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments").Scan(&initialReferences); err != nil {
				t.Fatal(err)
			}
			objects := &conversationContentFixture{commitThenFail: outcome == "uncertain committed", fail: outcome == "uncertain absent"}
			objects.onIO = func() error {
				if f.store.db.Stats().InUse != 0 {
					return errors.New("object I/O held the writer connection")
				}
				var references int
				if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments").Scan(&references); err != nil {
					return err
				}
				if references != initialReferences {
					return errors.New("object reference preceded publication")
				}
				return nil
			}
			f.store.contents = objects
			chat := &conversationService{store: f.store, server: f.service}
			plan := &conversationPublicationPlan{service: chat, record: record, original: record, now: now}
			message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "private text", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}}
			err := plan.appendMessage(t.Context(), &message)
			if outcome == "uncertain absent" {
				if !errors.Is(err, errConversationPublicationUncertain) {
					t.Fatalf("unresolved upload result=%v", err)
				}
				var references int
				if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments").Scan(&references); err != nil || references != initialReferences {
					t.Fatalf("uncertain upload left references=%d error=%v", references, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := plan.context(t.Context())
			if err := chat.transact(ctx, func(tx *sql.Tx, now time.Time) error {
				if err := chat.appendMessage(ctx, tx, &record, &message, now); err != nil {
					return err
				}
				_, err := chat.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id=?", record.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if objects.publications != 2 || len(objects.content) != 2 {
				t.Fatalf("publications=%d objects=%d", objects.publications, len(objects.content))
			}
			var references, messages, events int
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM attachments),(SELECT count(*) FROM conversation_messages),(SELECT count(*) FROM conversation_events)").Scan(&references, &messages, &events); err != nil || references != initialReferences+2 || messages != 1 || events != 1 {
				t.Fatalf("references=%d messages=%d events=%d error=%v", references, messages, events, err)
			}
		})
	}
}

func TestConversationEventStorageFootprint(t *testing.T) {
	f := newConversationFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	compressed := map[string][]byte{}
	f.store.contents = conversationArchiveStore{call: func(_ context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if request.Write {
			compressed[request.Reference.Key] = bytes.Clone(request.Data)
		}
		data, ok := compressed[request.Reference.Key]
		if !ok {
			return nil, errors.New("object missing")
		}
		return bytes.Clone(data), nil
	}}
	objectBytes := func() int64 {
		var size int64
		for _, data := range compressed {
			size += int64(len(data))
		}
		return size
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "footprint", now))
	payloadBytes := func() int64 {
		var size int64
		if err := f.store.reader.QueryRowContext(t.Context(), "SELECT sum(payload) FROM dbstat").Scan(&size); err != nil {
			t.Fatal(err)
		}
		return size
	}
	const count = 32
	measure := func(archived bool) int64 {
		before := payloadBytes()
		beforeObjects := objectBytes()
		for index := range count {
			body := map[string]any{"text": strings.Repeat("representative event content ", 2048), "index": index}
			raw, err := marshalNative(body)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if archived {
				ctx, err = f.store.preparePayload(ctx, string(record.OrganizationID), []byte(raw))
				if err != nil {
					t.Fatal(err)
				}
			}
			tx := f.tx(t)
			if archived {
				_, err = f.store.appendEvent(ctx, tx, record.ID, conversation.EventHeartbeat, body, now)
			} else {
				if _, err := tx.ExecContext(ctx, "UPDATE conversations SET event_seq=event_seq+1 WHERE id=?", record.ID); err != nil {
					t.Fatal(err)
				}
				_, err = tx.ExecContext(ctx, "INSERT INTO conversation_events(conversation_id,seq,type,body_json,created_at,conversion_state) VALUES(?,?,?,?,?,2)", record.ID, index+1, conversation.EventHeartbeat, raw, conversationTime(now))
				if err == nil {
					metadata, metadataErr := eventQueryMetadata([]byte(raw))
					err = metadataErr
					if err == nil {
						err = metadata.write(ctx, tx, "conversation_events", "conversation_id=? AND seq=?", record.ID, index+1)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
		}
		return payloadBytes() - before + objectBytes() - beforeObjects
	}
	inline, archived := measure(false), measure(true)
	saved := float64(inline-archived) / count
	t.Logf("fixture: events=%d inline_total=%d archived_sqlite_plus_gzip_total=%d net_bytes_saved_per_event=%.2f", count, inline, archived, saved)
	if saved <= 0 {
		t.Fatalf("fixture did not save bytes: %.2f", saved)
	}
}

func TestConversationRestartMaterializesStreamBeforeCompaction(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(strconv.FormatBool(stale), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			objects := &conversationContentFixture{}
			f.store.contents = objects
			record := f.create(t, f.record(f.owner, "restart stream", f.now))
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: f.now, UpdatedAt: f.now}
			tx := f.tx(t)
			if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			if err := f.appendDelta(t, f.service.conversations, t.Context(), tx, &message, "retained streamed text", f.now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			mutated := false
			objects.onIO = func() error {
				if f.store.db.Stats().InUse != 0 {
					return errors.New("restart object I/O held the writer")
				}
				if stale && !mutated {
					mutated = true
					_, err := f.store.db.ExecContext(t.Context(), "UPDATE conversation_messages SET revision=revision+1 WHERE id=?", message.ID)
					return err
				}
				return nil
			}
			_, err := f.store.normalizeAfterRestart(t.Context(), f.now)
			if stale {
				if err == nil {
					t.Fatal("stale restart content was accepted")
				}
				var delivery string
				if err := f.store.reader.QueryRowContext(t.Context(), "SELECT delivery FROM conversation_messages WHERE id=?", message.ID).Scan(&delivery); err != nil || delivery != string(conversation.DeliveryResponding) {
					t.Fatalf("stale recovery changed delivery=%s error=%v", delivery, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.compactRetainedEvents(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
			got, err := f.store.listMessages(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || len(got) != 1 || got[0].Text != message.Text || got[0].Delivery != conversation.DeliveryUnknown {
				t.Fatalf("restart stream changed: messages=%v error=%v", got, err)
			}
			var deltas int
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_events WHERE conversation_id=? AND type='message.delta'", record.ID).Scan(&deltas); err != nil || deltas != 0 {
				t.Fatalf("compacted deltas=%d error=%v", deltas, err)
			}
		})
	}
}

func TestConversationUsesExistingArchiveBackend(t *testing.T) {
	f := newConversationFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	objects := map[string][]byte{}
	writes := 0
	f.service.config.Content = func(_ context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if f.store.db.Stats().InUse != 0 {
			return nil, errors.New("archive backend held the writer")
		}
		if request.Write {
			writes++
			objects[request.Reference.Key] = bytes.Clone(request.Data)
		}
		data, ok := objects[request.Reference.Key]
		if !ok {
			return nil, errors.New("object missing")
		}
		return bytes.Clone(data), nil
	}
	chat := newConversationService(f.service, (ConversationConfig{Enabled: true}).normalized())
	t.Cleanup(chat.stop)
	for index := range 2 {
		now := time.Date(2026, 10, 28, 12, index, 0, 0, time.UTC)
		record := f.create(t, f.record(f.owner, "local archive", now))
		plan := &conversationPublicationPlan{service: chat, record: record, original: record, now: now}
		message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "local message " + strconv.Itoa(index), Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}}
		if err := plan.appendMessage(t.Context(), &message); err != nil {
			t.Fatal(err)
		}
		ctx := plan.context(t.Context())
		if err := chat.transact(ctx, func(tx *sql.Tx, now time.Time) error {
			return chat.appendMessage(ctx, tx, &record, &message, now)
		}); err != nil {
			t.Fatal(err)
		}
		got, err := chat.store.listMessages(t.Context(), chat.store.reader, record.ID, 0, 10)
		if err != nil || len(got) != 1 || got[0].Text != message.Text || got[0].ContentID == "" {
			t.Fatalf("local archive message=%v error=%v", got, err)
		}
	}
	if writes != 4 || len(objects) != 4 {
		t.Fatalf("local archive writes=%d objects=%d", writes, len(objects))
	}
}

func TestConversationPublicationRechecksTokenExpiry(t *testing.T) {
	objects := &conversationContentFixture{}
	now := time.Now().UTC().Truncate(time.Second)
	var clockMu sync.Mutex
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects}, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	})
	record := f.create(t, f.token, map[string]any{"title": "publication expiry"}).Conversation
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(now.Add(time.Minute)), f.ownerID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	objects.onIO = func() error {
		calls++
		if calls == 2 {
			clockMu.Lock()
			now = now.Add(2 * time.Minute)
			clockMu.Unlock()
		}
		return nil
	}
	response := f.command(t, f.token, record.ID, conversation.Command{Key: "expires-during-publication", Kind: conversation.CommandMessage, Text: "private message"})
	if response.Code == http.StatusAccepted || response.Code == http.StatusOK {
		t.Fatalf("expired credential committed after publication: status=%d", response.Code)
	}
	if calls < 2 {
		t.Fatal("fixture did not advance the clock after planning")
	}
	var messages, events int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM conversation_messages WHERE conversation_id=?),(SELECT count(*) FROM conversation_events WHERE conversation_id=?)", record.ID, record.ID).Scan(&messages, &events); err != nil || messages != 0 || events != 0 {
		t.Fatalf("expired write left messages=%d events=%d error=%v", messages, events, err)
	}
}
