package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type conversationContentFixture struct {
	mu             sync.Mutex
	content        map[string][]byte
	fail           bool
	commitThenFail bool
	onIO           func() error
	onContextIO    func(context.Context) error
	publications   int
}

func (s *conversationContentFixture) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if s.onContextIO != nil {
		if err := s.onContextIO(ctx); err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
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

func (s *conversationContentFixture) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.onContextIO != nil {
		if err := s.onContextIO(ctx); err != nil {
			return nil, err
		}
	}
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
	t.Parallel()
	for _, size := range []int{32, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := newConversationFixture(t)
			objects := &conversationContentFixture{}
			f.store.Contents = objects
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			record := f.create(t, f.record(f.owner, "payload", now))
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Text: strings.Repeat("x", size), Data: json.RawMessage(`{"value":"` + strings.Repeat("y", size) + `"}`), Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: now, UpdatedAt: now}
			if err := f.appendMessage(t, t.Context(), &message); err != nil {
				t.Fatal(err)
			}
			body := map[string]string{"text": strings.Repeat("z", size)}
			if _, err := f.appendEvent(t, t.Context(), record.ID, conversation.EventHeartbeat, body, now); err != nil {
				t.Fatal(err)
			}
			var messageText, messageData, messageActor, eventBody []byte
			if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT text, data_json, actor_json FROM conversation_messages WHERE organization_id=$1 AND id=$2`, f.organization, message.ID).Scan(&messageText, &messageData, &messageActor); err != nil {
				t.Fatal(err)
			}
			if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT body_json FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2`, f.organization, record.ID).Scan(&eventBody); err != nil {
				t.Fatal(err)
			}
			messageBytes, eventBytes := len(messageText)+len(messageData)+len(messageActor), len(eventBody)
			if messageBytes != 4 || eventBytes != 2 {
				t.Fatalf("inline payload bytes: message=%d event=%d; want 4 and 2", messageBytes, eventBytes)
			}
			got, err := f.listMessages(t.Context(), record.ID, 0, 10)
			if err != nil || len(got) != 1 || got[0].Text != message.Text || !bytes.Equal(got[0].Data, message.Data) || got[0].Actor != message.Actor {
				t.Fatalf("message round trip: messages=%d err=%v", len(got), err)
			}
			events, err := f.listEvents(t.Context(), record.ID, 0, 10)
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

