package hubserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationWorkerFixture drives the worker endpoints against a linked
// conversation that was seeded directly in the store: the operator API is
// built separately and the worker path must not depend on it.
type conversationWorkerFixture struct {
	nativeFixture
	mu        sync.Mutex
	now       time.Time
	chat      *conversationService
	worker    string
	principal string
	issue     tracker.NativeIssue
	policy    string
	lease     tracker.NativeLease
	attempt   string
	run       string
	record    conversationRecord
}

func newConversationWorkerFixture(t *testing.T) *conversationWorkerFixture {
	t.Helper()
	fixture := &conversationWorkerFixture{now: time.Now().UTC().Truncate(time.Second)}
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour}, now: func() time.Time {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.now
	}}
	f := newNativeFixture(t, openTestService(t, config), "", "conversation-worker")
	fixture.nativeFixture = f
	fixture.chat = f.service.conversations
	if fixture.chat == nil {
		t.Fatal("conversation service must be enabled")
	}
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	fixture.policy = policy.ID
	fixture.issue = f.create(t, "conversation-work")
	fixture.worker = f.worker(t, "conversation-worker")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", fixture.worker, map[string]any{"id": "conversation-machine", "hostname": "fixture", "display_name": "Fixture", "version": "test", "capacity": 1}), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "conversation-owner", "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	fixture.principal = token.ID
	linked := fixture.now
	record := conversationRecord{
		ID: conversation.NewConversationID(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
		OwnerPrincipalID: token.ID, OwnerSubject: "owner@example.test", Title: "linked", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
		WorkItemID: string(fixture.issue.WorkItemID), LinkedAt: &linked,
		Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: fixture.now},
		CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	tx := fixture.tx(t)
	if err := fixture.chat.store.createConversation(t.Context(), tx, &record); err != nil {
		t.Fatalf("createConversation() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fixture.record = record
	fixture.claim(t)
	return fixture
}

func (f *conversationWorkerFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *conversationWorkerFixture) tx(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// claim claims a fresh lease for the issue and starts a new attempt on it.
func (f *conversationWorkerFixture) claim(t *testing.T) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: "conversation-machine", SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &f.lease)
	f.attempt = newNativeID("attempt")
	f.run = newNativeID("run")
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
}

func (f *conversationWorkerFixture) release(t *testing.T) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/release", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
}

func (f *conversationWorkerFixture) identity() map[string]any {
	return map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt}
}

func (f *conversationWorkerFixture) bindBody() map[string]any {
	body := f.identity()
	body["run_id"] = f.run
	body["capabilities"] = map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}
	return body
}

func (f *conversationWorkerFixture) bind(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = f.bindBody()
	}
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/bind", f.worker, body)
}

func (f *conversationWorkerFixture) unbind(t *testing.T, outcome string) *httptest.ResponseRecorder {
	t.Helper()
	body := f.identity()
	body["outcome"] = outcome
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/unbind", f.worker, body)
}

func (f *conversationWorkerFixture) turnEvents(t *testing.T, events ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := f.identity()
	body["events"] = events
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, body)
}

func (f *conversationWorkerFixture) controls(t *testing.T, after int64, wait int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, f.base+"/conversations/"+f.record.ID+"/controls?after="+strconv.FormatInt(after, 10)+"&wait="+strconv.Itoa(wait), nil)
	request.Header.Set("Authorization", "Bearer "+f.worker)
	request.Header.Set("X-Detent-Lease", string(f.lease.ID))
	request.Header.Set("X-Detent-Fencing-Token", strconv.FormatInt(int64(f.lease.FencingToken), 10))
	request.Header.Set("X-Detent-Attempt", f.attempt)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

