package conversation_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type interleavedTurnContent struct {
	mu          sync.Mutex
	beforeWrite func()
}

type interleavedTurnRequest struct{}

func (c *interleavedTurnContent) arm(hook func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beforeWrite = hook
}

func (c *interleavedTurnContent) wrap(content entryauthority.ContentHandler) entryauthority.ContentHandler {
	return func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if request.Write && ctx.Value(interleavedTurnRequest{}) == true {
			c.mu.Lock()
			hook := c.beforeWrite
			c.beforeWrite = nil
			c.mu.Unlock()
			if hook != nil {
				hook()
			}
		}
		return content(ctx, request)
	}
}

func (f workerConversationProject) submitTurnBatch(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	request := hubfixture.NewRequest(t, http.MethodPost, f.path()+"/turn-events", f.worker, body)
	request = request.WithContext(context.WithValue(request.Context(), interleavedTurnRequest{}, true))
	response := httptest.NewRecorder()
	f.project.Hub.Handler().ServeHTTP(response, request)
	return response
}

func (f workerConversationProject) turnBatch(key string, events ...map[string]any) map[string]any {
	return map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt, "batch_key": key, "events": events}
}

func (f workerConversationProject) release(t *testing.T) {
	t.Helper()
	response := f.project.Hub.Request(t, http.MethodPost, f.project.Base+"/leases/"+string(f.lease.ID)+"/release", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, Reason: "completed"})
	hubfixture.RequireStatus(t, response, http.StatusNoContent)
}

func (f *workerConversationProject) replaceOwner(t *testing.T) {
	t.Helper()
	f.release(t)
	hub := f.project.Hub
	itemPath := f.project.Base + "/work-items/" + string(f.issue.WorkItemID)
	current := f.project.Request(t, http.MethodGet, itemPath, nil)
	hubfixture.RequireStatus(t, current, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, current, &issue)
	claim := hub.Request(t, http.MethodPost, f.project.Base+"/claims", f.worker, hubfixture.ProviderClaim(f.runner, issue, hubapi.NewID("session")))
	hubfixture.RequireStatus(t, claim, http.StatusOK)
	hubfixture.Decode(t, claim, &f.lease)
	start := hubfixture.StartedEvent(f.lease)
	start.IdempotencyKey = hubapi.NewID("start")
	f.attempt, f.run = start.Data.AttemptID, start.Data.RunID
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, itemPath+"/events", f.worker, start), http.StatusOK)
	bindWorkerConversation(t, *f)
}

type interleavingCase struct {
	name          string
	intervene     func(*testing.T, *workerConversationProject, int64, map[string]any)
	fencingOffset tracker.FencingToken
	duplicate     bool
	stale         bool
}

func TestConversationTurnEventsSurviveInterleavedWrites(t *testing.T) {
	t.Parallel()
	content := &interleavedTurnContent{}
	cfg := conversationTestConfig()
	cfg.Content = content.wrap(cfg.Content)
	hub := hubfixture.Open(t, cfg)
	for _, test := range []interleavingCase{
		{name: "own control poll", intervene: func(t *testing.T, f *workerConversationProject, cursor int64, _ map[string]any) {
			hubfixture.RequireStatus(t, f.controls(t, cursor, 0), http.StatusOK)
		}},
		{name: "user command", intervene: func(t *testing.T, f *workerConversationProject, _ int64, _ map[string]any) {
			request := conversation.Command{Key: "steer-1", Kind: conversation.CommandMessage, Text: "Also update the docs"}
			response := f.project.Request(t, http.MethodPost, f.path()+"/commands", request)
			hubfixture.RequireStatus(t, response, http.StatusOK)
		}},
		{name: "duplicate delivery", duplicate: true, intervene: func(t *testing.T, f *workerConversationProject, _ int64, body map[string]any) {
			hubfixture.RequireStatus(t, f.submitTurnBatch(t, body), http.StatusAccepted)
		}},
		{name: "released lease", stale: true, intervene: func(t *testing.T, f *workerConversationProject, _ int64, _ map[string]any) {
			f.release(t)
		}},
		{name: "replaced owner", stale: true, intervene: func(t *testing.T, f *workerConversationProject, _ int64, _ map[string]any) {
			f.replaceOwner(t)
		}},
		{name: "wrong fencing token", fencingOffset: 1, stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWorkerConversationProject(t, hub, "Interleaving "+test.name)
			answer := checkInterleavedTurn(t, content, f, test)
			if !test.duplicate {
				return
			}
			other := newWorkerConversationProject(t, hub, "Interleaving sibling project")
			otherAnswer := checkInterleavedTurn(t, content, other, test)
			if otherAnswer == answer {
				t.Fatal("the second project reused the first project's assistant message")
			}
			assertTurnAnswer(t, readRouteConversation(t, f.project, f.id), answer)
		})
	}
}

