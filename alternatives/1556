package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
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

func TestConversationRecordPayloadBackfill(t *testing.T) {
	f := newStorageArchiveFixture(t)
	f.store.contents = convertedConversationContent{service: f.service}
	scope := nativeScope{organization: f.organization, project: f.project.ID}
	var records []conversationRecord
	questions := map[string]conversationQuestionRecord{}
	receipts := map[string]conversation.Receipt{}
	attachments := map[string]string{}
	attachmentContents := map[string]string{}
	for range 2 {
		record := f.record(f.owner, "legacy record", f.now)
		record.Status = conversation.StatusSettled
		record.Execution = conversation.Execution{Status: conversation.ExecutionCompleted, Owner: conversation.Owner{LeaseID: "example-lease-" + record.ID}, Settled: true, UpdatedAt: f.now}
		record = f.createLegacyConversation(t, record)
		if _, err := f.store.db.ExecContext(t.Context(), `UPDATE conversations SET query_metadata_version=0,execution_settled=NULL WHERE id=?`, record.ID); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		question := conversationQuestionRecord{Question: conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, Status: conversation.QuestionAnswered, Prompts: []conversation.Prompt{{ID: "prompt", Question: "example question"}}, Answers: map[string][]string{"prompt": {record.ID}}, CreatedAt: f.now, UpdatedAt: f.now}}
		prompts, err := json.Marshal(question.Prompts)
		if err != nil {
			t.Fatal(err)
		}
		answers, err := json.Marshal(question.Answers)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(t.Context(), `INSERT INTO conversation_questions(id,conversation_id,status,prompts_json,answers_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, question.ID, record.ID, question.Status, string(prompts), string(answers), conversationTime(f.now), conversationTime(f.now)); err != nil {
			t.Fatal(err)
		}
		questions[record.ID] = question
		receipt := conversation.Receipt{Key: "command", Kind: conversation.CommandMessage, Status: conversation.DeliveryDelivered, UpdatedAt: f.now}
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(t.Context(), `INSERT INTO conversation_commands(conversation_id,key,kind,request_hash,receipt_json,receipt_status,query_metadata_version,created_at,updated_at) VALUES(?,?,?,?,?,NULL,0,?,?)`, record.ID, receipt.Key, receipt.Kind, "hash", string(raw), conversationTime(f.now), conversationTime(f.now)); err != nil {
			t.Fatal(err)
		}
		receipts[record.ID] = receipt
		ref := "legacy:" + record.ID
		if _, err := f.store.db.ExecContext(t.Context(), `INSERT INTO conversation_attachments(id,conversation_id,principal_id,name,mime,size,artifact_ref,created_at) VALUES(?,?,?,?,?,?,?,?)`, conversation.NewAttachmentID(), record.ID, f.owner, "example.txt", "text/plain", len(record.ID), ref, conversationTime(f.now)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(t.Context(), "INSERT INTO conversation_attachment_blobs(artifact_ref,content) VALUES(?,?)", ref, []byte(record.ID)); err != nil {
			t.Fatal(err)
		}
		attachments[record.ID] = ref
		attachmentContents[record.ID] = record.ID
	}
	for _, family := range storageConversionFamilies {
		if !family.recordPayload() {
			continue
		}
		if err := f.service.discoverStorageFamily(t.Context(), family, f.now); err != nil {
			t.Fatal(err)
		}
		for index, record := range records {
			mutated := false
			f.hook = func(request entryauthority.ContentRequest) error {
				if f.store.db.Stats().InUse != 0 {
					return errors.New("object I/O held writer during conversion")
				}
				if !request.Write || mutated {
					return nil
				}
				mutated = true
				var query string
				var raw []byte
				var err error
				identifier := record.ID
				switch family.name {
				case "conversation_records":
					records[index].Execution.UpdatedAt = f.now.Add(time.Minute)
					raw, err = json.Marshal(records[index].Execution)
					query = "UPDATE conversations SET execution_json=? WHERE id=?"
				case "conversation_questions":
					question := questions[record.ID]
					question.Prompts = append([]conversation.Prompt(nil), question.Prompts...)
					question.Prompts[0].Question = "updated example question"
					questions[record.ID] = question
					raw, err = json.Marshal(question.Prompts)
					query = "UPDATE conversation_questions SET prompts_json=? WHERE conversation_id=?"
				case "conversation_commands":
					receipt := receipts[record.ID]
					receipt.UpdatedAt = f.now.Add(time.Minute)
					receipts[record.ID] = receipt
					raw, err = json.Marshal(receipt)
					query = "UPDATE conversation_commands SET receipt_json=? WHERE conversation_id=?"
				case "conversation_attachments":
					attachmentContents[record.ID] = strings.ToUpper(record.ID)
					raw = []byte(attachmentContents[record.ID])
					query = "UPDATE conversation_attachment_blobs SET content=? WHERE artifact_ref=?"
					identifier = attachments[record.ID]
				}
				if err != nil {
					return err
				}
				var value any = string(raw)
				if family.name == "conversation_attachments" {
					value = raw
				}
				_, err = f.store.db.ExecContext(t.Context(), query, value, identifier)
				return err
			}
			if err := f.service.convertStorageAggregate(t.Context(), scope, family, record.ID, f.now); err != nil {
				t.Fatal(err)
			}
			if !mutated {
				t.Fatalf("%s did not publish before checking its source", family.name)
			}
			var contentID string
			if err := f.store.reader.QueryRowContext(t.Context(), "SELECT r.content_attachment_id FROM "+family.from+" WHERE "+family.aggregate+"=?", record.ID).Scan(&contentID); err != nil || contentID != "" {
				t.Fatalf("%s archived stale source: ref=%q err=%v", family.name, contentID, err)
			}
			var converted, cursor int64
			var complete bool
			progress := `SELECT converted_rows,cursor_rowid,complete FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`
			if err := f.store.reader.QueryRowContext(t.Context(), progress, scope.organization, scope.project, family.name, record.ID).Scan(&converted, &cursor, &complete); err != nil || converted != 0 || cursor != 0 || complete {
				t.Fatalf("%s advanced changed source: converted=%d cursor=%d complete=%v err=%v", family.name, converted, cursor, complete, err)
			}
			if err := f.service.convertStorageAggregate(t.Context(), scope, family, record.ID, f.now); err != nil {
				t.Fatal(err)
			}
			if err := f.service.convertStorageAggregate(t.Context(), scope, family, record.ID, f.now); err != nil {
				t.Fatal(err)
			}
			if err := f.store.reader.QueryRowContext(t.Context(), progress, scope.organization, scope.project, family.name, record.ID).Scan(&converted, &cursor, &complete); err != nil || converted != 1 || cursor == 0 || !complete {
				t.Fatalf("%s did not resume exactly once: converted=%d cursor=%d complete=%v err=%v", family.name, converted, cursor, complete, err)
			}
		}
	}
	assertMetadata := func() {
		t.Helper()
		for _, record := range records {
			var executionStatus, leaseID string
			var settled sql.NullBool
			var version int
			if err := f.store.reader.QueryRowContext(t.Context(), `SELECT execution_status,execution_lease_id,execution_settled,query_metadata_version FROM conversations WHERE id=?`, record.ID).Scan(&executionStatus, &leaseID, &settled, &version); err != nil || version != 1 || executionStatus != string(record.Execution.Status) || leaseID != record.Execution.Owner.LeaseID || !settled.Valid || settled.Bool != record.Execution.Settled {
				t.Fatalf("converted execution metadata lost: status=%q lease=%q settled=%+v version=%d err=%v", executionStatus, leaseID, settled, version, err)
			}
			var receiptStatus sql.NullString
			if err := f.store.reader.QueryRowContext(t.Context(), `SELECT receipt_status,query_metadata_version FROM conversation_commands WHERE conversation_id=?`, record.ID).Scan(&receiptStatus, &version); err != nil || version != 1 || !receiptStatus.Valid || receiptStatus.String != string(receipts[record.ID].Status) {
				t.Fatalf("converted receipt metadata lost: status=%+v version=%d err=%v", receiptStatus, version, err)
			}
		}
	}
	assertMetadata()
	if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertMetadata()
	for _, record := range records {
		got, err := f.store.readConversationByID(t.Context(), f.store.reader, record.ID)
		if err != nil || got.ContentID == "" || !reflect.DeepEqual(got.Execution, record.Execution) || got.Preferences != record.Preferences {
			t.Fatalf("record conversion changed payload: %+v, %v", got, err)
		}
		question, err := f.store.readQuestion(t.Context(), f.store.reader, record.ID, questions[record.ID].ID)
		if err != nil || !reflect.DeepEqual(question.Prompts, questions[record.ID].Prompts) || !reflect.DeepEqual(question.Answers, questions[record.ID].Answers) {
			t.Fatalf("question conversion changed payload: %+v, %v", question, err)
		}
		receipt, found, err := f.service.conversations.readReceipt(t.Context(), f.store.reader, record.ID, "command")
		if err != nil || !found || !reflect.DeepEqual(receipt, receipts[record.ID]) {
			t.Fatalf("receipt conversion changed payload: %+v, %v", receipt, err)
		}
		content, err := f.store.readAttachmentContent(t.Context(), f.store.reader, attachments[record.ID])
		if err != nil || string(content) != attachmentContents[record.ID] {
			t.Fatalf("attachment conversion changed payload: %q, %v", content, err)
		}
		var inline int
		if err := f.store.reader.QueryRowContext(t.Context(), `SELECT (SELECT length(execution_json)+length(preferences_json) FROM conversations WHERE id=?)+(SELECT length(prompts_json)+length(answers_json) FROM conversation_questions WHERE conversation_id=?)+(SELECT length(receipt_json) FROM conversation_commands WHERE conversation_id=?)`, record.ID, record.ID, record.ID).Scan(&inline); err != nil || inline != 10 {
			t.Fatalf("inline bytes=%d, %v", inline, err)
		}
	}
	var blobs int
	if err := f.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachment_blobs").Scan(&blobs); err != nil || blobs != 0 {
		t.Fatalf("remaining inline attachment blobs=%d, %v", blobs, err)
	}
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