// queue seeds an accepted user control the way the operator API would:
// reserved command key, queued message and queued receipt, then committed.
func (f *conversationWorkerFixture) queue(t *testing.T, kind conversation.MessageKind, key, text string, data map[string]any) conversationMessageRecord {
	t.Helper()
	ctx := t.Context()
	tx := f.tx(t)
	record, err := f.chat.store.readConversation(ctx, tx, f.record.OrganizationID, f.record.ProjectID, f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	commandKind := map[conversation.MessageKind]conversation.CommandKind{conversation.MessageText: conversation.CommandMessage, conversation.MessageAnswer: conversation.CommandAnswer, conversation.MessageInterrupt: conversation.CommandInterrupt, conversation.MessageContinue: conversation.CommandContinue}[kind]
	now := f.service.config.now()
	if _, _, err := f.chat.store.reserveCommand(ctx, tx, record.ID, key, commandKind, "hash-"+key, now); err != nil {
		t.Fatalf("reserveCommand() error = %v", err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	message := conversationMessageRecord{Role: conversation.RoleUser, Kind: kind, Text: text, Data: encoded, Delivery: conversation.DeliveryQueued, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.principal}, CommandKey: key}
	if err := f.chat.appendMessage(ctx, tx, &record, &message, now); err != nil {
		t.Fatalf("appendMessage() error = %v", err)
	}
	receipt := conversation.Receipt{Key: key, Kind: commandKind, Status: conversation.DeliveryQueued, MessageID: message.ID}
	if data != nil {
		receipt.QuestionID, _ = data["question_id"].(string)
	}
	if err := f.chat.recordReceipt(ctx, tx, record.ID, record.Execution.Owner.AttemptID, receipt, now); err != nil {
		t.Fatalf("recordReceipt() error = %v", err)
	}
	if err := f.chat.saveConversation(ctx, tx, &record, now); err != nil {
		t.Fatalf("saveConversation() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f.chat.committed(record)
	return message
}

func (f *conversationWorkerFixture) load(t *testing.T) conversationRecord {
	t.Helper()
	record, err := f.chat.store.readConversation(t.Context(), f.service.database.db, f.record.OrganizationID, f.record.ProjectID, f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (f *conversationWorkerFixture) messages(t *testing.T) map[string]conversationMessageRecord {
	t.Helper()
	list, err := f.chat.store.listMessages(t.Context(), f.service.database.db, f.record.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]conversationMessageRecord, len(list))
	for _, message := range list {
		byID[message.ID] = message
	}
	return byID
}

func (f *conversationWorkerFixture) receipt(t *testing.T, key string) conversation.Receipt {
	t.Helper()
	var encoded string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT receipt_json FROM conversation_commands WHERE conversation_id = ? AND key = ?", f.record.ID, key).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var receipt conversation.Receipt
	if err := json.Unmarshal([]byte(encoded), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func (f *conversationWorkerFixture) questions(t *testing.T) []conversationQuestionRecord {
	t.Helper()
	rows, err := f.service.database.db.QueryContext(t.Context(), "SELECT "+conversationQuestionColumns+" FROM conversation_questions WHERE conversation_id = ? ORDER BY created_at, id", f.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	records := []conversationQuestionRecord{}
	for rows.Next() {
		record, err := scanConversationQuestion(rows)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func (f *conversationWorkerFixture) eventTypes(t *testing.T, after int64) []string {
	t.Helper()
	events, err := f.chat.store.listEvents(t.Context(), f.service.database.db, f.record.ID, after, 1000)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, string(event.Type))
	}
	return types
}

type workerControlEnvelope struct {
	Cursor      int64                     `json:"cursor"`
	Key         string                    `json:"key"`
	Kind        string                    `json:"kind"`
	MessageID   string                    `json:"message_id"`
	Text        string                    `json:"text"`
	QuestionID  string                    `json:"question_id"`
	RequestID   string                    `json:"request_id"`
	Answers     map[string][]string       `json:"answers"`
	Attachments []conversation.Attachment `json:"attachments"`
	Expected    conversation.Expected     `json:"expected"`
}

type workerBindResponse struct {
	ConversationID string `json:"conversation_id"`
	Resume         struct {
		ThreadID string `json:"thread_id"`
	} `json:"resume"`
	Pending []workerControlEnvelope `json:"pending"`
	Cursor  int64                   `json:"cursor"`
}

type workerControlsResponse struct {
	Controls []workerControlEnvelope `json:"controls"`
	Cursor   int64                   `json:"cursor"`
}

func requireConversationErrorCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	requireNativeStatus(t, response, status)
	var failure struct {
		Code string `json:"code"`
	}
	decodeHubResponse(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
}

func TestConversationWorkerBind(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	first := f.queue(t, conversation.MessageText, "key-1", "First question", nil)
	second := f.queue(t, conversation.MessageContinue, "key-2", "Keep going", nil)

	response := f.bind(t, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var bound workerBindResponse
	decodeHubResponse(t, response, &bound)
	if bound.ConversationID != f.record.ID {
		t.Fatalf("conversation_id = %q, want %q", bound.ConversationID, f.record.ID)
	}
	if len(bound.Pending) != 2 || bound.Pending[0].MessageID != first.ID || bound.Pending[1].MessageID != second.ID {
		t.Fatalf("pending = %#v", bound.Pending)
	}
	if bound.Pending[0].Kind != "message" || bound.Pending[1].Kind != "message" || bound.Pending[0].Key != "key-1" || bound.Pending[1].Text != "Keep going" {
		t.Fatalf("pending envelopes = %#v", bound.Pending)
	}
	if bound.Pending[0].Expected.AttemptID != f.attempt || bound.Cursor != second.Seq {
		t.Fatalf("expected/cursor = %#v/%d", bound.Pending[0].Expected, bound.Cursor)
	}
	record := f.load(t)
	if record.Execution.Status != conversation.ExecutionStarting || record.Execution.Owner.AttemptID != f.attempt || record.Execution.Owner.LeaseID != string(f.lease.ID) || record.Execution.Owner.FencingToken != int64(f.lease.FencingToken) || record.Execution.Owner.RunID != f.run || record.Execution.Owner.MachineID != "conversation-machine" {
		t.Fatalf("execution = %#v", record.Execution)
	}
	if !record.Execution.Capabilities.Steer || !record.Execution.Capabilities.Answer {
		t.Fatalf("capabilities = %#v", record.Execution.Capabilities)
	}
	messages := f.messages(t)
	for _, message := range []conversationMessageRecord{first, second} {
		if got := messages[message.ID]; got.Delivery != conversation.DeliverySending || got.AttemptID != f.attempt {
			t.Fatalf("message %s = %#v", message.ID, got)
		}
	}
	if receipt := f.receipt(t, "key-1"); receipt.Status != conversation.DeliverySending {
		t.Fatalf("receipt = %#v", receipt)
	}

	t.Run("same attempt binds once", func(t *testing.T) {
		requireConversationErrorCode(t, f.bind(t, nil), http.StatusConflict, "stale_execution")
	})
	t.Run("stale fencing token", func(t *testing.T) {
		body := f.bindBody()
		body["fencing_token"] = f.lease.FencingToken + 1
		body["attempt_id"] = newNativeID("attempt")
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
	})
	t.Run("unknown attempt", func(t *testing.T) {
		body := f.bindBody()
		body["attempt_id"] = newNativeID("attempt")
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
	})
	t.Run("missing identity is invalid", func(t *testing.T) {
		body := f.bindBody()
		delete(body, "lease_id")
		requireNativeStatus(t, f.bind(t, body), http.StatusUnprocessableEntity)
	})
	t.Run("issue without conversation", func(t *testing.T) {
		other := f.create(t, "no-conversation")
		body := f.bindBody()
		requireConversationErrorCode(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(other.WorkItemID)+"/conversation/bind", f.worker, body), http.StatusNotFound, "not_found")
	})
	t.Run("foreign project worker", func(t *testing.T) {
		foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-project")
		token := foreign.worker(t, "foreign-worker")
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/bind", token, f.bindBody()), http.StatusNotFound)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/conversations/"+f.record.ID+"/turn-events", token, map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt, "events": []map[string]any{}}), http.StatusNotFound)
	})
}

func TestConversationWorkerControlsLongPoll(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	first := f.queue(t, conversation.MessageText, "steer-1", "Also do this", nil)
	response := f.controls(t, 0, 5)
	requireNativeStatus(t, response, http.StatusOK)
	var page workerControlsResponse
	decodeHubResponse(t, response, &page)
	if len(page.Controls) != 1 || page.Controls[0].MessageID != first.ID || page.Controls[0].Cursor != first.Seq || page.Cursor != first.Seq {
		t.Fatalf("controls = %#v", page)
	}
	if f.messages(t)[first.ID].Delivery != conversation.DeliverySending {
		t.Fatal("handed-off control must be sending")
	}

	t.Run("acknowledge marks sent once", func(t *testing.T) {
		before := f.load(t).EventSeq
		response := f.controls(t, first.Seq, 0)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 0 || page.Cursor != first.Seq {
			t.Fatalf("controls after ack = %#v", page)
		}
		if got := f.messages(t)[first.ID].Delivery; got != conversation.DeliverySent {
			t.Fatalf("delivery = %s, want sent", got)
		}
		if receipt := f.receipt(t, "steer-1"); receipt.Status != conversation.DeliverySent {
			t.Fatalf("receipt = %#v", receipt)
		}
		types := f.eventTypes(t, before)
		if len(types) != 2 || types[0] != "message.updated" || types[1] != "command.receipt" {
			t.Fatalf("ack events = %v", types)
		}
		after := f.load(t).EventSeq
		requireNativeStatus(t, f.controls(t, first.Seq, 0), http.StatusOK)
		if f.load(t).EventSeq != after {
			t.Fatal("a repeated acknowledgement must not emit events")
		}
	})

	t.Run("waits for a queued control", func(t *testing.T) {
		started := time.Now()
		done := make(chan workerControlsResponse, 1)
		go func() {
			response := f.controls(t, first.Seq, 10)
			var page workerControlsResponse
			_ = json.Unmarshal(response.Body.Bytes(), &page)
			done <- page
		}()
		time.Sleep(150 * time.Millisecond)
		second := f.queue(t, conversation.MessageInterrupt, "stop-1", "", nil)
		select {
		case page := <-done:
			if len(page.Controls) != 1 || page.Controls[0].MessageID != second.ID || page.Controls[0].Kind != "interrupt" {
				t.Fatalf("controls = %#v", page)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("poll did not return promptly after the control committed")
			}
		case <-time.After(8 * time.Second):
			t.Fatal("long poll did not return after a control was queued")
		}
	})

	t.Run("returns empty after the wait", func(t *testing.T) {
		messages := f.messages(t)
		var cursor int64
		for _, message := range messages {
			cursor = max(cursor, message.Seq)
		}
		started := time.Now()
		response := f.controls(t, cursor, 1)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 0 || page.Cursor != cursor {
			t.Fatalf("controls = %#v", page)
		}
		if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
			t.Fatalf("poll returned after %s without waiting", elapsed)
		}
	})

	t.Run("wrong attempt is stale", func(t *testing.T) {
		attempt := f.attempt
		f.attempt = newNativeID("attempt")
		defer func() { f.attempt = attempt }()
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
	})
}

func TestConversationWorkerTurnEvents(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	prompt := f.queue(t, conversation.MessageText, "prompt-1", "Fix the bug", nil)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	t.Run("turn started delivers the prompt and persists the thread", func(t *testing.T) {
		response := f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"})
		requireNativeStatus(t, response, http.StatusAccepted)
		var accepted struct {
			EventSeq int64 `json:"event_seq"`
		}
		decodeHubResponse(t, response, &accepted)
		record := f.load(t)
		if accepted.EventSeq != record.EventSeq || accepted.EventSeq == 0 {
			t.Fatalf("event_seq = %d, record %d", accepted.EventSeq, record.EventSeq)
		}
		if record.Execution.Status != conversation.ExecutionRunning || record.Execution.Owner.ThreadID != "thread-1" || record.Execution.Owner.TurnID != "turn-1" || record.ProviderThreadID != "thread-1" {
			t.Fatalf("execution = %#v thread %q", record.Execution, record.ProviderThreadID)
		}
		if got := f.messages(t)[prompt.ID]; got.Delivery != conversation.DeliveryDelivered || got.TurnID != "turn-1" {
			t.Fatalf("prompt = %#v", got)
		}
		if receipt := f.receipt(t, "prompt-1"); receipt.Status != conversation.DeliveryDelivered {
			t.Fatalf("receipt = %#v", receipt)
		}
	})

	t.Run("deltas assemble one assistant message per item", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t,
			map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "Hello"},
			map[string]any{"type": "delta", "provider_item_id": "item-1", "text": ", world"},
			map[string]any{"type": "item", "provider_item_id": "tool-1", "kind": "tool", "summary": "ran go test"},
			map[string]any{"type": "delta", "provider_item_id": "item-2", "text": "Second"},
		), http.StatusAccepted)
		var assistant, tool, second int
		for _, message := range f.messages(t) {
			switch {
			case message.Role == conversation.RoleAssistant && message.ProviderItemID == "item-1":
				assistant++
				if message.Text != "Hello, world" || message.Delivery != conversation.DeliveryResponding || message.AttemptID != f.attempt || message.TurnID != "turn-1" || message.Actor.Kind != conversation.ActorRunner {
					t.Fatalf("assistant = %#v", message)
				}
			case message.Role == conversation.RoleSystem && message.Kind == conversation.MessageTool:
				tool++
				if message.Text != "ran go test" || message.Delivery != conversation.DeliveryCompleted {
					t.Fatalf("tool = %#v", message)
				}
			case message.ProviderItemID == "item-2":
				second++
			}
		}
		if assistant != 1 || tool != 1 || second != 1 {
			t.Fatalf("assistant %d tool %d second %d", assistant, tool, second)
		}
	})

	var questionID string
	t.Run("question opened waits for input", func(t *testing.T) {
		before := f.load(t).EventSeq
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "choice", "header": "Pick", "question": "Which?", "options": []map[string]string{{"label": "Blue"}}, "free_text": false}}}), http.StatusAccepted)
		questions := f.questions(t)
		if len(questions) != 1 {
			t.Fatalf("questions = %#v", questions)
		}
		question := questions[0]
		questionID = question.ID
		expected := f.service.config.now().Add(time.Hour)
		if question.Status != conversation.QuestionPending || question.RequestID != "req-1" || question.Owner.AttemptID != f.attempt || question.Owner.TurnID != "turn-1" || question.ExpiresAt == nil || !question.ExpiresAt.Equal(expected) || len(question.Prompts) != 1 {
			t.Fatalf("question = %#v", question)
		}
		if status := f.messages(t)[question.MessageID]; status.Kind != conversation.MessageStatus || status.Role != conversation.RoleAssistant {
			t.Fatalf("question message = %#v", status)
		}
		if record := f.load(t); record.Execution.Status != conversation.ExecutionWaitingInput {
			t.Fatalf("execution = %#v", record.Execution)
		}
		var opened bool
		for _, kind := range f.eventTypes(t, before) {
			opened = opened || kind == "question.opened"
		}
		if !opened {
			t.Fatal("question.opened event missing")
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "choice", "question": "Which?"}}}), http.StatusAccepted)
		if len(f.questions(t)) != 1 {
			t.Fatal("a repeated question_opened must not duplicate the question")
		}
	})

	t.Run("control results update receipts", func(t *testing.T) {
		steer := f.queue(t, conversation.MessageText, "steer-ok", "More", nil)
		rejected := f.queue(t, conversation.MessageText, "steer-bad", "Bad", nil)
		lost := f.queue(t, conversation.MessageInterrupt, "stop-lost", "", nil)
		answer := f.queue(t, conversation.MessageAnswer, "answer-1", "", map[string]any{"question_id": questionID, "answers": map[string][]string{"choice": {"Blue"}}})
		response := f.controls(t, 0, 0)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 4 || page.Controls[3].Kind != "answer" || page.Controls[3].QuestionID != questionID || page.Controls[3].RequestID != "req-1" || page.Controls[3].Answers["choice"][0] != "Blue" || page.Controls[3].Expected.TurnID != "turn-1" {
			t.Fatalf("controls = %#v", page.Controls)
		}
		requireNativeStatus(t, f.controls(t, page.Cursor, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t,
			map[string]any{"type": "control_result", "key": "steer-ok", "status": "delivered"},
			map[string]any{"type": "control_result", "key": "steer-bad", "status": "rejected", "error": "turn ended"},
			map[string]any{"type": "control_result", "key": "stop-lost", "status": "unknown", "error": "connection lost"},
			map[string]any{"type": "control_result", "key": "answer-1", "status": "delivered"},
		), http.StatusAccepted)
		messages := f.messages(t)
		for _, test := range []struct {
			message conversationMessageRecord
			key     string
			want    conversation.Delivery
			code    string
		}{
			{steer, "steer-ok", conversation.DeliveryDelivered, ""},
			{rejected, "steer-bad", conversation.DeliveryRejected, "rejected"},
			{lost, "stop-lost", conversation.DeliveryUnknown, "unknown"},
			{answer, "answer-1", conversation.DeliverySent, ""},
		} {
			if got := messages[test.message.ID].Delivery; got != test.want {
				t.Fatalf("%s delivery = %s, want %s", test.key, got, test.want)
			}
			receipt := f.receipt(t, test.key)
			if receipt.Status != test.want || (test.code == "" && receipt.Error != nil) || (test.code != "" && (receipt.Error == nil || receipt.Error.Code != test.code)) {
				t.Fatalf("%s receipt = %#v", test.key, receipt)
			}
		}
		if question := f.questions(t)[0]; question.Status != conversation.QuestionAnswered {
			t.Fatalf("question = %#v", question)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "missing", "status": "delivered"}), http.StatusUnprocessableEntity)
	})

	t.Run("turn completed settles messages and questions", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "question_opened", "request_id": "req-2", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "again", "question": "Again?"}}}), http.StatusAccepted)
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "turn_completed", "turn_id": "turn-other", "status": "completed"}), http.StatusConflict, "stale_execution")
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": "completed"}), http.StatusAccepted)
		for _, message := range f.messages(t) {
			if message.Role == conversation.RoleAssistant && message.Kind == conversation.MessageText && message.Delivery != conversation.DeliveryCompleted {
				t.Fatalf("assistant message not completed: %#v", message)
			}
		}
		for _, question := range f.questions(t) {
			if question.RequestID == "req-2" && question.Status != conversation.QuestionExpired {
				t.Fatalf("question = %#v", question)
			}
			if question.RequestID == "req-1" && question.Status != conversation.QuestionAnswered {
				t.Fatalf("answered question changed: %#v", question)
			}
		}
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionRunning || record.Execution.Owner.TurnID != "" {
			t.Fatalf("execution = %#v", record.Execution)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "execution_status", "status": "bogus"}), http.StatusUnprocessableEntity)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "unknown_event"}), http.StatusUnprocessableEntity)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "execution_status", "status": "failed", "error": "provider crashed"}), http.StatusAccepted)
		if record := f.load(t); record.Execution.Status != conversation.ExecutionFailed || record.Execution.Error != "provider crashed" {
			t.Fatalf("execution = %#v", record.Execution)
		}
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-3", "text": "late"}), http.StatusConflict, "stale_execution")
	})
}