func TestConversationPayloadRequiresPublication(t *testing.T) {
	t.Parallel()
	for _, configured := range []bool{false, true} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			objects := &conversationContentFixture{}
			f := newConversationWorkerFixture(t, objects)
			requireNativeStatus(t, f.bind(t, f.bindBody()), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-publication", "turn_id": "turn-publication"}, map[string]any{"type": "delta", "provider_item_id": "publication-message", "text": "private content"}), http.StatusAccepted)
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			counts := func() (messages, events, attachments int) {
				t.Helper()
				if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2),(SELECT count(*) FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2),(SELECT count(*) FROM conversation_attachments WHERE organization_id=$1 AND conversation_id=$2)`, f.project.OrganizationID, f.record.ID).Scan(&messages, &events, &attachments); err != nil {
					t.Fatal(err)
				}
				return
			}
			beforeMessages, beforeEvents, beforeAttachments := counts()
			if beforeMessages != 1 {
				t.Fatalf("publication fixture messages=%d, want 1", beforeMessages)
			}
			beforePublications := objects.publications
			if configured {
				objects.setFail(true)
			} else {
				f.chat.Store.Contents = nil
			}
			message := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", f.token, conversation.Command{Key: "publication-message", Kind: conversation.CommandMessage, Text: "unpublished private message"})
			if message.Code < 400 {
				t.Fatalf("unpublished message was accepted: status=%d", message.Code)
			}
			event := f.turnEvents(t, map[string]any{"type": "item", "kind": "tool", "provider_item_id": "publication-tool", "summary": "private event", "data": map[string]any{"tool": "example"}})
			if event.Code < 400 {
				t.Fatalf("unpublished event was accepted: status=%d", event.Code)
			}
			delta := f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "publication-message", "text": "more private text"})
			if delta.Code < 400 {
				t.Fatalf("unpublished delta was accepted: status=%d", delta.Code)
			}
			if configured && objects.publications <= beforePublications {
				t.Fatal("fixture never attempted publication")
			}
			messages, events, attachments := counts()
			if messages != beforeMessages || events != beforeEvents || attachments != beforeAttachments {
				t.Fatalf("failed publication left messages=%d events=%d attachments=%d; want %d/%d/%d", messages, events, attachments, beforeMessages, beforeEvents, beforeAttachments)
			}
			f.chat.Store.Contents = objects
			stored := f.messages(t)
			if len(stored) != 1 {
				t.Fatalf("failed publication changed message population: %d", len(stored))
			}
			for _, message := range stored {
				if message.Text != "private content" {
					t.Fatalf("failed publication changed content: %q", message.Text)
				}
			}

		})
	}
}

func postgresContentTestMessage(t *testing.T, f *postgresStorageArchiveFixture, conversationID string, role conversation.Role, text string, delivery conversation.Delivery) conversationMessageRecord {
	t.Helper()
	message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: conversationID, Role: role, Kind: conversation.MessageText, Text: text, Delivery: delivery, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.scope.Credential.ID}, CreatedAt: f.now(), UpdatedAt: f.now()}
	raw, err := json.Marshal(conversationContent{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.conversations.Store.PublishPayload(t.Context(), string(f.project.OrganizationID), raw)
	if err != nil {
		t.Fatal(err)
	}
	message.ContentID = contentArchiveID(string(f.project.OrganizationID), ref)
	f.write(t, func(w tenantstore.Writer) error {
		if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: f.project.OrganizationID, ID: message.ContentID, Reference: ref, CreatedAt: f.now()}); err != nil {
			return err
		}
		row, err := w.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, message))
		if err == nil {
			message = conversationMessageFromStore(row)
		}
		return err
	})
	message.Text = text
	return message
}

func postgresContentTestDelta(t *testing.T, f *postgresStorageArchiveFixture, message conversationMessageRecord, text string, offset int64) attachment.ArchiveReference {
	t.Helper()
	raw, err := json.Marshal(conversationDeltaFact{MessageID: message.ID, Text: text, Offset: offset, Bytes: len(text)})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.conversations.Store.PublishPayload(t.Context(), string(f.project.OrganizationID), raw)
	if err != nil {
		t.Fatal(err)
	}
	id := contentArchiveID(string(f.project.OrganizationID), ref)
	f.write(t, func(w tenantstore.Writer) error {
		if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: f.project.OrganizationID, ID: id, Reference: ref, CreatedAt: f.now()}); err != nil {
			return err
		}
		_, err := w.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: message.ConversationID, Event: conversation.Event{Type: conversation.EventMessageDelta, Data: raw, CreatedAt: f.now()}, ContentReference: id})
		return err
	})
	return ref
}

func TestConversationSnapshotIgnoresStoredDeltas(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"snapshot", "messages"} {
		for _, tt := range []struct {
			name              string
			completed, legacy bool
		}{{name: "active"}, {name: "completed", completed: true}, {name: "legacy active", legacy: true}, {name: "legacy completed", completed: true, legacy: true}} {
			t.Run(endpoint+"/"+tt.name, func(t *testing.T) {
				f := newPostgresStorageArchiveFixture(t)
				issue := f.closed(t)
				record := f.messages(t, issue.WorkItemID, 0, conversation.VisibilityShared)
				text := "stored prefix 🌍"
				delivery := conversation.DeliveryCompleted
				if !tt.completed {
					text += " and suffix"
					delivery = conversation.DeliveryResponding
				}
				message := postgresContentTestMessage(t, f, record.ID, conversation.RoleAssistant, text, delivery)
				for _, delta := range []struct {
					text   string
					offset int64
				}{{"stored ", 0}, {"prefix 🌍", int64(len("stored "))}, {" and suffix", int64(len("stored prefix 🌍"))}} {
					ref := postgresContentTestDelta(t, f, message, delta.text, delta.offset)
					f.bodies.mu.Lock()
					delete(f.bodies.content, ref.Key)
					f.bodies.mu.Unlock()
				}
				if tt.legacy {
					if _, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), `UPDATE conversation_messages SET stored_text_bytes=NULL WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, message.ID); err != nil {
						t.Fatal(err)
					}
				}
				reads := 0
				f.bodies.onIO = func() error { reads++; return nil }
				deltaQueries := 0
				readContext := conversationObjectIOContext(t, f.bodies, func(_ time.Time, query string) {
					if strings.Contains(query, "FROM conversation_events") && strings.Contains(query, "delta_offset") {
						deltaQueries++
					}
				})
				var raw json.RawMessage
				var err error
				if endpoint == "snapshot" {
					raw, err = f.service.readConversationSnapshot(readContext, f.scope, record.ID)
				} else {
					raw, err = f.service.readConversationMessages(readContext, f.scope, record.ID, 0, 10)
				}
				if err != nil {
					t.Fatalf("canonical read touched unavailable retained delta objects: %v", err)
				}
				var response conversationSnapshot
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Messages) != 1 || response.Messages[0].Text != text {
					t.Fatalf("message content changed: %+v", response.Messages)
				}
				if deltaQueries != 0 {
					t.Fatalf("canonical message read queried retained deltas %d times", deltaQueries)
				}
				if reads != 1 {
					t.Fatalf("canonical message read fetched %d objects, want 1", reads)
				}
			})
		}
	}
}

