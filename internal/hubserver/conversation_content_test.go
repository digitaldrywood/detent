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
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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

func newConversationContentTenantFixture(t *testing.T, objects attachment.ArchiveContentStore) conversationAPIFixture {
	t.Helper()
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f.service.tokenUseWork.Wait()
	return f
}

func appendConversationContentMessage(t *testing.T, f conversationAPIFixture, message *conversationMessageRecord) {
	t.Helper()
	f.service.tokenUseWork.Wait()
	coordinator := &conversationTurnCoordinator{service: f.service.conversations}
	if err := coordinator.write(t.Context(), message.ConversationID, func(ctx context.Context, w tenantstore.Writer, record *conversationRecord, now time.Time) error {
		return coordinator.appendMessage(ctx, w, record, message, now)
	}, func(ctx context.Context, plan *conversationPublicationPlan) error {
		return plan.appendMessage(ctx, message)
	}); err != nil {
		t.Fatal(err)
	}
}

func appendConversationContentDelta(t *testing.T, f conversationAPIFixture, message *conversationMessageRecord, text string) {
	t.Helper()
	coordinator := &conversationTurnCoordinator{service: f.service.conversations}
	if err := coordinator.write(t.Context(), message.ConversationID, func(ctx context.Context, w tenantstore.Writer, _ *conversationRecord, now time.Time) error {
		return coordinator.appendDelta(ctx, w, message, text, now)
	}, func(ctx context.Context, plan *conversationPublicationPlan) error {
		return plan.appendDelta(ctx, *message, text)
	}); err != nil {
		t.Fatal(err)
	}
}

func conversationContentScope(f conversationAPIFixture) nativeScope {
	return nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: f.ownerID, Hash: apikey.HashToken(f.token), Scope: apiScopeOperator}}
}

func readConversationContentEndpoint(t *testing.T, f conversationAPIFixture, scope nativeScope, id, endpoint string) (json.RawMessage, error) {
	t.Helper()
	if endpoint == "snapshot" {
		return f.service.readConversationSnapshot(t.Context(), scope, id)
	}
	return f.service.readConversationMessages(t.Context(), scope, id, 0, 10)
}

func TestConversationPayloadBytesStayBounded(t *testing.T) {
	for _, size := range []int{32, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			objects := &conversationContentFixture{}
			f := newConversationContentTenantFixture(t, objects)
			id := f.create(t, f.token, map[string]any{"title": "payload"}).Conversation.ID
			now := f.service.config.now()
			message := conversationMessageRecord{ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Text: strings.Repeat("x", size), Data: json.RawMessage(`{"value":"` + strings.Repeat("y", size) + `"}`), Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.ownerID}, CreatedAt: now, UpdatedAt: now}
			appendConversationContentMessage(t, f, &message)
			body := map[string]string{"text": strings.Repeat("z", size)}
			coordinator := &conversationTurnCoordinator{service: f.service.conversations}
			if err := coordinator.write(t.Context(), id, func(ctx context.Context, w tenantstore.Writer, record *conversationRecord, now time.Time) error {
				return appendCoordinatorEvent(ctx, w, record.OrganizationID, id, conversation.EventHeartbeat, body, now)
			}, func(ctx context.Context, plan *conversationPublicationPlan) error { return plan.event(ctx, body) }); err != nil {
				t.Fatal(err)
			}
			var ref attachment.ArchiveReference
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.Message(t.Context(), id, message.ID)
				if err != nil {
					return err
				}
				if len(stored.Text)+len(stored.Data) != 2 || stored.ContentReference == "" || stored.Actor != message.Actor {
					t.Fatalf("inline message payload changed: text=%d data=%d reference=%q actor=%+v", len(stored.Text), len(stored.Data), stored.ContentReference, stored.Actor)
				}
				events, err := r.ConversationEvents(t.Context(), id, 0, 10)
				if err != nil {
					return err
				}
				if len(events.Items) != 2 {
					t.Fatalf("events=%d, want2", len(events.Items))
				}
				for _, event := range events.Items {
					if len(event.Data) != 2 || event.ContentReference == "" {
						t.Fatalf("inline event bytes=%d reference=%q", len(event.Data), event.ContentReference)
					}
				}
				object, err := r.ConversationContent(t.Context(), events.Items[1].ContentReference)
				ref = object.Reference
				return err
			}); err != nil {
				t.Fatal(err)
			}
			page, err := f.service.readPGConversationPage(t.Context(), conversationContentScope(f), id, 0, 10)
			if err != nil || len(page.messages) != 1 || page.messages[0].Text != message.Text || !bytes.Equal(page.messages[0].Data, message.Data) || page.messages[0].Actor != message.Actor {
				t.Fatalf("message round trip: messages=%d error=%v", len(page.messages), err)
			}
			var payload conversationContent
			if err := f.service.readConversationObject(t.Context(), string(f.project.OrganizationID), ref, &payload); err != nil {
				t.Fatal(err)
			}
			var decoded map[string]string
			if err := json.Unmarshal(payload.Data, &decoded); err != nil || !reflect.DeepEqual(decoded, body) {
				t.Fatalf("event body changed: error=%v", err)
			}
		})
	}
}