func TestConversationWorkerUnbind(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		outcome string
		want    conversation.ExecutionStatus
	}{
		{"succeeded", conversation.ExecutionCompleted},
		{"failed", conversation.ExecutionFailed},
		{"cancelled", conversation.ExecutionInterrupted},
		{"interrupted", conversation.ExecutionInterrupted},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			t.Parallel()
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
				map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "partial"},
				map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "?"}}},
			), http.StatusAccepted)
			text := f.queue(t, conversation.MessageText, "text-1", "Later", nil)
			stop := f.queue(t, conversation.MessageInterrupt, "stop-1", "", nil)
			requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
			// Accepted after the hand-off, so it was never given to the worker.
			waiting := f.queue(t, conversation.MessageText, "text-2", "And this", nil)
			requireNativeStatus(t, f.unbind(t, test.outcome), http.StatusOK)
			record := f.load(t)
			if record.Execution.Status != test.want || record.Execution.Owner.AttemptID != f.attempt {
				t.Fatalf("execution = %#v", record.Execution)
			}
			messages := f.messages(t)
			// A control the worker was handed is never replayed, whatever
			// its kind; one that was never handed out stays queued for the
			// next attempt (decisions section 10.3).
			if messages[text.ID].Delivery != conversation.DeliveryUnknown || messages[stop.ID].Delivery != conversation.DeliveryUnknown {
				t.Fatalf("text = %s stop = %s", messages[text.ID].Delivery, messages[stop.ID].Delivery)
			}
			if messages[waiting.ID].Delivery != conversation.DeliveryQueued {
				t.Fatalf("waiting = %s, want queued", messages[waiting.ID].Delivery)
			}
			if f.receipt(t, "stop-1").Status != conversation.DeliveryUnknown || f.receipt(t, "text-1").Status != conversation.DeliveryUnknown || f.receipt(t, "text-2").Status != conversation.DeliveryQueued {
				t.Fatal("receipts must follow the control deliveries")
			}
			for _, message := range messages {
				if message.ProviderItemID == "item-1" && message.Delivery != conversation.Delivery(test.want) {
					t.Fatalf("assistant = %#v", message)
				}
			}
			if question := f.questions(t)[0]; question.Status != conversation.QuestionExpired {
				t.Fatalf("question = %#v", question)
			}
			requireNativeStatus(t, f.unbind(t, test.outcome), http.StatusOK)
			requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "late"}), http.StatusConflict, "stale_execution")
			if test.outcome == "succeeded" {
				// A finishing worker may release its lease before unbinding.
				g := newConversationWorkerFixture(t)
				requireNativeStatus(t, g.bind(t, nil), http.StatusOK)
				g.release(t)
				if record := g.load(t); record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != "runner lease lost" {
					t.Fatalf("execution after release = %#v", record.Execution)
				}
				requireNativeStatus(t, g.unbind(t, "succeeded"), http.StatusOK)
				if record := g.load(t); record.Execution.Status != conversation.ExecutionCompleted || record.Execution.Error != "" {
					t.Fatalf("execution after late unbind = %#v", record.Execution)
				}
				requireNativeStatus(t, g.unbind(t, "failed"), http.StatusOK)
				if record := g.load(t); record.Execution.Status != conversation.ExecutionCompleted {
					t.Fatalf("a settled execution must not change again: %#v", record.Execution)
				}
			}
		})
	}
}

