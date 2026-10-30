package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func TestConversationRestartNormalizesArchivedRecords(t *testing.T) {
	f := newConversationFixture(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var ids []string
	for _, status := range []conversation.ExecutionStatus{conversation.ExecutionRunning, conversation.ExecutionWaitingInput} {
		record := f.record(f.owner, "restart archived payload", now)
		record.Execution = conversation.Execution{Status: status, Owner: conversation.Owner{AttemptID: string(status)}, Capabilities: conversation.Capabilities{Answer: true}, UpdatedAt: now}
		record = f.create(t, record)
		ids = append(ids, record.ID)
		initial := conversation.Receipt{Key: "command", Kind: conversation.CommandMessage, Status: conversation.DeliverySaved, UpdatedAt: now}
		pending := initial
		pending.Status = conversation.DeliverySending
		initialRaw, err := json.Marshal(initial)
		if err != nil {
			t.Fatal(err)
		}
		pendingRaw, err := json.Marshal(pending)
		if err != nil {
			t.Fatal(err)
		}
		ctx := prepareRecordTestPayload(t, f, record, initialRaw, pendingRaw)
		tx := f.tx(t)
		if _, _, err := f.store.reserveCommand(ctx, tx, record.ID, initial.Key, initial.Kind, "hash", now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.updateReceipt(ctx, tx, record.ID, pending.Key, pending); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
	}
	f.store.contents.(*conversationContentFixture).onIO = func() error {
		_, err := f.store.db.ExecContext(t.Context(), "UPDATE conversations SET title=title")
		return err
	}
	summary, err := f.store.normalizeAfterRestart(t.Context(), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Conversations != 2 || summary.Receipts != 2 {
		t.Fatalf("archived restart normalization counts=%+v, want 2 conversations and receipts", summary)
	}
	for _, id := range ids {
		record, err := f.store.readConversationByID(t.Context(), f.store.reader, id)
		if err != nil || record.Execution.Status != conversation.ExecutionUnknown || record.Execution.Owner.AttemptID == "" || record.Revision != 2 {
			t.Fatalf("normalized execution=%+v err=%v", record.Execution, err)
		}
		var raw, contentID string
		if err := f.store.reader.QueryRowContext(t.Context(), "SELECT receipt_json,content_attachment_id FROM conversation_commands WHERE conversation_id=?", id).Scan(&raw, &contentID); err != nil {
			t.Fatal(err)
		}
		receipt, err := f.store.decodeReceipt(t.Context(), f.store.reader, id, raw, contentID)
		if err != nil || receipt.Status != conversation.DeliveryUnknown || raw != "{}" || contentID == "" {
			t.Fatalf("normalized receipt=%+v inline=%q ref=%q err=%v", receipt, raw, contentID, err)
		}
	}
	again, err := f.store.normalizeAfterRestart(t.Context(), now.Add(2*time.Hour))
	if err != nil || again != (conversationNormalizeSummary{}) {
		t.Fatalf("restart normalization was not idempotent: summary=%+v err=%v", again, err)
	}
}
