package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
		ctx, err := f.store.prepareLegacyToolRevision(t.Context(), record.ID, message.ID)
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
	var selections []tracedStatement
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, args []driver.NamedValue) {
		if !strings.HasPrefix(query, "SELECT r.rowid FROM conversation_tool_revisions ") {
			return
		}
		statement := tracedStatement{query: query}
		for _, arg := range args {
			statement.args = append(statement.args, arg.Value)
		}
		selections = append(selections, statement)
	}})
	for range 12 {
		if err := f.service.convertLegacyStorage(ctx, f.now); err != nil {
			t.Fatal(err)
		}
	}
	if len(selections) == 0 {
		t.Fatal("tool revision conversion selection was not traced")
	}
	for _, statement := range selections {
		plan := strings.Join(explainQueryPlan(t, f.store.reader, statement.query, statement.args), "; ")
		if !strings.Contains(plan, "SEARCH r USING ") || (!strings.Contains(plan, "conversation_tool_revisions_payload_pending") && !strings.Contains(plan, "storage_queued_tool_revisions")) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("unbounded tool revision conversion selection: %s", plan)
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

func TestConversationLegacyToolUpdateIgnoresUnrelatedHistory(t *testing.T) {
	for _, metadataVersion := range []int{0, 1} {
		t.Run(strconv.Itoa(metadataVersion), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			objects := &conversationContentFixture{}
			f.store.contents = objects
			record := f.create(t, f.record(f.owner, "active legacy tools", f.now))
			var messages []conversationMessageRecord
			tx := f.tx(t)
			for range 101 {
				message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Delivery: conversation.DeliveryCompleted, Text: "original tool output", Data: json.RawMessage(`{"tool":"example","item_id":"item","status":"running"}`), Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: f.now, UpdatedAt: f.now}
				if len(messages) >= 2 {
					message.Text = "unrelated historical output"
				}
				if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
					t.Fatal(err)
				}
				messages = append(messages, message)
			}
			for _, message := range messages[:2] {
				if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageAccepted, projectMessage(message), f.now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE conversation_messages SET query_metadata_version=? WHERE conversation_id=?", metadataVersion, record.ID); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			before, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			f.service.conversations.coordinator = &recordingWaker{}
			coordinator := &conversationTurnCoordinator{service: f.service.conversations}
			err = coordinator.write(t.Context(), record.ID, func(ctx context.Context, tx *sql.Tx, _ *conversationRecord, now time.Time) error {
				for _, message := range messages[:2] {
					message.Text = "finished tool output"
					if err := f.service.conversations.updateMessage(ctx, tx, message, now); err != nil {
						return err
					}
				}
				return nil
			}, func(ctx context.Context, p *conversationPublicationPlan) error {
				for _, message := range messages[:2] {
					message.Text = "finished tool output"
					if err := p.updateMessage(ctx, message); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.store.listEvents(t.Context(), f.store.reader, record.ID, 0, len(before))
			if err != nil || len(before) != len(after) {
				t.Fatalf("historical tool replay changed: events=%d want=%d err=%v", len(after), len(before), err)
			}
			for index := range before {
				var original, replayed map[string]any
				if err := json.Unmarshal(before[index].Data, &original); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(after[index].Data, &replayed); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(original, replayed) || before[index].Seq != after[index].Seq || before[index].Type != after[index].Type || !before[index].CreatedAt.Equal(after[index].CreatedAt) {
					t.Fatalf("historical tool replay changed at event %d", index)
				}
			}
			for _, content := range objects.content {
				if strings.Contains(string(content), "unrelated historical output") {
					t.Fatal("published tool history unrelated to the request")
				}
			}
			var untouched, archived int
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=? AND content_attachment_id='' AND revision=1", record.ID).Scan(&untouched); err != nil || untouched != 99 {
				t.Fatalf("unrelated history changed: untouched=%d err=%v", untouched, err)
			}
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_tool_revisions WHERE conversation_id=? AND content_attachment_id<>'' AND text=''", record.ID).Scan(&archived); err != nil || archived != 2 {
				t.Fatalf("old tool payloads were not archived: archived=%d err=%v", archived, err)
			}
		})
	}
}