func TestConversationReconcileLeaseLoss(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "?"}}},
	), http.StatusAccepted)
	text := f.queue(t, conversation.MessageText, "text-1", "Still waiting", nil)
	stop := f.queue(t, conversation.MessageInterrupt, "stop-1", "", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	// Accepted after the hand-off, so it was never given to the worker.
	waiting := f.queue(t, conversation.MessageText, "text-2", "And this one", nil)

	t.Run("expired lease is reconciled", func(t *testing.T) {
		f.advance(11 * time.Minute)
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "late"}), http.StatusConflict, "stale_execution")
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatalf("reconcileExecutions() error = %v", err)
		}
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != "runner lease lost" {
			t.Fatalf("execution = %#v", record.Execution)
		}
		messages := f.messages(t)
		// Everything the worker was handed becomes unknown; only what was
		// never handed out stays queued (decisions section 10.3).
		if messages[text.ID].Delivery != conversation.DeliveryUnknown || messages[stop.ID].Delivery != conversation.DeliveryUnknown {
			t.Fatalf("text = %s stop = %s", messages[text.ID].Delivery, messages[stop.ID].Delivery)
		}
		if messages[waiting.ID].Delivery != conversation.DeliveryQueued {
			t.Fatalf("waiting = %s, want queued", messages[waiting.ID].Delivery)
		}
		if question := f.questions(t)[0]; question.Status != conversation.QuestionExpired {
			t.Fatalf("question = %#v", question)
		}
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatalf("second reconcileExecutions() error = %v", err)
		}
	})

	t.Run("release hook interrupts and a new attempt rebinds", func(t *testing.T) {
		f.claim(t)
		body := f.bindBody()
		body["thread_id"] = "thread-1"
		response := f.bind(t, body)
		requireNativeStatus(t, response, http.StatusOK)
		var bound workerBindResponse
		decodeHubResponse(t, response, &bound)
		if bound.Resume.ThreadID != "thread-1" || len(bound.Pending) != 1 || bound.Pending[0].MessageID != waiting.ID {
			t.Fatalf("bound = %#v", bound)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-2"}), http.StatusAccepted)
		f.release(t)
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionInterrupted {
			t.Fatalf("execution after release = %#v", record.Execution)
		}
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
	})

	t.Run("restart normalization then fresh bind", func(t *testing.T) {
		f.claim(t)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-3"}), http.StatusAccepted)
		if _, err := f.chat.store.normalizeAfterRestart(t.Context(), f.service.config.now()); err != nil {
			t.Fatal(err)
		}
		if record := f.load(t); record.Execution.Status != conversation.ExecutionUnknown {
			t.Fatalf("execution after restart = %#v", record.Execution)
		}
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-9", "text": "late"}), http.StatusConflict, "stale_execution")
		f.release(t)
		f.claim(t)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		if record := f.load(t); record.Execution.Status != conversation.ExecutionStarting || record.Execution.Owner.AttemptID != f.attempt {
			t.Fatalf("execution after rebind = %#v", record.Execution)
		}
	})
}

