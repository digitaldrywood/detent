package hubserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestConversationRecordPayloadsNeverInline(t *testing.T) {
	f := newConversationFixture(t)
	ctx := conversationObjectIOContext(t, f.store.Contents.(*conversationContentFixture))
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	record := f.record(f.owner, "payloads", now)
	if err := f.createRecord(ctx, &record); err != nil {
		t.Fatal(err)
	}
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	t.Run("execution and preferences", func(t *testing.T) {
		var execution, preferences string
		if err := admin.QueryRowContext(ctx, "SELECT execution_json,preferences_json FROM conversations WHERE organization_id=$1 AND id=$2", f.organization, record.ID).Scan(&execution, &preferences); err != nil {
			t.Fatal(err)
		}
		if execution != "{}" || preferences != "{}" {
			t.Fatalf("inline execution/preferences remain: %s / %s", execution, preferences)
		}
		got, err := f.readRecord(ctx, f.project.ID, record.ID)
		if err != nil || !reflect.DeepEqual(got.Execution, record.Execution) || got.Preferences != record.Preferences.Normalized() {
			t.Fatalf("execution/preferences round trip changed: %+v, %v", got, err)
		}
	})
	t.Run("question", func(t *testing.T) {
		question := conversationQuestionRecord{Question: conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "prompt", Question: "example question"}}, CreatedAt: now, UpdatedAt: now}}
		if err := f.upsertQuestion(t, ctx, question); err != nil {
			t.Fatal(err)
		}
		var prompts, answers string
		if err := admin.QueryRowContext(ctx, "SELECT prompts_json,answers_json FROM conversation_questions WHERE organization_id=$1 AND conversation_id=$2 AND id=$3", f.organization, record.ID, question.ID).Scan(&prompts, &answers); err != nil {
			t.Fatal(err)
		}
		if prompts != "[]" || answers != "{}" {
			t.Fatalf("inline question remains: %s / %s", prompts, answers)
		}
		got, err := f.readQuestion(ctx, record.ID, question.ID)
		if err != nil || !reflect.DeepEqual(got.Prompts, question.Prompts) {
			t.Fatalf("question round trip changed: %+v, %v", got, err)
		}
	})
	t.Run("receipt", func(t *testing.T) {
		if _, _, err := f.reserveCommand(t, ctx, record.ID, "command", conversation.CommandMessage, "hash", now); err != nil {
			t.Fatal(err)
		}
		if err := f.updateReceipt(t, ctx, record.ID, "command", conversation.Receipt{Key: "command", Kind: conversation.CommandMessage, Status: conversation.DeliveryDelivered, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		var raw, contentID string
		if err := admin.QueryRowContext(ctx, "SELECT receipt_json,content_attachment_id FROM conversation_commands WHERE organization_id=$1 AND conversation_id=$2 AND key='command'", f.organization, record.ID).Scan(&raw, &contentID); err != nil {
			t.Fatal(err)
		}
		if raw != "{}" || contentID == "" {
			t.Fatalf("inline or unpublished receipt remains: %s / %s", raw, contentID)
		}
		var got conversationstore.Command
		if err := f.readTenant(ctx, func(r tenantstore.Reader) error {
			var err error
			var found bool
			got, found, err = r.Command(ctx, record.ID, "command")
			if err == nil && !found {
				return corestore.ErrNotFound
			}
			return err
		}); err != nil || got.Receipt.Status != conversation.DeliveryDelivered {
			t.Fatalf("receipt round trip changed: %+v, %v", got, err)
		}
	})
	t.Run("attachment", func(t *testing.T) {
		ref, err := f.store.PublishPayload(ctx, string(f.organization), []byte("example"))
		if err != nil {
			t.Fatal(err)
		}
		contentID := contentArchiveID(string(f.organization), ref)
		expires := now.Add(time.Hour)
		attachment := conversationstore.Attachment{ID: conversation.NewAttachmentID(), ConversationID: record.ID, OrganizationID: f.organization, ProjectID: f.project.ID, PrincipalID: f.owner, Name: "example.txt", MIME: "text/plain", Size: 7, ArtifactRef: "example-ref", ContentID: contentID, CreatedAt: now, ExpiresAt: &expires}
		object := conversationstore.ContentObject{OrganizationID: f.organization, ID: contentID, Reference: ref, CreatedAt: now}
		if err := f.writeTenant(ctx, func(w tenantstore.Writer) error {
			return w.(conversationstore.AttachmentWriter).CreateConversationAttachment(ctx, attachment, object)
		}); err != nil {
			t.Fatal(err)
		}
		var inline bool
		if err := admin.QueryRowContext(ctx, "SELECT to_regclass('public.conversation_attachment_blobs') IS NOT NULL").Scan(&inline); err != nil || inline {
			t.Fatalf("inline attachment table remains: %v, %v", inline, err)
		}
		var stored conversationstore.Attachment
		var storedObject conversationstore.ContentObject
		if err := f.readTenant(ctx, func(r tenantstore.Reader) error {
			var err error
			stored, err = r.(conversationstore.AttachmentReader).ConversationAttachmentByRef(ctx, f.project.ID, record.ID, attachment.ArtifactRef)
			if err != nil {
				return err
			}
			storedObject, err = r.ConversationContent(ctx, stored.ContentID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		got, err := f.store.Contents.ReadContent(ctx, string(f.organization), storedObject.Reference)
		if err != nil || string(got) != "example" {
			t.Fatalf("attachment round trip changed: %q, %v", got, err)
		}
	})
}

type conversationIOProbeKey struct{}

func conversationObjectIOContext(t *testing.T, objects *conversationContentFixture, statements ...func(time.Time, string)) context.Context {
	t.Helper()
	type span struct{ start, end time.Time }
	var mu sync.Mutex
	var spans []span
	var operations []time.Time
	marker := new(byte)
	objects.onContextIO = func(ctx context.Context) error {
		if ctx.Value(conversationIOProbeKey{}) == marker {
			mu.Lock()
			operations = append(operations, time.Now())
			mu.Unlock()
		}
		return nil
	}
	ctx := context.WithValue(t.Context(), conversationIOProbeKey{}, marker)
	ctx = postgres.WithTransactionTiming(ctx, func(name string, start time.Time) {
		if name == "pg_callback" {
			mu.Lock()
			spans = append(spans, span{start, time.Now()})
			mu.Unlock()
		}
	}, statements...)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if len(spans) == 0 || len(operations) == 0 {
			t.Fatalf("missing attributable transaction/object evidence: spans=%d operations=%d", len(spans), len(operations))
		}
		for _, operation := range operations {
			for _, transaction := range spans {
				if !operation.Before(transaction.start) && !operation.After(transaction.end) {
					t.Error("object I/O held its operation's PostgreSQL transaction")
				}
			}
		}
	})
	return ctx
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
	ctx, err := f.store.PreparePayloads(t.Context(), string(record.OrganizationID), contents...)
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
	ctx, err := store.PreparePayloads(t.Context(), string(record.OrganizationID), content)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
