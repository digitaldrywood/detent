package hubserver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func TestConversationToolReplayAfterBackfillAndUpdate(t *testing.T) {
	for _, latest := range []struct{ name, text string }{
		{name: "shorter", text: "new"},
		{name: "longer", text: "replacement output longer than the historical output"},
		{name: "data_only", text: "original historical output"},
	} {
		t.Run(latest.name, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			record := f.record(f.owner, "legacy tool replay", f.now)
			record.Status = conversation.StatusSettled
			record = f.create(t, record)
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Delivery: conversation.DeliveryCompleted, Text: "original historical output", Data: json.RawMessage(`{"tool":"example","item_id":"item","status":"running"}`), Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: f.now, UpdatedAt: f.now}
			tx := f.tx(t)
			if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageAccepted, projectMessage(message), f.now); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			f.store.contents = convertedConversationContent{service: f.service}
			before, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			f.hook = func(request entryauthority.ContentRequest) error {
				if request.Write {
					if _, err := f.store.db.ExecContext(t.Context(), "UPDATE conversations SET title=title WHERE id=?", record.ID); err != nil {
						return fmt.Errorf("writer held during tool payload publication: %w", err)
					}
				}
				return nil
			}
			for range 4 {
				if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
			}
			var archived bool
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT content_attachment_id<>'' FROM conversation_messages WHERE id=?", message.ID).Scan(&archived); err != nil || !archived {
				t.Fatalf("legacy message was not converted before update: archived=%v err=%v", archived, err)
			}
			message.Text = latest.text
			message.Data = json.RawMessage(`{"tool":"example","item_id":"item","status":"completed"}`)
			content, err := messageContent(message)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := f.store.preparePayload(t.Context(), string(f.project.OrganizationID), content)
			if err != nil {
				t.Fatal(err)
			}
			tx = f.tx(t)
			if err := f.store.updateMessage(ctx, tx, message); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			after, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("historical replay changed after converted tool update: err=%v", err)
			}
		})
	}
}