func checkInterleavedTurn(t *testing.T, content *interleavedTurnContent, f workerConversationProject, test interleavingCase) string {
	t.Helper()
	start := f.turnBatch("start-1", map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"})
	hubfixture.RequireStatus(t, f.submitTurnBatch(t, start), http.StatusAccepted)
	stop := conversation.Command{Key: "interrupt-1", Kind: conversation.CommandInterrupt, Expected: conversation.Expected{AttemptID: f.attempt}}
	queued := f.project.Request(t, http.MethodPost, f.path()+"/commands", stop)
	hubfixture.RequireStatus(t, queued, http.StatusOK)
	var receipt conversation.Receipt
	hubfixture.Decode(t, queued, &receipt)
	polled := f.controls(t, 0, 0)
	hubfixture.RequireStatus(t, polled, http.StatusOK)
	var page conversations.WorkerControlsResponse
	hubfixture.Decode(t, polled, &page)
	if !slices.ContainsFunc(page.Controls, func(control conversations.WorkerControl) bool {
		return control.Key == receipt.Key && control.MessageID == receipt.MessageID && control.Kind == "interrupt"
	}) {
		t.Fatal("worker did not receive the queued interrupt")
	}
	batch := f.turnBatch("batch-1",
		map[string]any{"type": "control_result", "key": receipt.Key, "status": "delivered"},
		map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "partial answer"})
	if test.fencingOffset != 0 {
		batch["fencing_token"] = f.lease.FencingToken + test.fencingOffset
	}
	before := readRouteConversation(t, f.project, f.id)
	middle := before
	if test.intervene != nil {
		content.arm(func() {
			test.intervene(t, &f, page.Cursor, batch)
			middle = readRouteConversation(t, f.project, f.id)
		})
	}
	result := f.submitTurnBatch(t, batch)
	content.arm(nil)
	if test.intervene != nil && middle.Cursor == before.Cursor {
		t.Fatal("the intervening write did not publish")
	}
	after := readRouteConversation(t, f.project, f.id)
	if test.stale {
		hubfixture.RequireCode(t, result, http.StatusConflict, "stale_execution")
		if after.Cursor != middle.Cursor {
			t.Fatal("a stale batch published events")
		}
		for _, message := range after.Messages {
			if message.ProviderItemID != nil && *message.ProviderItemID == "item-1" {
				t.Fatal("a stale batch published assistant text")
			}
			if message.ID == receipt.MessageID && message.Delivery == conversation.DeliveryDelivered {
				t.Fatal("a stale batch delivered the interrupt")
			}
		}
		return ""
	}

	hubfixture.RequireStatus(t, result, http.StatusAccepted)
	var accepted conversations.WorkerTurnEventsResponse
	hubfixture.Decode(t, result, &accepted)
	if accepted.EventSeq != after.Cursor {
		t.Fatalf("worker cursor = %d, replay cursor = %d", accepted.EventSeq, after.Cursor)
	}
	if !slices.ContainsFunc(after.Messages, func(message conversations.ConversationMessageResource) bool {
		return message.ID == receipt.MessageID && message.Delivery == conversation.DeliveryDelivered
	}) {
		t.Fatal("the interrupt was not delivered")
	}
	var answer string
	for _, message := range after.Messages {
		if message.ProviderItemID != nil && *message.ProviderItemID == "item-1" {
			answer = message.ID
			break
		}
	}
	assertTurnAnswer(t, after, answer)
	eventCursor := middle.Cursor
	if test.duplicate {
		if after.Cursor != middle.Cursor {
			t.Fatal("a concurrent duplicate batch published events")
		}
		eventCursor = before.Cursor
	}
	requireTurnEventOrder(t, f, eventCursor)

	replayed := f.submitTurnBatch(t, batch)
	hubfixture.RequireStatus(t, replayed, http.StatusAccepted)
	var replayReceipt conversations.WorkerTurnEventsResponse
	hubfixture.Decode(t, replayed, &replayReceipt)
	settled := readRouteConversation(t, f.project, f.id)
	if replayReceipt.EventSeq != accepted.EventSeq || settled.Cursor != after.Cursor {
		t.Fatal("a redelivered batch published events")
	}
	assertTurnAnswer(t, settled, answer)
	return answer
}

func assertTurnAnswer(t *testing.T, snapshot conversations.ConversationSnapshot, answer string) {
	t.Helper()
	found := false
	for _, message := range snapshot.Messages {
		if message.ProviderItemID == nil || *message.ProviderItemID != "item-1" {
			continue
		}
		if message.ID != answer || message.Text != "partial answer" {
			t.Fatalf("unexpected assistant response %s: %q", message.ID, message.Text)
		}
		found = true
	}
	if !found {
		t.Fatal("the partial assistant answer is missing")
	}
}

func requireTurnEventOrder(t *testing.T, f workerConversationProject, cursor int64) {
	t.Helper()
	wanted := []conversation.EventType{conversation.EventMessageUpdated, conversation.EventCommandReceipt, conversation.EventMessageAccepted, conversation.EventMessageDelta}
	err := f.project.Hub.Repository.ReadTenant(t.Context(), f.project.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
		events, err := reader.ConversationEvents(t.Context(), f.id, cursor, 100)
		if err != nil {
			return err
		}
		index := 0
		for _, event := range events.Items {
			if event.Type == wanted[index] {
				index++
				if index == len(wanted) {
					return nil
				}
			}
		}
		t.Fatalf("turn replay is missing %s in order", wanted[index])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