func TestConversationPayloadRequiresPublication(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			f := newConversationContentTenantFixture(t, &conversationContentFixture{})
			id := f.create(t, f.token, map[string]any{"title": "publication"}).Conversation.ID
			archiveRows := func() [3]int {
				t.Helper()
				var counts [3]int
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM conversation_content_objects WHERE organization_id=$1),(SELECT count(*) FROM archive_objects WHERE organization_id=$1),(SELECT count(*) FROM archive_object_owners WHERE organization_id=$1)`, f.project.OrganizationID).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
					t.Fatal(err)
				}
				return counts
			}
			before := archiveRows()
			if !configured {
				f.service.conversations.store.contents = nil
			}
			coordinator := &conversationTurnCoordinator{service: f.service.conversations}
			now := f.service.config.now()
			message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: id, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "private content", Data: json.RawMessage(`{"key":"value"}`), Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.ownerID}, CreatedAt: now, UpdatedAt: now}
			for _, test := range []struct {
				name  string
				write func(context.Context, tenantstore.Writer) error
			}{
				{"message", func(ctx context.Context, w tenantstore.Writer) error {
					record, err := w.Conversation(ctx, f.project.ID, id)
					if err != nil {
						return err
					}
					converted := conversationRecordFromStore(record)
					return coordinator.appendMessage(ctx, w, &converted, &message, now)
				}},
				{"event", func(ctx context.Context, w tenantstore.Writer) error {
					return appendCoordinatorEvent(ctx, w, f.project.OrganizationID, id, conversation.EventHeartbeat, map[string]string{"text": "private event"}, now)
				}},
				{"delta", func(ctx context.Context, w tenantstore.Writer) error {
					return coordinator.appendDelta(ctx, w, &message, "more private text", now)
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { return test.write(t.Context(), w) }); err == nil {
						t.Fatal("unpublished content accepted")
					}
				})
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				messages, err := r.Messages(t.Context(), id, 0, 10)
				if err != nil {
					return err
				}
				events, err := r.ConversationEvents(t.Context(), id, 0, 10)
				if err != nil {
					return err
				}
				attachments, err := r.(conversationstore.AttachmentReader).MessageAttachments(t.Context(), f.project.ID, id, []string{message.ID})
				if len(messages.Items) != 0 || len(events.Items) != 0 || len(attachments) != 0 {
					t.Fatalf("unpublished rows: messages=%d events=%d attachments=%d", len(messages.Items), len(events.Items), len(attachments))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if after := archiveRows(); after != before {
				t.Fatalf("unpublished archive/content metadata changed: got=%v want=%v", after, before)
			}
		})
	}
}

func TestConversationSnapshotIgnoresStoredDeltas(t *testing.T) {
	for _, endpoint := range []string{"snapshot", "messages"} {
		for _, test := range []struct {
			name              string
			completed, legacy bool
		}{
			{name: "active"}, {name: "completed", completed: true}, {name: "legacy active", legacy: true}, {name: "legacy completed", completed: true, legacy: true},
		} {
			t.Run(endpoint+"/"+test.name, func(t *testing.T) {
				objects := &conversationContentFixture{}
				f := newConversationContentTenantFixture(t, objects)
				id := f.create(t, f.token, map[string]any{"title": "retained deltas"}).Conversation.ID
				now := f.service.config.now()
				message := conversationMessageRecord{ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.ownerID}, CreatedAt: now, UpdatedAt: now}
				appendConversationContentMessage(t, f, &message)
				for _, text := range []string{"stored ", "prefix 🌍"} {
					appendConversationContentDelta(t, f, &message, text)
				}
				if test.completed {
					message.Delivery = conversation.DeliveryCompleted
					coordinator := &conversationTurnCoordinator{service: f.service.conversations}
					if err := coordinator.write(t.Context(), id, func(ctx context.Context, w tenantstore.Writer, _ *conversationRecord, now time.Time) error {
						return coordinator.updateMessage(ctx, w, message, now)
					}, func(ctx context.Context, plan *conversationPublicationPlan) error {
						return plan.updateMessage(ctx, message)
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					appendConversationContentDelta(t, f, &message, " and suffix")
				}
				if test.legacy {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						row, err := w.Message(t.Context(), id, message.ID)
						if err != nil {
							return err
						}
						row.StoredTextBytes = 0
						_, err = w.UpdateMessage(t.Context(), row, row.Revision)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				var canonical attachment.ArchiveReference
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.Message(t.Context(), id, message.ID)
					if err != nil {
						return err
					}
					object, err := r.ConversationContent(t.Context(), row.ContentReference)
					canonical = object.Reference
					return err
				}); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				for key := range objects.content {
					if key != canonical.Key {
						delete(objects.content, key)
					}
				}
				objects.mu.Unlock()
				reads := 0
				objects.onIO = func() error {
					reads++
					if occupied := f.service.database.postgres.Stats().AcquiredConns(); occupied != 0 {
						t.Errorf("object read holds %d database connections", occupied)
					}
					return nil
				}
				raw, err := readConversationContentEndpoint(t, f, conversationContentScope(f), id, endpoint)
				if err != nil {
					t.Fatalf("read touched retained deltas: %v", err)
				}
				var response conversationSnapshot
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Messages) != 1 || response.Messages[0].Text != message.Text {
					t.Fatalf("message content changed: %+v", response.Messages)
				}
				if reads != 1 {
					t.Fatalf("canonical object reads=%d,want1 and no delta history", reads)
				}
			})
		}
	}
}

func TestConversationDeltaHistoryCannotAmplifyCanonicalContent(t *testing.T) {
	for _, endpoint := range []string{"snapshot", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			objects := &conversationContentFixture{}
			f := newConversationContentTenantFixture(t, objects)
			id := f.create(t, f.token, map[string]any{"title": "invalid delta history"}).Conversation.ID
			now := f.service.config.now()
			message := conversationMessageRecord{ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "x", Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.ownerID}, CreatedAt: now, UpdatedAt: now}
			appendConversationContentMessage(t, f, &message)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				for range 4 {
					raw, err := json.Marshal(conversationDeltaFact{MessageID: message.ID, Text: "x", Offset: 0, Bytes: 1})
					if err != nil {
						return err
					}
					if _, err := w.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: id, Event: conversation.Event{Type: conversation.EventMessageDelta, Data: raw, CreatedAt: now}}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			reads := 0
			objects.onIO = func() error { reads++; return nil }
			raw, err := readConversationContentEndpoint(t, f, conversationContentScope(f), id, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			var response conversationSnapshot
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Messages) != 1 || response.Messages[0].Text != "x" || reads != 1 {
				t.Fatalf("malformed history amplified canonical content: messages=%+v reads=%d", response.Messages, reads)
			}
		})
	}
}

func TestConversationObjectReadsReleaseDatabaseTransactions(t *testing.T) {
	for _, endpoint := range []string{"snapshot", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			objects := &conversationContentFixture{}
			f := newConversationContentTenantFixture(t, objects)
			id := f.create(t, f.token, map[string]any{"title": "read transaction"}).Conversation.ID
			now := f.service.config.now()
			message := conversationMessageRecord{ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.ownerID}, CreatedAt: now, UpdatedAt: now}
			appendConversationContentMessage(t, f, &message)
			saved := conversationMessageRecord{ConversationID: id, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "saved text", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.ownerID}, CreatedAt: now, UpdatedAt: now}
			appendConversationContentMessage(t, f, &saved)
			appendConversationContentDelta(t, f, &message, "streamed text")
			question := conversation.Question{ID: conversation.NewQuestionID(), ConversationID: id, MessageID: message.ID, Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "prompt", Question: "Continue?"}}, CreatedAt: now, UpdatedAt: now}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: id, Question: question})
			}); err != nil {
				t.Fatal(err)
			}
			reads := 0
			objects.onIO = func() error { reads++; return nil }
			scope := conversationContentScope(f)
			deniedScope := scope
			deniedScope.credential.ID = f.otherID
			deniedScope.credential.Hash = apikey.HashToken(f.other)
			_, err := readConversationContentEndpoint(t, f, deniedScope, id, endpoint)
			var denied *nativeError
			if !errors.As(err, &denied) || denied.Code != "not_found" {
				t.Fatalf("unauthorized read=%v", err)
			}
			if reads != 0 {
				t.Fatalf("unauthorized read fetched %d objects", reads)
			}
			objects.onIO = func() error {
				reads++
				if occupied := f.service.database.postgres.Stats().AcquiredConns(); occupied != 0 {
					t.Errorf("object read holds %d database connections", occupied)
				}
				if reads == 1 {
					return f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						for _, id := range []string{message.ID, saved.ID} {
							row, err := w.Message(t.Context(), message.ConversationID, id)
							if err != nil {
								return err
							}
							row.ContentReference = ""
							row.Text = "changed after capture"
							if _, err := w.UpdateMessage(t.Context(), row, row.Revision); err != nil {
								return err
							}
						}
						return nil
					})
				}
				return nil
			}
			raw, err := readConversationContentEndpoint(t, f, scope, id, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			var response conversationSnapshot
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Messages) != 2 || response.Messages[0].Text != "streamed text" || response.Messages[1].Text != saved.Text {
				t.Fatalf("captured content changed: %+v", response.Messages)
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
			if reads != 2 {
				t.Fatalf("captured message object reads=%d,want2", reads)
			}
		})
	}
}

func TestConversationEventStorageFootprint(t *testing.T) {
	compressed := map[string][]byte{}
	objects := conversationArchiveStore{call: func(_ context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if request.Write {
			compressed[request.Reference.Key] = bytes.Clone(request.Data)
		}
		data, ok := compressed[request.Reference.Key]
		if !ok {
			return nil, errors.New("object missing")
		}
		return bytes.Clone(data), nil
	}}
	f := newConversationContentTenantFixture(t, objects)
	id := f.create(t, f.token, map[string]any{"title": "footprint"}).Conversation.ID
	compressed = map[string][]byte{}
	coordinator := &conversationTurnCoordinator{service: f.service.conversations}
	const count = 32
	var inline int64
	for index := range count {
		body := map[string]any{"text": strings.Repeat("representative event content ", 2048), "index": index}
		raw, err := marshalNative(body)
		if err != nil {
			t.Fatal(err)
		}
		inline += int64(len(raw))
		if err := coordinator.write(t.Context(), id, func(ctx context.Context, w tenantstore.Writer, record *conversationRecord, now time.Time) error {
			return appendCoordinatorEvent(ctx, w, record.OrganizationID, id, conversation.EventHeartbeat, body, now)
		}, func(ctx context.Context, plan *conversationPublicationPlan) error { return plan.event(ctx, body) }); err != nil {
			t.Fatal(err)
		}
	}
	adminValue, ok := projectRecordDatabases.Load(f.service.database.postgres)
	if !ok {
		t.Fatal("fixture PostgreSQL administrator unavailable")
	}
	var metadata int64
	if err := adminValue.(*sql.DB).QueryRowContext(t.Context(), "SELECT COALESCE(sum(pg_column_size(e)),0) FROM conversation_events e WHERE organization_id=$1 AND conversation_id=$2", f.project.OrganizationID, id).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	archived := metadata
	for _, data := range compressed {
		archived += int64(len(data))
	}
	saved := float64(inline-archived) / count
	t.Logf("fixture: events=%d inline_payload_bytes=%d archived_postgres_plus_gzip_bytes=%d net_bytes_saved_per_event=%.2f", count, inline, archived, saved)
	if saved <= 0 {
		t.Fatalf("fixture did not save bytes: %.2f", saved)
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
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		credential, err := w.AccessCredential(t.Context(), f.ownerID)
		if err != nil {
			return err
		}
		return w.RenewAccessCredential(t.Context(), credential.ID, credential.Hash, credential.Fingerprint, now.Add(time.Minute), now)
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	objects.onIO = func() error {
		calls++
		if calls == 1 {
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
	if calls < 1 {
		t.Fatal("fixture did not advance the clock after planning")
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		messages, err := r.Messages(t.Context(), record.ID, 0, 10)
		if err != nil {
			return err
		}
		events, err := r.ConversationEvents(t.Context(), record.ID, 0, 10)
		if len(messages.Items) != 0 || len(events.Items) != 0 {
			t.Fatalf("expired write left messages=%d events=%d", len(messages.Items), len(events.Items))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConversationRejectedCreateDoesNotPublish(t *testing.T) {
	for _, test := range []struct {
		name      string
		key       string
		subject   string
		noFeature bool
		status    int
	}{
		{name: "missing feature", key: "create-feature", noFeature: true, status: http.StatusTooManyRequests},
		{name: "missing key", status: http.StatusUnprocessableEntity},
		{name: "long key", key: strings.Repeat("k", 129), status: http.StatusUnprocessableEntity},
		{name: "missing subject", key: "create-subject", subject: "wi_missing", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &conversationContentFixture{}
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
			if test.noFeature {
				config := pilotHostedPlans()
				for i := range config.Plans {
					config.Plans[i].Features = []string{}
				}
				f.service.database.hostedOrganization = f.project.OrganizationID
				if err := f.service.database.configureHostedPlans(t.Context(), &HostedConfig{Plans: &config}); err != nil {
					t.Fatal(err)
				}
			}
			before := objects.publications
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token, map[string]any{
				"key": test.key, "subject_work_item_id": test.subject,
				"first_message": map[string]string{"key": "first-message", "text": "rejected opening message"},
			})
			requireNativeStatus(t, response, test.status)
			if objects.publications != before {
				t.Fatalf("rejected conversation published %d objects; want 0", objects.publications-before)
			}
		})
	}
}

func newConversationContentWorkerFixture(t *testing.T, objects attachment.ArchiveContentStore) (*conversationWorkerFixture, conversationAPIFixture) {
	t.Helper()
	operator := newConversationContentTenantFixture(t, objects)
	issue := operator.nativeFixture.create(t, "Example conversation work")
	runner := prepareRunner(t, operator.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate)
	runner.enroll(t)
	fixture := &conversationWorkerFixture{nativeFixture: operator.nativeFixture, chat: operator.service.conversations, worker: runner.redemption.Credential, principal: operator.ownerID, issue: issue, policy: hubTestPolicy().ID, attempt: newNativeID("attempt"), run: newNativeID("run")}
	fixture.lease = nativeAttemptFixtureLease(t, runner, issue, newNativeID("session"))
	fixture.startAttempt(t, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"})
	now := operator.service.config.now()
	if err := operator.service.config.TenantRepository.WriteTenant(t.Context(), operator.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: operator.project.OrganizationID, ProjectID: operator.project.ID, ID: conversation.NewConversationID(), OwnerPrincipalID: operator.ownerID, OwnerSubject: "owner@example.test", Title: "Example linked conversation", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: string(issue.WorkItemID), LinkedAt: &now, Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now})
		fixture.record = conversationRecordFromStore(stored)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return fixture, operator
}

type conversationControlReadRepository struct {
	tenantstore.Repository
	reads atomic.Int64
}

func (r *conversationControlReadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(conversationControlReadReader{Reader: reader, reads: &r.reads})
	})
}

func (r *conversationControlReadRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(writer tenantstore.Writer) error {
		return write(conversationControlReadWriter{Writer: writer, reads: &r.reads})
	})
}

type conversationControlReadReader struct {
	tenantstore.Reader
	reads *atomic.Int64
}

func (r conversationControlReadReader) MessageByCommandKey(ctx context.Context, id, key string) (conversationstore.Message, bool, error) {
	r.reads.Add(1)
	return r.Reader.MessageByCommandKey(ctx, id, key)
}

type conversationControlReadWriter struct {
	tenantstore.Writer
	reads *atomic.Int64
}

func (w conversationControlReadWriter) MessageByCommandKey(ctx context.Context, id, key string) (conversationstore.Message, bool, error) {
	w.reads.Add(1)
	return w.Writer.MessageByCommandKey(ctx, id, key)
}

func TestConversationTurnBatchValidationReadsEachControlOnce(t *testing.T) {
	reads := make(map[string]int)
	for _, test := range []struct {
		name    string
		repeats int
	}{
		{"single control result", 1},
		{"repeated control results", conversationMaxTurnEvents - 1},
	} {
		func() {
			objects := &conversationContentFixture{}
			f, operator := newConversationContentWorkerFixture(t, objects)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
			requireNativeStatus(t, operator.command(t, operator.token, f.record.ID, conversation.Command{Key: "steer-1", Kind: conversation.CommandMessage, Text: "Keep going", Expected: conversation.Expected{AttemptID: f.attempt, TurnID: "turn-1"}}), http.StatusOK)
			requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
			counted := &conversationControlReadRepository{Repository: f.service.config.TenantRepository}
			f.service.config.TenantRepository = counted
			var count int
			objects.mu.Lock()
			objects.onIO = func() error {
				count++
				return nil
			}
			objects.mu.Unlock()
			events := make([]map[string]any, 0, test.repeats+1)
			for range test.repeats {
				events = append(events, map[string]any{"type": "control_result", "key": "steer-1", "status": "delivered"})
			}
			events = append(events, map[string]any{"type": "unknown"})
			requireNativeStatus(t, f.turnEvents(t, events...), http.StatusUnprocessableEntity)
			if count := counted.reads.Load(); count != 1 {
				t.Fatalf("control metadata reads=%d,want1", count)
			}
			objects.mu.Lock()
			reads[test.name] = count
			objects.mu.Unlock()
		}()
	}
	if reads["repeated control results"] != reads["single control result"] {
		t.Fatalf("rejected batch content reads: repeated=%d single=%d", reads["repeated control results"], reads["single control result"])
	}
}

func TestConversationRejectedTurnBatchDoesNotPublish(t *testing.T) {
	for _, test := range []struct {
		name   string
		event  map[string]any
		status int
	}{
		{name: "unknown event", event: map[string]any{"type": "unknown"}, status: http.StatusUnprocessableEntity},
		{name: "invalid execution status", event: map[string]any{"type": "execution_status", "status": "invalid"}, status: http.StatusUnprocessableEntity},
		{name: "missing turn", event: map[string]any{"type": "turn_started"}, status: http.StatusUnprocessableEntity},
		{name: "another turn", event: map[string]any{"type": "turn_started", "turn_id": "other-turn"}, status: http.StatusConflict},
		{name: "missing item", event: map[string]any{"type": "delta", "text": "suffix"}, status: http.StatusUnprocessableEntity},
		{name: "long delta", event: map[string]any{"type": "delta", "provider_item_id": "delta", "text": strings.Repeat("x", conversationMaxDeltaBytes+1)}, status: http.StatusUnprocessableEntity},
		{name: "invalid kind", event: map[string]any{"type": "item", "kind": "unknown", "summary": "invalid"}, status: http.StatusUnprocessableEntity},
		{name: "invalid item data", event: map[string]any{"type": "item", "kind": "tool", "data": []string{"invalid"}}, status: http.StatusUnprocessableEntity},
		{name: "missing question request", event: map[string]any{"type": "question_opened"}, status: http.StatusUnprocessableEntity},
		{name: "invalid prompts", event: map[string]any{"type": "question_opened", "request_id": "question"}, status: http.StatusUnprocessableEntity},
		{name: "missing control key", event: map[string]any{"type": "control_result", "status": "delivered"}, status: http.StatusUnprocessableEntity},
		{name: "unknown control key", event: map[string]any{"type": "control_result", "key": "missing-control", "status": "delivered"}, status: http.StatusUnprocessableEntity},
		{name: "missing completed turn", event: map[string]any{"type": "turn_completed", "status": "completed"}, status: http.StatusUnprocessableEntity},
		{name: "completed another turn", event: map[string]any{"type": "turn_completed", "turn_id": "other-turn", "status": "completed"}, status: http.StatusConflict},
		{name: "invalid completion status", event: map[string]any{"type": "turn_completed", "turn_id": "active-turn", "status": "unknown"}, status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &conversationContentFixture{}
			f, _ := newConversationContentWorkerFixture(t, objects)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "turn_id": "active-turn"}), http.StatusAccepted)
			before := objects.publications
			response := f.turnEvents(t,
				map[string]any{"type": "item", "kind": "tool", "summary": "valid earlier event", "data": map[string]string{"output": "payload"}},
				test.event,
			)
			requireNativeStatus(t, response, test.status)
			if objects.publications != before {
				t.Fatalf("rejected batch published %d objects; want 0", objects.publications-before)
			}
		})
	}
}

func TestConversationTurnBatchKeepsSequentialTurnState(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []map[string]any
		status int
		turn   string
	}{
		{"complete before restarting", []map[string]any{{"type": "turn_started", "turn_id": "first-turn"}, {"type": "turn_completed", "turn_id": "first-turn", "status": "completed"}, {"type": "turn_started", "turn_id": "second-turn"}}, http.StatusAccepted, "second-turn"},
		{"reject another turn", []map[string]any{{"type": "turn_started", "turn_id": "first-turn"}, {"type": "turn_completed", "turn_id": "other-turn", "status": "completed"}}, http.StatusConflict, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _ := newConversationContentWorkerFixture(t, &conversationContentFixture{})
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			var before, after conversationstore.Conversation
			read := func(target *conversationstore.Conversation) {
				t.Helper()
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					*target, err = r.Conversation(t.Context(), f.project.ID, f.record.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			read(&before)
			requireNativeStatus(t, f.turnEvents(t, test.events...), test.status)
			read(&after)
			if test.status == http.StatusAccepted {
				if after.Execution.Owner.TurnID != test.turn || after.Execution.Status != conversation.ExecutionRunning {
					t.Fatalf("batch turn state=%+v", after.Execution)
				}
			} else if !reflect.DeepEqual(after.Execution, before.Execution) || after.EventSequence != before.EventSequence {
				t.Fatal("rejected batch committed a partial turn")
			}
		})
	}
}

func TestConversationUsesExistingArchiveBackend(t *testing.T) {
	objects := map[string][]byte{}
	writes := 0
	server := &Service{database: &database{}, notifications: newNotificationBroker(), config: Config{Logger: discardLogger(), now: time.Now}}
	server.config.Content = func(_ context.Context, request entryauthority.ContentRequest) ([]byte, error) {
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
	chat := newConversationService(server, (ConversationConfig{Enabled: true}).normalized())
	server.conversations = chat
	t.Cleanup(chat.stop)
	for index := range 2 {
		text := "archive message " + strconv.Itoa(index)
		payload, err := messageContent(conversationMessageRecord{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := chat.store.publishPayload(t.Context(), "org_example", payload)
		if err != nil {
			t.Fatal(err)
		}
		var got conversationContent
		if err := server.readConversationObject(t.Context(), "org_example", ref, &got); err != nil {
			t.Fatal(err)
		}
		if got.Text != text {
			t.Fatalf("archive message = %q, want %q", got.Text, text)
		}
	}
	if writes != 2 || len(objects) != 2 {
		t.Fatalf("archive writes=%d objects=%d", writes, len(objects))
	}
}