func TestConversationCanonicalReadRejectsInvalidBodyLength(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name             string
		stored, declared int64
	}{{"stored bytes", 1, 2}, {"declared bytes", 2, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			issue := f.closed(t)
			record := f.messages(t, issue.WorkItemID, 0, conversation.VisibilityShared)
			message := postgresContentTestMessage(t, f, record.ID, conversation.RoleAssistant, "xx", conversation.DeliveryResponding)
			if _, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), `UPDATE conversation_messages SET stored_text_bytes=$3,text_bytes=$4 WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, message.ID, tt.stored, tt.declared); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.readConversationMessages(t.Context(), f.scope, record.ID, 0, 10); err == nil {
				t.Fatal("canonical content exceeding its persisted byte boundary was accepted")
			}
		})
	}
}

func TestConversationObjectReadsReleaseDatabaseTransactions(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"snapshot", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			issue := f.closed(t)
			record := f.messages(t, issue.WorkItemID, 0, conversation.VisibilityPrivate)
			message := postgresContentTestMessage(t, f, record.ID, conversation.RoleAssistant, "streamed text", conversation.DeliveryResponding)
			saved := postgresContentTestMessage(t, f, record.ID, conversation.RoleUser, "saved text", conversation.DeliverySaved)
			delta := postgresContentTestDelta(t, f, message, "streamed text", 0)
			question := conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, MessageID: message.ID, Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "prompt", Question: "Continue?"}}, CreatedAt: f.now(), UpdatedAt: f.now()}
			f.write(t, func(w tenantstore.Writer) error {
				return w.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Question: question})
			})
			ctx := conversationObjectIOContext(t, f.bodies)
			read := func(scope hubscope.NativeScope) (json.RawMessage, error) {
				if endpoint == "snapshot" {
					return f.service.readConversationSnapshot(ctx, scope, record.ID)
				}
				return f.service.readConversationMessages(ctx, scope, record.ID, 0, 10)
			}
			reads := 0
			f.bodies.onIO = func() error { reads++; return nil }
			other := conversationFixturePrincipal(t, f.nativeFixture, "different-owner")
			otherCredential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(other.Token), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("authenticate different owner: status=%d error=%v", status, err)
			}
			denied := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: otherCredential}
			_, err = read(denied)
			var deniedError *hubapi.NativeError
			if !errors.As(err, &deniedError) || deniedError.Code != "not_found" {
				t.Fatalf("unauthorized read result=%v", err)
			}
			if reads != 0 {
				t.Fatalf("unauthorized read fetched %d objects", reads)
			}
			f.bodies.onIO = func() error {
				reads++
				if reads == 1 {
					f.bodies.mu.Lock()
					delete(f.bodies.content, delta.Key)
					f.bodies.mu.Unlock()
				}
				return nil
			}
			raw, err := read(f.scope)
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
				t.Fatalf("expected canonical message object reads, got %d", reads)
			}
		})
	}
}

func TestConversationEventStorageFootprint(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	compressed := map[string][]byte{}
	f.store.Contents = conversationArchiveStore{Call: func(_ context.Context, request entryauthority.ContentRequest) ([]byte, error) {
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
	beforeObjects := objectBytes()
	const count = 32
	var inline int64
	for index := range count {
		body := map[string]any{"text": strings.Repeat("representative event content ", 2048), "index": index}
		raw, err := hubapi.MarshalNative(body)
		if err != nil {
			t.Fatal(err)
		}
		inline += int64(len(raw))
		if _, err := f.appendEvent(t, t.Context(), record.ID, conversation.EventHeartbeat, body, now); err != nil {
			t.Fatal(err)
		}
	}
	var metadata int64
	var rows int
	if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT count(*),coalesce(sum(pg_column_size(e)),0) FROM conversation_events e WHERE organization_id=$1 AND conversation_id=$2`, f.organization, record.ID).Scan(&rows, &metadata); err != nil {
		t.Fatal(err)
	}
	if rows != count {
		t.Fatalf("stored events=%d, want %d", rows, count)
	}
	archived := metadata + objectBytes() - beforeObjects
	saved := float64(inline-archived) / count
	t.Logf("fixture: events=%d uncompressed_body_total=%d archived_postgres_rows_plus_gzip_total=%d net_bytes_saved_per_event=%.2f", count, inline, archived, saved)
	if saved <= 0 {
		t.Fatalf("fixture did not save bytes: %.2f", saved)
	}
}