func TestConversationControlRouterWakeAndStop(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	subscription, cancel := f.chat.broker.subscribe(f.record.ID)
	defer cancel()
	f.chat.controls.Wake(f.record.ID)
	select {
	case <-subscription.wake:
	case <-time.After(time.Second):
		t.Fatal("Wake must notify broker subscribers")
	}
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	done := make(chan int, 1)
	go func() { done <- f.controls(t, 0, 10).Code }()
	time.Sleep(100 * time.Millisecond)
	f.chat.controls.Stop()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("poll after stop = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop must end in-flight polls")
	}
}

// TestConversationWorkerQuestionSnapshot covers what a reloading tab must
// see: a prompt that allows more than one selection keeps that flag through
// storage, and a question the current attempt already settled stays in the
// snapshot so its card renders locked instead of disappearing.
func TestConversationWorkerQuestionSnapshot(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{
		"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1",
		"prompts": []map[string]any{
			{"id": "surfaces", "header": "Surfaces", "question": "Which surfaces?", "options": []map[string]string{{"label": "Checkout lock"}, {"label": "Orphan lease"}}, "multiple": true},
			{"id": "notes", "header": "Notes", "question": "Anything else?", "free_text": true},
		},
	}), http.StatusAccepted)

	questions := f.questions(t)
	if len(questions) != 1 || len(questions[0].Prompts) != 2 {
		t.Fatalf("questions = %#v", questions)
	}
	t.Run("multiple round trips through storage", func(t *testing.T) {
		if !questions[0].Prompts[0].Multiple || questions[0].Prompts[1].Multiple {
			t.Fatalf("prompts = %#v", questions[0].Prompts)
		}
		encoded, err := json.Marshal(questions[0].Question)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Questions []map[string]any `json:"questions"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Questions[0]["multiple"] != true {
			t.Errorf("a multiple-choice prompt must report multiple: %#v", wire.Questions[0])
		}
		if _, present := wire.Questions[1]["multiple"]; present {
			t.Errorf("a single-choice prompt must omit multiple: %#v", wire.Questions[1])
		}
	})

	t.Run("a settled question stays in the snapshot", func(t *testing.T) {
		question := questions[0]
		f.queue(t, conversation.MessageAnswer, "answer-1", "", map[string]any{"question_id": question.ID, "answers": map[string][]string{"surfaces": {"Checkout lock"}, "notes": {"None"}}})
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "answer-1", "status": "delivered"}), http.StatusAccepted)
		ctx := t.Context()
		db := f.service.database.db
		pending, err := f.chat.store.listPendingQuestions(ctx, db, f.record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("pending questions = %#v, want none once answered", pending)
		}
		snapshot, err := f.chat.store.listSnapshotQuestions(ctx, db, f.record.ID, f.attempt, conversationSnapshotQuestions)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot) != 1 || snapshot[0].ID != question.ID || snapshot[0].Status != conversation.QuestionAnswered {
			t.Fatalf("snapshot questions = %#v", snapshot)
		}
		other, err := f.chat.store.listSnapshotQuestions(ctx, db, f.record.ID, "att_other", conversationSnapshotQuestions)
		if err != nil {
			t.Fatal(err)
		}
		if len(other) != 0 {
			t.Fatalf("settled questions of another attempt = %#v, want none", other)
		}
	})
}

// TestConversationBindReportsResume covers what a bind hands the runner: a
// runner that names the thread resumes it and the execution says so, while a
// runner that cannot reach the thread is handed a transcript, the execution
// says transcript, and the history records the hand-off with the number of
// messages the transcript carried (decisions section 10.4).
func TestConversationBindReportsResume(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	body := f.bindBody()
	body["thread_id"] = "thread-1"
	requireNativeStatus(t, f.bind(t, body), http.StatusOK)
	if record := f.load(t); record.Execution.Resume != conversation.ResumeThread {
		t.Fatalf("resume = %q, want thread", record.Execution.Resume)
	}
	for _, message := range f.messages(t) {
		if message.Kind == conversation.MessageStatus {
			t.Fatalf("a resumable thread must not append a transcript notice: %#v", message)
		}
	}
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "the renewal returns early"},
		map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": "completed"},
	), http.StatusAccepted)
	requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
	history := len(f.messages(t))

	// The hub forgets which runner produced the thread, which is what a
	// hub-side coordinator turn looks like to the next runner.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET provider_thread_runner_id = 'rnr_elsewhere' WHERE id = ?", f.record.ID); err != nil {
		t.Fatal(err)
	}
	f.release(t)
	f.claim(t)
	response := f.bind(t, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var bound workerBindResponse
	decodeHubResponse(t, response, &bound)
	if bound.Resume.ThreadID != "" {
		t.Fatalf("resume.thread_id = %q, want empty for a runner that cannot reach the thread", bound.Resume.ThreadID)
	}
	record := f.load(t)
	if record.Execution.Resume != conversation.ResumeTranscript {
		t.Fatalf("resume = %q, want transcript", record.Execution.Resume)
	}
	requireConversationTranscriptNotice(t, f)
	var notice string
	for _, message := range f.messages(t) {
		if message.Kind == conversation.MessageStatus && message.Role == conversation.RoleSystem {
			notice = message.Text
		}
	}
	if want := conversationTranscriptNotice(history); notice != want {
		t.Fatalf("notice = %q, want %q", notice, want)
	}
}

// TestConversationInterruptedTurnStaysInterrupted covers a turn that ended
// without a provider error: the execution records interrupted or failed
// rather than reporting running, the turn is cleared, and the unbind outcome
// still wins afterwards (decisions section 10.7).
func TestConversationInterruptedTurnStaysInterrupted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status string
		want   conversation.ExecutionStatus
	}{
		{"interrupted", conversation.ExecutionInterrupted},
		{"failed", conversation.ExecutionFailed},
		{"completed", conversation.ExecutionRunning},
	} {
		t.Run(test.status, func(t *testing.T) {
			t.Parallel()
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
				map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "partial"},
			), http.StatusAccepted)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": test.status}), http.StatusAccepted)
			record := f.load(t)
			if record.Execution.Status != test.want {
				t.Fatalf("execution = %q, want %q", record.Execution.Status, test.want)
			}
			if record.Execution.Owner.TurnID != "" {
				t.Fatalf("turn = %q, want cleared", record.Execution.Owner.TurnID)
			}
			if record.Execution.Owner.AttemptID != f.attempt {
				t.Fatalf("attempt = %q, want the bound %q", record.Execution.Owner.AttemptID, f.attempt)
			}
			if record.Execution.Settled {
				t.Fatal("a completed turn is not an unbind; the worker's outcome must still win")
			}

			// The unbind outcome wins over whatever the turn reported.
			requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
			after := f.load(t)
			if after.Execution.Status != conversation.ExecutionCompleted || !after.Execution.Settled {
				t.Fatalf("execution after unbind = %#v, want completed and settled", after.Execution)
			}
			// The turn's own output keeps the turn's verdict; the unbind
			// outcome is about the execution, not about settled messages.
			for _, message := range f.messages(t) {
				if message.ProviderItemID == "item-1" && string(message.Delivery) != test.status {
					t.Fatalf("assistant message = %q, want %q", message.Delivery, test.status)
				}
			}

			// A repeated unbind is idempotent.
			before := f.load(t)
			requireNativeStatus(t, f.unbind(t, "failed"), http.StatusOK)
			if again := f.load(t); again.EventSeq != before.EventSeq || again.Execution.Status != before.Execution.Status {
				t.Fatalf("a repeated unbind changed state: %#v -> %#v", before.Execution, again.Execution)
			}
		})
	}
}

// TestConversationTurnEventBatchIsIdempotent covers a worker that retries a
// batch after a timeout: the repeat applies nothing, answers with the event
// sequence the first attempt produced, and never appends the same delta or
// item twice.
func TestConversationTurnEventBatchIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	batch := func(key string, events ...map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body := f.identity()
		body["events"] = events
		body["batch_key"] = key
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, body)
	}
	events := []map[string]any{
		{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		{"type": "delta", "provider_item_id": "item-1", "text": "the renewal returns early"},
	}

	first := batch("batch-1", events...)
	requireNativeStatus(t, first, http.StatusAccepted)
	var applied struct {
		EventSeq int64 `json:"event_seq"`
	}
	decodeHubResponse(t, first, &applied)
	before := f.load(t)
	messages := f.messages(t)

	replay := batch("batch-1", events...)
	requireNativeStatus(t, replay, http.StatusAccepted)
	var again struct {
		EventSeq int64 `json:"event_seq"`
	}
	decodeHubResponse(t, replay, &again)
	if again.EventSeq != applied.EventSeq {
		t.Fatalf("replayed event_seq = %d, want %d", again.EventSeq, applied.EventSeq)
	}
	after := f.load(t)
	if after.EventSeq != before.EventSeq || after.Revision != before.Revision {
		t.Fatalf("a replayed batch changed state: seq %d->%d revision %d->%d", before.EventSeq, after.EventSeq, before.Revision, after.Revision)
	}
	repeated := f.messages(t)
	if len(repeated) != len(messages) {
		t.Fatalf("messages = %d, want %d (a replayed batch must not duplicate history)", len(repeated), len(messages))
	}
	for id, message := range repeated {
		if message.Text != messages[id].Text {
			t.Fatalf("message %s = %q, want %q (a replayed delta must not be appended twice)", id, message.Text, messages[id].Text)
		}
	}

	t.Run("a different key still applies", func(t *testing.T) {
		requireNativeStatus(t, batch("batch-2", map[string]any{"type": "delta", "provider_item_id": "item-1", "text": " under load"}), http.StatusAccepted)
		for _, message := range f.messages(t) {
			if message.ProviderItemID == "item-1" && message.Text != "the renewal returns early under load" {
				t.Fatalf("text = %q, want both deltas applied once", message.Text)
			}
		}
	})

	t.Run("a batch key that is too long is refused", func(t *testing.T) {
		requireConversationErrorCode(t, batch(strings.Repeat("k", 129), events...), http.StatusUnprocessableEntity, "invalid_request")
	})
}

// A turn that reported how it ended but whose worker then lost the lease
// without unbinding still settles: the controls it was handed resolve and the
// execution keeps the status the turn reported.
func TestConversationReconcileSettlesTerminalTurnAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
	handed := f.queue(t, conversation.MessageText, "text-1", "Handed over", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "execution_status", "status": "failed", "error": "provider crashed"}), http.StatusAccepted)
	if record := f.load(t); record.Execution.Status != conversation.ExecutionFailed || record.Execution.Settled {
		t.Fatalf("execution before lease loss = %#v", record.Execution)
	}

	f.advance(11 * time.Minute)
	if err := f.chat.reconcileExecutions(t.Context()); err != nil {
		t.Fatalf("reconcileExecutions() error = %v", err)
	}
	record := f.load(t)
	if record.Execution.Status != conversation.ExecutionFailed || record.Execution.Error != "provider crashed" || !record.Execution.Settled {
		t.Fatalf("execution after lease loss = %#v", record.Execution)
	}
	if delivery := f.messages(t)[handed.ID].Delivery; delivery != conversation.DeliveryUnknown {
		t.Fatalf("handed control = %s, want unknown", delivery)
	}
	seq := record.EventSeq
	if err := f.chat.reconcileExecutions(t.Context()); err != nil {
		t.Fatalf("second reconcileExecutions() error = %v", err)
	}
	if again := f.load(t); again.EventSeq != seq {
		t.Fatalf("a settled execution was reconciled again: event_seq %d -> %d", seq, again.EventSeq)
	}
}

// An answer to a question past its expires_at is refused even while the turn
// that asked it is still live.
func TestConversationAnswerRefusesExpiredQuestion(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "?", "free_text": true}}},
	), http.StatusAccepted)
	question := f.questions(t)[0]
	answer := func(key string) *httptest.ResponseRecorder {
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", f.token, conversation.Command{
			Key: key, Kind: conversation.CommandAnswer, QuestionID: question.ID,
			Answers: map[string][]string{"q": {"yes"}}, Expected: conversation.Expected{AttemptID: f.attempt},
		})
	}
	setExpiry := func(at time.Time) {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_questions SET expires_at = ? WHERE id = ?", conversationTime(at), question.ID); err != nil {
			t.Fatal(err)
		}
	}
	setExpiry(f.service.config.now().Add(-time.Second))
	requireConversationErrorCode(t, answer("late"), http.StatusConflict, "stale_execution")
	setExpiry(f.service.config.now().Add(time.Hour))
	requireNativeStatus(t, answer("in-time"), http.StatusOK)
}
