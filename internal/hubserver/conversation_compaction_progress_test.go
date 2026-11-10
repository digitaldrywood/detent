package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func seedCompactionProgress(t *testing.T, prefix int, delivery conversation.Delivery) (streamFixture, conversationstore.Conversation, conversationstore.Message) {
	t.Helper()
	f := streamFixture{conversationAPIFixture: newConversationAPIFixture(t, nil)}
	id := f.create(t, f.token, map[string]any{"title": "Example active history"}).Conversation.ID
	now := time.Now().UTC().Truncate(time.Microsecond)
	var message conversationstore.Message
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		var err error
		message, err = writer.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: f.project.OrganizationID, ConversationID: id, ID: conversation.NewMessageID(), Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Example streamed response", Delivery: delivery, Actor: conversation.Actor{Kind: conversation.ActorRunner}, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			return err
		}
		for range prefix {
			if _, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: id, Event: conversation.Event{Type: conversation.EventQuestionUpdated, Data: []byte(`{"status":"answered"}`), CreatedAt: now}}); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(conversationDeltaBody(message.ID, 0, "fragment"))
		if err != nil {
			return err
		}
		_, err = writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: id, Event: conversation.Event{Type: conversation.EventMessageDelta, Data: raw, CreatedAt: now}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f, readStreamCompactionConversation(t, f, id), message
}

func checkCompactionRestartEligibility(t *testing.T) {
	f, before, message := seedCompactionProgress(t, 0, conversation.DeliveryResponding)
	now := time.Now().UTC()
	if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 0 {
		t.Fatalf("streaming scan: removed=%d error=%v", removed, err)
	}
	if scanned := readStreamCompactionConversation(t, f, before.ID); scanned.Compaction.ContentScannedSequence != before.EventSequence {
		t.Fatalf("streaming scan incomplete: %+v", scanned.Compaction)
	}
	f.service = tfConvReopenConversationService(t, f.service)
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		stored, err := reader.Message(t.Context(), before.ID, message.ID)
		if err == nil && stored.Delivery != conversation.DeliveryUnknown {
			t.Fatalf("restart delivery=%s, want unknown", stored.Delivery)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after := readStreamCompactionConversation(t, f, before.ID); after.EventSequence != before.EventSequence || after.Status != conversation.StatusActive {
		t.Fatalf("restart changed event head or active state: %+v", after)
	}
	if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 1 {
		t.Fatalf("restart terminal delta excluded: removed=%d error=%v", removed, err)
	}
}

func checkCompactionAppendProgress(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) { checkCompactionAppendMode(t, full) })
	}
}

func checkCompactionAppendMode(t *testing.T, full bool) {
	f, record, _ := seedCompactionProgress(t, 700, conversation.DeliveryCompleted)
	now := time.Now().UTC()
	if full {
		setProgressConversationOld(t, f, record.ID, now)
	}
	if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 0 {
		t.Fatalf("retained prefix scan: removed=%d error=%v", removed, err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		_, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Event: conversation.Event{Type: conversation.EventQuestionUpdated, Data: []byte(`{"status":"answered"}`), CreatedAt: now}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 1 {
		t.Fatalf("append starved terminal delta beyond retained prefix: removed=%d error=%v", removed, err)
	}
	if got := readStreamCompactionConversation(t, f, record.ID); got.Compaction.ContentScannedSequence != got.EventSequence || got.Compaction.ThroughSequence != record.EventSequence {
		t.Fatalf("append scan did not reach terminal delta: %+v", got.Compaction)
	}
}

func setProgressConversationOld(t *testing.T, f streamFixture, id string, now time.Time) {
	t.Helper()
	old := now.Add(-31 * 24 * time.Hour)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		record, err := writer.Conversation(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		record.Status, record.UpdatedAt, record.SettledAt, record.LastMessageAt = conversation.StatusSettled, old, &old, &old
		_, err = writer.UpdateConversation(t.Context(), record, record.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func checkCompactionPredecessorInvalidation(t *testing.T) {
	for _, kind := range []conversation.EventType{conversation.EventMessageUpdated, conversation.EventExecutionUpdated, conversation.EventConversationUpdated} {
		t.Run(string(kind), func(t *testing.T) {
			f, record, message := seedCompactionProgress(t, 0, conversation.DeliveryCompleted)
			now := time.Now().UTC()
			setProgressConversationOld(t, f, record.ID, now)
			body, err := json.Marshal(map[string]string{"id": message.ID, "attempt_id": "attempt_example", "status": "completed", "title": "Same snapshot"})
			if err != nil {
				t.Fatal(err)
			}
			appendEvent := func() {
				t.Helper()
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
					_, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Event: conversation.Event{Type: kind, Data: body, CreatedAt: now}})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent()
			if kind == conversation.EventConversationUpdated {
				appendEvent()
			}
			if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 1 {
				t.Fatalf("initial compaction: removed=%d error=%v", removed, err)
			}
			appendEvent()
			if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 1 {
				t.Fatalf("new replacement did not retire predecessor: removed=%d error=%v", removed, err)
			}
		})
	}
}

type compactionLookupPlan struct {
	NodeType  string                 `json:"Node Type"`
	Index     string                 `json:"Index Name"`
	Condition string                 `json:"Index Cond"`
	Rows      float64                `json:"Actual Rows"`
	Loops     float64                `json:"Actual Loops"`
	Removed   float64                `json:"Rows Removed by Filter"`
	Plans     []compactionLookupPlan `json:"Plans"`
}

func checkCompactionPendingLookup(t *testing.T) {
	f := streamFixture{conversationAPIFixture: newConversationAPIFixture(t, nil)}
	target := f.create(t, f.token, map[string]any{"title": "Example closed history"}).Conversation.ID
	other := f.create(t, f.token, map[string]any{"title": "Example unrelated pending"}).Conversation.ID
	admin := conversationTestAdmin(t, f.service)
	for _, row := range []struct{ id, delivery string }{{target, "completed"}, {other, "queued"}} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages (organization_id,conversation_id,id,seq,role,kind,text,data_json,delivery,actor_json,created_at,updated_at) SELECT $1,$2,'msg_'||md5($2||'/'||n::text),n,'assistant','text','','{}'::jsonb,$3,'{}'::jsonb,clock_timestamp(),clock_timestamp() FROM generate_series(1,4000) n`, f.project.OrganizationID, row.id, row.delivery); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), "ANALYZE conversation_messages"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := admin.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT (SELECT m.id FROM conversation_messages m WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.delivery IN ('saved','queued','sending','responding') ORDER BY m.id LIMIT 1) IS NULL`, f.project.OrganizationID, target).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	t.Logf("pending lookup plan: %s", raw)
	var plans []struct{ Plan compactionLookupPlan }
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	found := false
	var walk func(compactionLookupPlan)
	walk = func(node compactionLookupPlan) {
		if node.Index == "conversation_messages_compaction_pending_idx" {
			found = strings.Contains(node.Condition, "organization_id") && strings.Contains(node.Condition, "conversation_id") && node.Rows*node.Loops+node.Removed <= 1
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	for _, plan := range plans {
		walk(plan.Plan)
	}
	if !found {
		t.Fatal("pending lookup lacks bounded tenant/conversation index access")
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+other, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
}
