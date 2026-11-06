package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func TestConversationRecordPayloadsNeverInline(t *testing.T) {
	f := newConversationFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f.store.contents.(*conversationContentFixture).onIO = func() error {
		if f.store.db.Stats().InUse != 0 {
			return errors.New("object I/O held the writer connection")
		}
		return nil
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "payloads", now))
	t.Run("execution and preferences", func(t *testing.T) {
		var execution, preferences string
		if err := f.store.reader.QueryRowContext(t.Context(), "SELECT execution_json,preferences_json FROM conversations WHERE id=?", record.ID).Scan(&execution, &preferences); err != nil {
			t.Fatal(err)
		}
		if execution != "{}" || preferences != "{}" {
			t.Fatalf("inline execution/preferences remain: %s / %s", execution, preferences)
		}
		got, err := f.store.readConversationByID(t.Context(), f.store.reader, record.ID)
		if err != nil || !reflect.DeepEqual(got.Execution, record.Execution) || got.Preferences != record.Preferences.Normalized() {
			t.Fatalf("execution/preferences round trip changed: %+v, %v", got, err)
		}
	})
	t.Run("question", func(t *testing.T) {
		question := conversationQuestionRecord{Question: conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "prompt", Question: "example question"}}, CreatedAt: now, UpdatedAt: now}}
		raw, err := questionPayload(question)
		if err != nil {
			t.Fatal(err)
		}
		ctx := prepareRecordTestPayload(t, f, record, raw)
		tx := f.tx(t)
		if err := f.store.upsertQuestion(ctx, tx, question); err != nil {
			t.Fatal(err)
		}
		var prompts, answers string
		if err := tx.QueryRowContext(t.Context(), "SELECT prompts_json,answers_json FROM conversation_questions WHERE id=?", question.ID).Scan(&prompts, &answers); err != nil {
			t.Fatal(err)
		}
		if prompts != "[]" || answers != "{}" {
			t.Fatalf("inline question remains: %s / %s", prompts, answers)
		}
		f.commit(t, tx)
		got, err := f.store.readQuestion(t.Context(), f.store.reader, record.ID, question.ID)
		if err != nil || !reflect.DeepEqual(got.Prompts, question.Prompts) {
			t.Fatalf("question round trip changed: %+v, %v", got, err)
		}
	})
	t.Run("receipt", func(t *testing.T) {
		initial, err := json.Marshal(conversation.Receipt{Key: "command", Kind: conversation.CommandMessage, Status: conversation.DeliverySaved, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		final, err := json.Marshal(conversation.Receipt{Key: "command", Kind: conversation.CommandMessage, Status: conversation.DeliveryDelivered, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		ctx := prepareRecordTestPayload(t, f, record, initial, final)
		tx := f.tx(t)
		if _, _, err := f.store.reserveCommand(ctx, tx, record.ID, "command", conversation.CommandMessage, "hash", now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.updateReceipt(ctx, tx, record.ID, "command", conversation.Receipt{Key: "command", Kind: conversation.CommandMessage, Status: conversation.DeliveryDelivered, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		var receipt string
		if err := tx.QueryRowContext(t.Context(), "SELECT receipt_json FROM conversation_commands WHERE conversation_id=?", record.ID).Scan(&receipt); err != nil {
			t.Fatal(err)
		}
		if receipt != "{}" {
			t.Fatalf("inline receipt remains: %s", receipt)
		}
		f.commit(t, tx)
		var raw, contentID string
		if err := f.store.reader.QueryRowContext(t.Context(), "SELECT receipt_json,content_attachment_id FROM conversation_commands WHERE conversation_id=? AND key='command'", record.ID).Scan(&raw, &contentID); err != nil {
			t.Fatal(err)
		}
		got, err := f.store.decodeReceipt(t.Context(), f.store.reader, record.ID, raw, contentID)
		if err != nil || got.Status != conversation.DeliveryDelivered {
			t.Fatalf("receipt round trip changed: %+v, %v", got, err)
		}
	})
	t.Run("attachment", func(t *testing.T) {
		ctx, err := f.store.preparePayload(t.Context(), string(record.OrganizationID), []byte("example"))
		if err != nil {
			t.Fatal(err)
		}
		tx := f.tx(t)
		attachment := conversationAttachmentRecord{ID: conversation.NewAttachmentID(), ConversationID: record.ID, OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, PrincipalID: record.OwnerPrincipalID, Name: "example.txt", MIME: "text/plain", Size: 7, ArtifactRef: "example-ref", CreatedAt: now}
		if err := f.store.insertAttachment(ctx, tx, attachment, []byte("example")); err != nil {
			t.Fatal(err)
		}
		var content []byte
		if err := tx.QueryRowContext(t.Context(), "SELECT content FROM conversation_attachment_blobs WHERE artifact_ref=?", attachment.ArtifactRef).Scan(&content); err != sql.ErrNoRows {
			t.Fatalf("inline attachment remains: %q, %v", content, err)
		}
		f.commit(t, tx)
		got, err := f.store.readAttachmentContent(t.Context(), f.store.reader, attachment.ArtifactRef)
		if err != nil || string(got) != "example" {
			t.Fatalf("attachment round trip changed: %q, %v", got, err)
		}
	})
}

func prepareRecordTestPayload(t *testing.T, f conversationFixture, record conversationRecord, values ...[]byte) context.Context {
	t.Helper()
	contents := make([][]byte, 0, len(values))
	for _, raw := range values {
		content, err := messageContent(conversationMessageRecord{Data: raw})
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, content)
	}
	ctx, err := f.store.preparePayloads(t.Context(), string(record.OrganizationID), contents...)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func prepareTestConversationRecord(t *testing.T, store *conversationStore, record conversationRecord) context.Context {
	t.Helper()
	raw, err := recordPayload(record)
	if err != nil {
		t.Fatal(err)
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.preparePayload(t.Context(), string(record.OrganizationID), content)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func mustMarshalConversationExecution(t *testing.T, store *conversationStore, id string, mutate func(*conversation.Execution)) string {
	t.Helper()
	record, err := store.readConversationByID(t.Context(), store.reader, id)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&record.Execution)
	}
	raw, err := json.Marshal(record.Execution)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConversationPayloadConversionRejectsInvalidMetadata(t *testing.T) {
	for _, raw := range []string{`{"item_id":[]}`, `{"tool":[]}`} {
		t.Run(raw, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			f.store.contents = convertedConversationContent{service: f.service}
			record := f.record(f.owner, "invalid legacy metadata", f.now)
			record.Status = conversation.StatusSettled
			record = f.create(t, record)
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: "original output", Data: json.RawMessage(`{}`), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: f.now, UpdatedAt: f.now}
			tx := f.tx(t)
			if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			if _, err := f.store.db.ExecContext(t.Context(), "UPDATE conversation_messages SET data_json=?,query_metadata_version=0 WHERE id=?", raw, message.ID); err != nil {
				t.Fatal(err)
			}
			before := len(f.objects)
			for _, family := range storageConversionFamilies {
				if family.name != "messages" {
					continue
				}
				if err := f.service.discoverStorageFamily(t.Context(), family, f.now); err != nil {
					t.Fatal(err)
				}
				if err := f.service.convertStorageAggregate(t.Context(), nativeScope{organization: f.organization, project: f.project.ID}, family, record.ID, f.now); err == nil {
					t.Fatal("invalid message metadata was converted")
				}
			}
			var text, data, contentID string
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT text,data_json,content_attachment_id FROM conversation_messages WHERE id=?", message.ID).Scan(&text, &data, &contentID); err != nil {
				t.Fatal(err)
			}
			if text != message.Text || data != raw || contentID != "" || len(f.objects) != before {
				t.Fatalf("invalid source was changed: text=%q data=%q ref=%q", text, data, contentID)
			}
		})
	}
}
