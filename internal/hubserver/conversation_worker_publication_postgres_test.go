package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func checkPGWorkerMessagePage(t *testing.T) {
	f := newConversationWorkerFixture(t)
	plan, err := f.chat.newPublicationPlan(t.Context(), f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]conversationMessageRecord, corestore.MaxPageSize+1)
	for index := range messages {
		message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: f.record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: fmt.Sprintf("message-%d", index), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.principal}, CreatedAt: f.now, UpdatedAt: f.now}
		messages[index], err = plan.message(t.Context(), message)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(t.Context(), writer, f.project.OrganizationID, plan.prepared, f.now); err != nil {
			return err
		}
		for _, message := range messages {
			if _, err := writer.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, message)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	actor, err := json.Marshal(messages[0].Actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), `UPDATE conversation_messages SET actor_json=$1,actor_kind=CASE WHEN id=$2 THEN NULL ELSE actor_kind END,actor_principal_id=CASE WHEN id=$3 THEN NULL ELSE actor_principal_id END WHERE organization_id=$4 AND conversation_id=$5 AND id IN ($2,$3)`, string(actor), messages[0].ID, messages[1].ID, f.project.OrganizationID, f.record.ID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query string
		start int
		count int
		more  bool
	}{
		{"limit=500", 0, len(messages), false},
		{"limit=200", 1, corestore.MaxPageSize, true},
		{"limit=500&before=2", 0, 1, false},
	} {
		t.Run(test.query, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+f.record.ID+"/messages?"+test.query, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page conversationMessagesPage
			decodeHubResponse(t, response, &page)
			if len(page.Messages) != test.count || (page.NextCursor != nil) != test.more {
				t.Fatalf("page count=%d cursor=%v", len(page.Messages), page.NextCursor)
			}
			for index, actual := range page.Messages {
				expected := messages[test.start+index]
				if actual.ID != expected.ID || actual.Text != expected.Text || actual.Actor != expected.Actor {
					t.Fatalf("page message %d changed identity, content, actor or order", index)
				}
			}
		})
	}
}

func checkPGWorkerCompletionHistory(t *testing.T) {
	for _, test := range []struct {
		name       string
		history    int
		messageID  bool
		extraDelta bool
	}{
		{name: "completion after 200 completed messages", history: corestore.MaxPageSize},
		{name: "provider delta after 201 completed messages", history: corestore.MaxPageSize + 1, extraDelta: true},
		{name: "message delta after 201 completed messages", history: corestore.MaxPageSize + 1, messageID: true, extraDelta: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			plan, err := f.chat.newPublicationPlan(t.Context(), f.record.ID)
			if err != nil {
				t.Fatal(err)
			}
			history, err := plan.message(t.Context(), conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: f.record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "historical response", Delivery: conversation.DeliveryCompleted, AttemptID: f.attempt, TurnID: "turn-history", Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.principal}, CreatedAt: f.now, UpdatedAt: f.now})
			if err != nil {
				t.Fatal(err)
			}
			other := history
			other.ID, other.TurnID, other.Delivery = conversation.NewMessageID(), "turn-other", conversation.DeliveryResponding
			f.writeTenant(t, func(writer tenantstore.Writer) error {
				if err := registerPreparedConversationObjects(t.Context(), writer, f.project.OrganizationID, plan.prepared, f.now); err != nil {
					return err
				}
				for range test.history {
					row := history
					row.ID = conversation.NewMessageID()
					if _, err := writer.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, row)); err != nil {
						return err
					}
				}
				if test.extraDelta {
					_, err := writer.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, other))
					return err
				}
				return nil
			})
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "turn_started", "thread_id": "thread-current", "turn_id": "turn-current"},
				map[string]any{"type": "delta", "provider_item_id": "assistant-current", "text": "first"},
			), http.StatusAccepted)
			var current conversationstore.Message
			f.readTenant(t, func(reader tenantstore.Reader) error {
				page, err := reader.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: f.record.ID, AttemptID: f.attempt, TurnID: "turn-current", Role: conversation.RoleAssistant, Limit: 2})
				if err == nil && len(page.Items) != 1 {
					t.Fatalf("current assistant messages = %d, want 1", len(page.Items))
				}
				if err == nil {
					current = page.Items[0]
				}
				return err
			})
			owner := f.load(t).Execution.Owner
			completion := []map[string]any{}
			wantText := "first"
			if test.extraDelta {
				delta := map[string]any{"type": "delta", "provider_item_id": "assistant-current", "text": " second"}
				if test.messageID {
					delta["message_id"] = current.ID
				}
				completion = append(completion, delta)
				wantText += " second"
			}
			completion = append(completion, map[string]any{"type": "turn_completed", "turn_id": "turn-current", "status": "completed"})
			requireNativeStatus(t, f.turnEvents(t, completion...), http.StatusAccepted)
			message, err := f.chat.readPublicationMessage(t.Context(), f.project.OrganizationID, f.record.ID, current.ID)
			if err != nil || message.Text != wantText || message.Delivery != conversation.DeliveryCompleted || message.ContentID == "" {
				t.Fatalf("completed assistant = %+v, error = %v", message, err)
			}
			replay, err := f.replay(t)
			if err != nil {
				t.Fatal(err)
			}
			completed := false
			for _, event := range replay {
				if event.Type != conversation.EventMessageUpdated {
					continue
				}
				var resource conversationMessageResource
				if err := json.Unmarshal(event.Data, &resource); err != nil {
					t.Fatal(err)
				}
				if resource.ID == current.ID && resource.Delivery == conversation.DeliveryCompleted {
					completed = true
					if resource.Text != wantText {
						t.Fatalf("completion replay text = %q, want %q", resource.Text, wantText)
					}
				}
			}
			if !completed {
				t.Fatal("completion replay omitted the current assistant")
			}
			owner.TurnID = ""
			if record := f.load(t); record.Execution.Owner != owner || record.Execution.Status != conversation.ExecutionRunning {
				t.Fatalf("completion changed execution ownership: %+v", record.Execution)
			}
			var unchanged int
			if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND attempt_id=$3 AND turn_id='turn-history' AND delivery='completed' AND revision=1 AND content_attachment_id=$4`, f.project.OrganizationID, f.record.ID, f.attempt, history.ContentID).Scan(&unchanged); err != nil || unchanged != test.history {
				t.Fatalf("unrelated history changed: count=%d want=%d error=%v", unchanged, test.history, err)
			}
			if test.extraDelta {
				f.readTenant(t, func(reader tenantstore.Reader) error {
					message, err := reader.Message(t.Context(), f.record.ID, other.ID)
					if err == nil && (message.Delivery != conversation.DeliveryResponding || message.Revision != 1 || message.ContentReference != other.ContentID) {
						t.Fatalf("another turn changed: %+v", message)
					}
					return err
				})
			}
		})
	}
}
