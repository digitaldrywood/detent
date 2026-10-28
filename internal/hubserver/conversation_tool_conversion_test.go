package hubserver

import (
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func TestConversationToolBackfillAfterLiveUpdate(t *testing.T) {
	f := newStorageArchiveFixture(t)
	for range 2 {
		record := f.record(f.owner, "legacy tool discovery", f.now)
		record.Status = conversation.StatusSettled
		record = f.create(t, record)
		message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Delivery: conversation.DeliveryCompleted, Text: "original tool output", Data: json.RawMessage(`{"tool":"example","item_id":"item","status":"running"}`), Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: f.now, UpdatedAt: f.now}
		tx := f.tx(t)
		if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
			t.Fatal(err)
		}
		message.Text = "legacy completed output"
		if err := f.updateLegacyToolMessage(t, t.Context(), tx, message, f.now); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
		f.store.contents = convertedConversationContent{service: f.service}
		ctx, err := f.store.prepareLegacyToolRevisions(t.Context(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		message.Text = "current output"
		content, err := messageContent(message)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err = f.store.preparePayload(ctx, string(f.project.OrganizationID), content)
		if err != nil {
			t.Fatal(err)
		}
		tx = f.tx(t)
		if err := f.store.updateMessage(ctx, tx, message); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
	}
	for range 12 {
		if err := f.service.convertLegacyStorage(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
	}
	var pending, archived int
	if err := f.store.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_tool_revisions WHERE content_attachment_id=''`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("live updates stranded %d inline historical tool revisions", pending)
	}
	if err := f.store.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_tool_revisions WHERE content_attachment_id<>'' AND text=''`).Scan(&archived); err != nil || archived != 4 {
		t.Fatalf("archived revisions=%d want=4 err=%v", archived, err)
	}
}