func TestConversationPublicationRechecksTokenExpiry(t *testing.T) {
	t.Parallel()
	objects := &conversationContentFixture{}
	now := time.Now().UTC().Truncate(time.Second)
	var clockMu sync.Mutex
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects}, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	})
	record := f.create(t, f.token, map[string]any{"title": "publication expiry"}).Conversation
	if result, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), "UPDATE api_tokens SET expires_at=$1 WHERE organization_id=$2 AND id=$3", now.Add(time.Minute), f.project.OrganizationID, f.ownerID); err != nil {
		t.Fatal(err)
	} else if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("expiry rows=%d, error=%v", count, err)
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
		t.Fatal("fixture did not advance the clock during publication")
	}
	var messages, events int
	if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2),(SELECT count(*) FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2)", f.project.OrganizationID, record.ID).Scan(&messages, &events); err != nil || messages != 0 || events != 0 {
		t.Fatalf("expired write left messages=%d events=%d error=%v", messages, events, err)
	}
}

func TestConversationRejectedCreateDoesNotPublish(t *testing.T) {
	t.Parallel()
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
				featureless := billingstore.PlanReference{ID: "example-featureless", Version: 1}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					assignment, err := w.BillingAssignment(t.Context())
					if errors.Is(err, corestore.ErrNotFound) {
						if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: f.project.OrganizationID, PlanReference: featureless, Name: "Example featureless", Features: []string{}, Allowances: map[string]int64{}}); err != nil {
							return err
						}
						if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: f.project.OrganizationID, Base: featureless}, 0); err != nil {
							return err
						}
						assignment, err = w.BillingAssignment(t.Context())
					}
					if err != nil {
						return err
					}
					plans := []billingstore.PlanReference{assignment.Base}
					if assignment.Subscription != nil {
						plans = append(plans, *assignment.Subscription)
					}
					for _, reference := range plans {
						plan, err := w.BillingPlan(t.Context(), reference)
						if err != nil {
							return err
						}
						plan.Features = []string{}
						if err := w.ReplaceBillingPlan(t.Context(), plan); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if f.service.database.plansService().Plans() == nil {
					f.service.database.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: hostedplans.HostedPlansConfig{Base: hostedplans.PlanReference{ID: featureless.ID, Version: featureless.Version}, WindowSeconds: 60, RetentionWindows: 10}})
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

func TestConversationTurnBatchValidationReadsEachControlOnce(t *testing.T) {
	t.Parallel()
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
			f := newConversationWorkerFixture(t, objects)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
			f.queue(t, conversation.MessageText, "steer-1", "Keep going", nil)
			requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
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
	t.Parallel()
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
			f := newConversationWorkerFixture(t, objects)
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

func TestConversationUsesExistingArchiveBackend(t *testing.T) {
	t.Parallel()
	objects := map[string][]byte{}
	writes := 0
	server := &Service{database: &database{}, notifications: newNotificationBroker(), config: Config{Logger: discardLogger(), Clock: time.Now}}
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
	chat := newConversationService(server, (ConversationConfig{Enabled: true}).Normalized())
	server.conversations = chat
	t.Cleanup(chat.Stop)
	for index := range 2 {
		text := "archive message " + strconv.Itoa(index)
		payload, err := messageContent(conversationMessageRecord{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := chat.Store.PublishPayload(t.Context(), "org_example", payload)
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
