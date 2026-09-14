package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// fakeCoordinatorBackend scripts coordinator turns. Each turn records the
// request and tools it saw; run decides what the turn does.
type fakeCoordinatorBackend struct {
	mu       sync.Mutex
	requests []runner.AgentTurnRequest
	tools    [][]runner.AgentTool
	results  []runner.AgentToolResult
	run      func(ctx context.Context, turn int, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error)
	started  chan struct{}
}

func newFakeCoordinatorBackend() *fakeCoordinatorBackend {
	backend := &fakeCoordinatorBackend{started: make(chan struct{}, 16)}
	backend.run = func(_ context.Context, _ int, _ runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		for _, delta := range []string{"Hello, ", "world."} {
			if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: delta}); err != nil {
				return runner.AgentTurnResult{}, err
			}
		}
		return runner.AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1"}, nil
	}
	return backend
}

func (b *fakeCoordinatorBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return b.RunTurnWithTools(ctx, request, nil, nil, onUpdate)
}

func (b *fakeCoordinatorBackend) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	b.tools = append(b.tools, tools)
	turn := len(b.requests)
	run := b.run
	b.mu.Unlock()
	b.started <- struct{}{}
	recording := func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		result, err := handle(ctx, call)
		b.mu.Lock()
		b.results = append(b.results, result)
		b.mu.Unlock()
		return result, err
	}
	return run(ctx, turn, recording, onUpdate)
}

func (b *fakeCoordinatorBackend) turns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

func (b *fakeCoordinatorBackend) request(t *testing.T, index int) runner.AgentTurnRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if index >= len(b.requests) {
		t.Fatalf("turn %d not recorded (have %d)", index, len(b.requests))
	}
	return b.requests[index]
}

func (b *fakeCoordinatorBackend) toolResults() []runner.AgentToolResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentToolResult(nil), b.results...)
}

func (b *fakeCoordinatorBackend) setRun(run func(ctx context.Context, turn int, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.run = run
}

func (b *fakeCoordinatorBackend) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
}

// blockingRun blocks until release is closed or the context ends, then
// returns the given error (or ctx.Err when the context ended).
func blockingRun(release chan struct{}, delta string) func(ctx context.Context, turn int, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return func(ctx context.Context, _ int, _ runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		if delta != "" {
			if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: delta}); err != nil {
				return runner.AgentTurnResult{}, err
			}
		}
		select {
		case <-release:
			return runner.AgentTurnResult{ThreadID: "thread-blocked"}, nil
		case <-ctx.Done():
			return runner.AgentTurnResult{}, ctx.Err()
		}
	}
}

type coordinatorFixture struct {
	nativeFixture
	backend       *fakeCoordinatorBackend
	conversations *conversationService
	owner         string
	organization  tracker.OrganizationID
}

func newCoordinatorFixture(t *testing.T, name string) *coordinatorFixture {
	t.Helper()
	backend := newFakeCoordinatorBackend()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Conversation: &ConversationConfig{Enabled: true, Backend: backend, Workspace: t.TempDir()}})
	var organization tracker.OrganizationID
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local = 1").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "In Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "In Review", "Done"}},
		{Name: "Blocked", Transitions: []string{"Todo", "Done"}},
		{Name: "In Review", Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "project-" + name, "name": name, "states": states})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "operator-" + name, "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": organization, "project_id": project.ID})
	requireNativeStatus(t, response, http.StatusNoContent)
	if service.conversations == nil {
		t.Fatal("conversation service is not enabled")
	}
	return &coordinatorFixture{
		nativeFixture: nativeFixture{service: service, project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID), token: token.Token},
		backend:       backend,
		conversations: service.conversations,
		owner:         token.ID,
		organization:  organization,
	}
}

func (f *coordinatorFixture) coordinator() conversationCoordinator {
	return f.conversations.coordinator
}

func (f *coordinatorFixture) transact(t *testing.T, fn func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("transaction error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func (f *coordinatorFixture) seed(t *testing.T, title string, mutate func(*conversationRecord)) conversationRecord {
	t.Helper()
	now := time.Now().UTC()
	record := conversationRecord{
		ID:               conversation.NewConversationID(),
		OrganizationID:   f.organization,
		ProjectID:        f.project.ID,
		OwnerPrincipalID: f.owner,
		Title:            title,
		Visibility:       conversation.VisibilityPrivate,
		Status:           conversation.StatusActive,
		Execution:        conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if mutate != nil {
		mutate(&record)
	}
	f.transact(t, func(tx *sql.Tx) error {
		return f.conversations.store.createConversation(t.Context(), tx, &record)
	})
	return record
}

// say stores a user text message as saved, the state the API leaves it in,
// and reports the commit to the service, which wakes the coordinator.
func (f *coordinatorFixture) say(t *testing.T, record *conversationRecord, text string) conversationMessageRecord {
	t.Helper()
	message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageText, Text: text, Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}}
	f.transact(t, func(tx *sql.Tx) error {
		return f.conversations.appendMessage(t.Context(), tx, record, &message, time.Now().UTC())
	})
	f.conversations.committed(*record)
	return message
}

// history stores an already answered exchange without waking anyone.
func (f *coordinatorFixture) history(t *testing.T, record *conversationRecord, role conversation.Role, text string, delivery conversation.Delivery) {
	t.Helper()
	actor := conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}
	if role == conversation.RoleAssistant {
		actor = conversation.Actor{Kind: conversation.ActorCoordinator}
	}
	message := conversationMessageRecord{Role: role, Kind: conversation.MessageText, Text: text, Delivery: delivery, Actor: actor}
	f.transact(t, func(tx *sql.Tx) error {
		return f.conversations.appendMessage(t.Context(), tx, record, &message, time.Now().UTC())
	})
}

func (f *coordinatorFixture) conversation(t *testing.T, id string) conversationRecord {
	t.Helper()
	record, err := f.conversations.store.readConversation(t.Context(), f.service.database.db, f.organization, f.project.ID, id)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	return record
}

func (f *coordinatorFixture) messages(t *testing.T, id string) []conversationMessageRecord {
	t.Helper()
	messages, err := f.conversations.store.listMessages(t.Context(), f.service.database.db, id, 0, 500)
	if err != nil {
		t.Fatalf("listMessages() error = %v", err)
	}
	return messages
}

func (f *coordinatorFixture) events(t *testing.T, id string) []conversation.Event {
	t.Helper()
	events, err := f.conversations.store.listEvents(t.Context(), f.service.database.db, id, 0, 1000)
	if err != nil {
		t.Fatalf("listEvents() error = %v", err)
	}
	return events
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// lastReply returns the newest assistant text message.
func lastReply(messages []conversationMessageRecord) (conversationMessageRecord, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == conversation.RoleAssistant && messages[i].Kind == conversation.MessageText {
			return messages[i], true
		}
	}
	return conversationMessageRecord{}, false
}

// waitAssistant waits until the newest assistant text message, appended
// after the turn started, reaches the delivery. History seeded before the
// turn is skipped by waiting for the backend to start first.
func (f *coordinatorFixture) waitAssistant(t *testing.T, id string, delivery conversation.Delivery) conversationMessageRecord {
	t.Helper()
	var found conversationMessageRecord
	waitUntil(t, "assistant message "+string(delivery), func() bool {
		message, ok := lastReply(f.messages(t, id))
		if ok && message.Delivery == delivery {
			found = message
			return true
		}
		return false
	})
	return found
}

func TestConversationCoordinatorAvailability(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "available")
	if !f.coordinator().Available() {
		t.Fatal("Available() = false with a backend configured")
	}
	without := &conversationService{config: ConversationConfig{}, logger: discardLogger()}
	if newConversationCoordinator(without).Available() {
		t.Fatal("Available() = true without a backend")
	}
}

func TestConversationCoordinatorAnswersPendingMessages(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "answers")
	record := f.seed(t, "SECRET-TITLE", nil)
	user := f.say(t, &record, "What is blocked?")

	assistant := f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	if assistant.Text != "Hello, world." {
		t.Fatalf("assistant text = %q, want streamed text", assistant.Text)
	}
	if assistant.Actor.Kind != conversation.ActorCoordinator {
		t.Fatalf("assistant actor = %#v, want coordinator", assistant.Actor)
	}
	messages := f.messages(t, record.ID)
	if messages[0].ID != user.ID || messages[0].Delivery != conversation.DeliveryDelivered {
		t.Fatalf("user message = %#v, want delivered", messages[0])
	}
	got := f.conversation(t, record.ID)
	if got.Execution.Status != conversation.ExecutionIdle || got.Execution.Owner.ThreadID != "thread-1" || got.ProviderThreadID != "thread-1" {
		t.Fatalf("execution = %#v thread = %q, want idle with thread-1", got.Execution, got.ProviderThreadID)
	}

	var deltas []string
	var sawRunning bool
	for _, event := range f.events(t, record.ID) {
		switch event.Type {
		case conversation.EventMessageDelta:
			var body struct {
				MessageID string `json:"message_id"`
				Text      string `json:"text"`
			}
			if err := json.Unmarshal(event.Data, &body); err != nil {
				t.Fatal(err)
			}
			if body.MessageID != assistant.ID {
				t.Fatalf("delta for %q, want %q", body.MessageID, assistant.ID)
			}
			deltas = append(deltas, body.Text)
		case conversation.EventExecutionUpdated:
			if strings.Contains(string(event.Data), `"status":"running"`) {
				sawRunning = true
			}
		}
	}
	if strings.Join(deltas, "") != "Hello, world." || len(deltas) == 0 {
		t.Fatalf("deltas = %q, want ordered stream of the reply", deltas)
	}
	if !sawRunning {
		t.Fatal("expected an execution.updated event with status running")
	}

	request := f.backend.request(t, 0)
	if !request.ReadOnly || request.Resume.ThreadID != "" || request.Workspace == "" {
		t.Fatalf("request = %#v, want read-only fresh thread with workspace", request)
	}
	if !strings.Contains(request.Prompt, "What is blocked?") || strings.Contains(request.Prompt, "<transcript>") {
		t.Fatalf("prompt = %q, want the pending message without a transcript", request.Prompt)
	}
	if request.ReasoningEffort != "low" || request.TurnTimeout != 10*time.Minute || request.MaxDuration != 15*time.Minute {
		t.Fatalf("request limits = %s/%s/%s", request.ReasoningEffort, request.TurnTimeout, request.MaxDuration)
	}
	if strings.Contains(request.ToolInstructions, "SECRET-TITLE") || strings.Contains(request.Prompt, "SECRET-TITLE") {
		t.Fatal("the conversation title must never reach the model instructions")
	}
	if !strings.Contains(request.ToolInstructions, "list_attention") || !strings.Contains(request.ToolInstructions, "propose_issue") {
		t.Fatalf("instructions = %q, want tool guidance", request.ToolInstructions)
	}
	names := map[string]bool{}
	for _, tool := range f.backend.tools[0] {
		names[tool.Name] = true
	}
	for _, name := range []string{"list_attention", "explain_issue", "propose_issue"} {
		if !names[name] {
			t.Fatalf("tools = %v, missing %s", names, name)
		}
	}

	// The next message resumes the provider thread.
	f.say(t, &record, "And running?")
	waitUntil(t, "second turn", func() bool { return f.backend.turns() == 2 })
	f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	waitUntil(t, "idle", func() bool { return f.conversation(t, record.ID).Execution.Status == conversation.ExecutionIdle })
	second := f.backend.request(t, 1)
	if second.Resume.ThreadID != "thread-1" {
		t.Fatalf("second turn resume = %q, want thread-1", second.Resume.ThreadID)
	}
	if strings.Contains(second.Prompt, "What is blocked?") {
		t.Fatalf("second prompt = %q, must only contain new messages", second.Prompt)
	}
	completed := 0
	for _, message := range f.messages(t, record.ID) {
		if message.Role == conversation.RoleAssistant && message.Delivery == conversation.DeliveryCompleted {
			completed++
		}
	}
	if completed != 2 {
		t.Fatalf("completed assistant messages = %d, want 2", completed)
	}
}

func TestConversationCoordinatorPromptTranscriptOnlyWithoutThread(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		threadID string
		want     bool
	}{
		{name: "no thread includes transcript", threadID: "", want: true},
		{name: "thread carries context", threadID: "thread-existing", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newCoordinatorFixture(t, "transcript-"+strings.ReplaceAll(test.name, " ", "-"))
			record := f.seed(t, "Earlier chat", func(record *conversationRecord) { record.ProviderThreadID = test.threadID })
			f.history(t, &record, conversation.RoleUser, "earlier question", conversation.DeliveryDelivered)
			f.history(t, &record, conversation.RoleAssistant, "earlier answer", conversation.DeliveryCompleted)
			f.say(t, &record, "follow-up")
			f.backend.waitStarted(t)
			f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
			request := f.backend.request(t, 0)
			if got := strings.Contains(request.Prompt, "earlier answer"); got != test.want {
				t.Fatalf("prompt transcript present = %v, want %v: %q", got, test.want, request.Prompt)
			}
			if !strings.Contains(request.Prompt, "follow-up") {
				t.Fatalf("prompt = %q, want the pending message", request.Prompt)
			}
			if test.want && (!strings.Contains(request.Prompt, "<transcript>") || strings.Index(request.Prompt, "earlier answer") > strings.Index(request.Prompt, "follow-up")) {
				t.Fatalf("prompt = %q, want delimited transcript before the new messages", request.Prompt)
			}
			if request.Resume.ThreadID != test.threadID {
				t.Fatalf("resume = %q, want %q", request.Resume.ThreadID, test.threadID)
			}
		})
	}
}

func TestConversationCoordinatorCoalescesMessagesSavedDuringTurn(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "coalesce")
	release := make(chan struct{})
	f.backend.setRun(blockingRun(release, ""))
	record := f.seed(t, "coalesce", nil)
	f.say(t, &record, "first")
	f.backend.waitStarted(t)
	f.backend.setRun(newFakeCoordinatorBackend().run)
	f.say(t, &record, "second")
	f.say(t, &record, "third")
	messages := f.messages(t, record.ID)
	if messages[0].Delivery != conversation.DeliverySending {
		t.Fatalf("first message = %s, want sending during the turn", messages[0].Delivery)
	}
	close(release)
	waitUntil(t, "follow-up turn", func() bool { return f.backend.turns() == 2 })
	waitUntil(t, "all answered", func() bool {
		for _, message := range f.messages(t, record.ID) {
			if message.Role == conversation.RoleUser && message.Delivery != conversation.DeliveryDelivered {
				return false
			}
		}
		return f.conversation(t, record.ID).Execution.Status == conversation.ExecutionIdle
	})
	time.Sleep(50 * time.Millisecond)
	if f.backend.turns() != 2 {
		t.Fatalf("turns = %d, want exactly one follow-up turn", f.backend.turns())
	}
	second := f.backend.request(t, 1)
	if !strings.Contains(second.Prompt, "second") || !strings.Contains(second.Prompt, "third") || strings.Contains(second.Prompt, "first") {
		t.Fatalf("follow-up prompt = %q, want only the two newer messages", second.Prompt)
	}
	if strings.Index(second.Prompt, "second") > strings.Index(second.Prompt, "third") {
		t.Fatalf("follow-up prompt = %q, want newest last", second.Prompt)
	}
	if second.Resume.ThreadID != "thread-blocked" {
		t.Fatalf("follow-up resume = %q, want the thread from the first turn", second.Resume.ThreadID)
	}
}

func TestConversationCoordinatorFailurePreservesUserText(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "failure")
	f.backend.setRun(func(_ context.Context, _ int, _ runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		_ = onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "partial"})
		return runner.AgentTurnResult{}, errors.New("provider exploded")
	})
	record := f.seed(t, "failure", nil)
	f.say(t, &record, "please answer")
	assistant := f.waitAssistant(t, record.ID, conversation.DeliveryFailed)
	var data struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(assistant.Data, &data); err != nil || !strings.Contains(data.Error, "provider exploded") {
		t.Fatalf("assistant data = %s (%v), want the error", assistant.Data, err)
	}
	if assistant.Text != "partial" {
		t.Fatalf("assistant text = %q, want streamed partial text kept", assistant.Text)
	}
	messages := f.messages(t, record.ID)
	if messages[0].Delivery != conversation.DeliveryFailed || messages[0].Text != "please answer" {
		t.Fatalf("user message = %#v, want failed with text preserved", messages[0])
	}
	waitUntil(t, "execution failed", func() bool {
		execution := f.conversation(t, record.ID).Execution
		return execution.Status == conversation.ExecutionFailed && strings.Contains(execution.Error, "provider exploded")
	})
}

func TestConversationCoordinatorCancel(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "cancel")
	release := make(chan struct{})
	f.backend.setRun(blockingRun(release, "thinking"))
	record := f.seed(t, "cancel", nil)
	if f.coordinator().Cancel(record.ID) {
		t.Fatal("Cancel() = true without a running turn")
	}
	f.say(t, &record, "stop me")
	f.backend.waitStarted(t)
	waitUntil(t, "streamed delta", func() bool {
		message, ok := lastReply(f.messages(t, record.ID))
		return ok && message.Text == "thinking"
	})
	if !f.coordinator().Cancel(record.ID) {
		t.Fatal("Cancel() = false with a running turn")
	}
	assistant := f.waitAssistant(t, record.ID, conversation.DeliveryInterrupted)
	if assistant.Text != "thinking" {
		t.Fatalf("assistant text = %q, want partial text kept", assistant.Text)
	}
	messages := f.messages(t, record.ID)
	if messages[0].Delivery != conversation.DeliveryDelivered {
		t.Fatalf("user message = %s, want delivered after cancel", messages[0].Delivery)
	}
	waitUntil(t, "idle", func() bool { return f.conversation(t, record.ID).Execution.Status == conversation.ExecutionIdle })
	if f.coordinator().Cancel(record.ID) {
		t.Fatal("Cancel() = true after the turn ended")
	}
}

func TestConversationCoordinatorStopPersistsUnknown(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "stop")
	release := make(chan struct{})
	f.backend.setRun(blockingRun(release, ""))
	record := f.seed(t, "stop", nil)
	f.say(t, &record, "hang")
	f.backend.waitStarted(t)
	done := make(chan struct{})
	go func() {
		f.coordinator().Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return")
	}
	assistant, ok := lastReply(f.messages(t, record.ID))
	if !ok || assistant.Delivery != conversation.DeliveryUnknown {
		t.Fatalf("assistant = %#v, want unknown after Stop", assistant)
	}
	if messages := f.messages(t, record.ID); messages[0].Delivery != conversation.DeliveryUnknown {
		t.Fatalf("user message = %s, want unknown after Stop", messages[0].Delivery)
	}
	if got := f.conversation(t, record.ID).Execution.Status; got != conversation.ExecutionUnknown {
		t.Fatalf("execution = %s, want unknown after Stop", got)
	}
	f.coordinator().Wake(record.ID)
	f.coordinator().Stop()
}

// TestConversationCoordinatorIgnoresLinked proves the hub-side coordinator
// leaves a linked conversation to its runner. Settled is deliberately not a
// second case: a settled conversation with a pending message is woken by the
// message itself, so the coordinator still owes it a turn (section 14).
func TestConversationCoordinatorIgnoresLinked(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "ignored")
	issue := f.create(t, "linked")
	linkedAt := time.Now().UTC()
	linked := f.seed(t, "linked", func(record *conversationRecord) {
		record.WorkItemID = string(issue.WorkItemID)
		record.LinkedAt = &linkedAt
		record.Visibility = conversation.VisibilityShared
	})
	message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "hello", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}}
	f.transact(t, func(tx *sql.Tx) error {
		return f.conversations.appendMessage(t.Context(), tx, &linked, &message, time.Now().UTC())
	})
	f.coordinator().Wake(linked.ID)
	time.Sleep(150 * time.Millisecond)
	if f.backend.turns() != 0 {
		t.Fatalf("turns = %d, want none for a linked conversation", f.backend.turns())
	}
	if messages := f.messages(t, linked.ID); messages[0].Delivery != conversation.DeliverySaved {
		t.Fatalf("message in %s = %s, want untouched", linked.Title, messages[0].Delivery)
	}
}

// callTool makes the fake backend call one tool and finish.
func callTool(name, arguments string) func(ctx context.Context, turn int, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return func(ctx context.Context, _ int, handle runner.AgentToolHandler, _ runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		if _, err := handle(ctx, runner.AgentToolCall{Name: name, Arguments: json.RawMessage(arguments)}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-tools"}, nil
	}
}

func decodeToolResult(t *testing.T, result runner.AgentToolResult, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(result.Content), target); err != nil {
		t.Fatalf("decode tool result %q: %v", result.Content, err)
	}
}

type attentionItem struct {
	WorkItemID string `json:"work_item_id"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	ProjectID  string `json:"project_id"`
	Reason     string `json:"reason"`
	URL        string `json:"url"`
}

type attentionResult struct {
	Blocked         []attentionItem `json:"blocked"`
	WaitingForInput []attentionItem `json:"waiting_for_input"`
	Running         []attentionItem `json:"running"`
	Review          []attentionItem `json:"review"`
	Unavailable     []attentionItem `json:"unavailable"`
	Error           string          `json:"error"`
}

func TestConversationCoordinatorListAttention(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "attention")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	blocker := f.create(t, "blocker")
	blocked := f.create(t, "blocked")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(blocked.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "block"}, ExpectedRevision: 1, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
	review := f.create(t, "review")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(review.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "review"}, ExpectedRevision: 1, State: "In Review", Reason: "user_requested"}), http.StatusOK)
	running := f.create(t, "running")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f.nativeFixture, worker, "machine", "session", running.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(running.WorkItemID)+"/events", worker, nativeStartedEvent(lease)), http.StatusOK)
	waiting := f.create(t, "waiting")
	linkedAt := time.Now().UTC()
	linkedConversation := f.seed(t, "waiting chat", func(record *conversationRecord) {
		record.WorkItemID = string(waiting.WorkItemID)
		record.LinkedAt = &linkedAt
		record.Visibility = conversation.VisibilityShared
	})
	f.transact(t, func(tx *sql.Tx) error {
		return f.conversations.store.upsertQuestion(t.Context(), tx, conversationQuestionRecord{Question: conversation.Question{
			ID: conversation.NewQuestionID(), ConversationID: linkedConversation.ID, Status: conversation.QuestionPending,
			Prompts: []conversation.Prompt{{ID: "p1", Question: "Which branch?", FreeText: true}}, CreatedAt: linkedAt, UpdatedAt: linkedAt,
		}})
	})
	f.create(t, "plain todo")

	f.backend.setRun(callTool("list_attention", `{"scope":"project"}`))
	record := f.seed(t, "attention", nil)
	f.say(t, &record, "what needs me?")
	f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	results := f.backend.toolResults()
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("tool results = %#v, want one success", results)
	}
	var got attentionResult
	decodeToolResult(t, results[0], &got)
	find := func(items []attentionItem, id tracker.NativeWorkItemID) *attentionItem {
		for i := range items {
			if items[i].WorkItemID == string(id) {
				return &items[i]
			}
		}
		return nil
	}
	if item := find(got.Blocked, blocked.WorkItemID); item == nil || !strings.Contains(item.Reason, string(blocker.WorkItemID)) || item.URL != "/chat/issues/"+string(blocked.WorkItemID) || item.Title != "blocked" {
		t.Fatalf("blocked = %#v, want the dependency-blocked issue with reason and url", got.Blocked)
	}
	if item := find(got.Running, running.WorkItemID); item == nil || item.Reason == "" {
		t.Fatalf("running = %#v, want the leased issue", got.Running)
	}
	if item := find(got.WaitingForInput, waiting.WorkItemID); item == nil {
		t.Fatalf("waiting_for_input = %#v, want the issue with a pending question", got.WaitingForInput)
	}
	if item := find(got.Review, review.WorkItemID); item == nil || item.State != "In Review" {
		t.Fatalf("review = %#v, want the issue in review", got.Review)
	}
	for _, group := range [][]attentionItem{got.Blocked, got.Running, got.WaitingForInput, got.Review} {
		if find(group, blocker.WorkItemID) != nil {
			t.Fatalf("plain todo issue must not need attention: %#v", got)
		}
	}
	if len(got.Unavailable) != 0 {
		t.Fatalf("unavailable = %#v, want none", got.Unavailable)
	}
	if stored := f.messages(t, record.ID); len(stored) < 2 {
		t.Fatalf("messages = %d", len(stored))
	}
}

func TestConversationCoordinatorToolArgumentsAndScope(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "scope")
	own := f.create(t, "own issue")
	other := newNativeFixture(t, f.service, "", "other-project")
	foreign := other.create(t, "foreign issue")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(own.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "first comment"}), http.StatusOK)

	tests := []struct {
		name      string
		tool      string
		arguments string
		success   bool
		contains  string
	}{
		{name: "explain own issue", tool: "explain_issue", arguments: `{"work_item_id":"` + string(own.WorkItemID) + `"}`, success: true, contains: `"first comment"`},
		{name: "explain foreign issue denied", tool: "explain_issue", arguments: `{"work_item_id":"` + string(foreign.WorkItemID) + `"}`, success: false, contains: `"error"`},
		{name: "unknown field rejected", tool: "list_attention", arguments: `{"scope":"project","bogus":1}`, success: false, contains: `"error"`},
		{name: "unknown tool", tool: "delete_everything", arguments: `{}`, success: false, contains: `"error"`},
		{name: "all projects lists only granted", tool: "list_attention", arguments: `{"scope":"all_projects"}`, success: true, contains: `"blocked"`},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f.backend.setRun(callTool(test.tool, test.arguments))
			record := f.seed(t, test.name, nil)
			f.say(t, &record, "go")
			f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
			results := f.backend.toolResults()
			if len(results) != index+1 {
				t.Fatalf("tool results = %d, want %d", len(results), index+1)
			}
			result := results[index]
			if result.Success != test.success || !strings.Contains(result.Content, test.contains) {
				t.Fatalf("result = %#v, want success=%v containing %s", result, test.success, test.contains)
			}
			if test.tool == "explain_issue" && test.success {
				var explained struct {
					Title          string `json:"title"`
					State          string `json:"state"`
					ConversationID any    `json:"conversation_id"`
					Comments       []struct {
						Body string `json:"body"`
					} `json:"comments"`
				}
				decodeToolResult(t, result, &explained)
				if explained.Title != "own issue" || explained.State != "Todo" || len(explained.Comments) != 1 || explained.ConversationID != nil {
					t.Fatalf("explained = %#v", explained)
				}
			}
			if test.name == "all projects lists only granted" && strings.Contains(result.Content, string(foreign.WorkItemID)) {
				t.Fatalf("all_projects leaked a project without a grant: %s", result.Content)
			}
		})
	}
}

func TestConversationCoordinatorProposeIssue(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "propose")
	f.backend.setRun(callTool("propose_issue", `{"title":"Add retries","objective":"Retry failed uploads twice."}`))
	record := f.seed(t, "propose", nil)
	f.say(t, &record, "file it")
	f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	results := f.backend.toolResults()
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("results = %#v", results)
	}
	var proposal struct {
		Proposal struct {
			ProjectID string `json:"project_id"`
			Title     string `json:"title"`
			Objective string `json:"objective"`
		} `json:"proposal"`
	}
	decodeToolResult(t, results[0], &proposal)
	if proposal.Proposal.ProjectID != string(f.project.ID) || proposal.Proposal.Title != "Add retries" {
		t.Fatalf("proposal = %#v", proposal)
	}
	var found bool
	for _, message := range f.messages(t, record.ID) {
		if message.Role == conversation.RoleAssistant && message.Kind == conversation.MessageStatus {
			if !strings.Contains(string(message.Data), `"proposal"`) || !strings.Contains(string(message.Data), "Add retries") || message.Delivery != conversation.DeliveryCompleted {
				t.Fatalf("proposal message = %#v", message)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected an assistant status message carrying the proposal")
	}
	var issues int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id = ?", f.project.ID).Scan(&issues); err != nil {
		t.Fatal(err)
	}
	if issues != 0 {
		t.Fatalf("propose_issue created %d issues, want none", issues)
	}
	other := newNativeFixture(t, f.service, "", "propose-other")
	f.backend.setRun(callTool("propose_issue", `{"title":"Cross","objective":"x","project_id":"`+string(other.project.ID)+`"}`))
	second := f.seed(t, "propose-denied", nil)
	f.say(t, &second, "file it elsewhere")
	f.waitAssistant(t, second.ID, conversation.DeliveryCompleted)
	results = f.backend.toolResults()
	if len(results) != 2 || results[1].Success || !strings.Contains(results[1].Content, `"error"`) {
		t.Fatalf("cross-project proposal = %#v, want denied", results)
	}
}

func TestConversationCoordinatorToolMessagesFromUpdates(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "toolmessages")
	f.backend.setRun(func(_ context.Context, _ int, _ runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		for _, update := range []runner.AgentUpdate{
			{Type: runner.AgentUpdateTurnStarted, ThreadID: "thread-early"},
			{Type: runner.AgentUpdateToolStarted, ItemID: "item-1", Tool: "shell", Command: "git status " + strings.Repeat("x", 1000)},
			{Type: runner.AgentUpdateToolCompleted, ItemID: "item-1", Tool: "shell", Status: "completed"},
			{Type: runner.AgentUpdateMessageDelta, Delta: "done"},
		} {
			if err := onUpdate(update); err != nil {
				return runner.AgentTurnResult{}, err
			}
		}
		return runner.AgentTurnResult{ThreadID: "thread-early"}, nil
	})
	record := f.seed(t, "tools", nil)
	f.say(t, &record, "check")
	f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	var tools []conversationMessageRecord
	for _, message := range f.messages(t, record.ID) {
		if message.Kind == conversation.MessageTool {
			tools = append(tools, message)
		}
	}
	if len(tools) != 1 || tools[0].Role != conversation.RoleSystem || tools[0].Delivery != conversation.DeliveryCompleted || !strings.Contains(tools[0].Text, "shell") {
		t.Fatalf("tool messages = %#v, want one completed system tool message", tools)
	}
	if len([]rune(tools[0].Text)) > 500 {
		t.Fatalf("tool summary length = %d, want at most 500 runes", len([]rune(tools[0].Text)))
	}
	if got := f.conversation(t, record.ID).ProviderThreadID; got != "thread-early" {
		t.Fatalf("thread id = %q, want persisted from the turn update", got)
	}
}

// TestConversationCoordinatorExplainIssueHidesCoordinatorItems proves the
// coordinator cannot read a coordinator item through explain_issue: those
// issues carry other people's private chats and are not project work
// (decisions section 10.1).
func TestConversationCoordinatorExplainIssueHidesCoordinatorItems(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "coordinator-items")
	record := f.seed(t, "coordinator-items", nil)
	item := f.create(t, "Coordinator turn for conversation deadbeef")
	if _, err := f.service.database.db.ExecContext(t.Context(),
		`INSERT INTO coordinator_items (work_item_id, conversation_id, organization_id, project_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		string(item.WorkItemID), record.ID, string(f.organization), string(f.project.ID), testTimestamp); err != nil {
		t.Fatalf("seed coordinator item: %v", err)
	}

	f.backend.setRun(callTool("explain_issue", `{"work_item_id":"`+string(item.WorkItemID)+`"}`))
	f.say(t, &record, "explain that one")
	f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	results := f.backend.toolResults()
	if len(results) != 1 {
		t.Fatalf("tool results = %#v, want one", results)
	}
	if results[0].Success {
		t.Fatalf("explain_issue on a coordinator item succeeded: %s", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "is not readable in this conversation") {
		t.Fatalf("result = %s, want the opaque not-readable error", results[0].Content)
	}
}

// TestConversationReconcileLeavesHubCoordinatorTurns proves the reconcile
// loop does not interrupt a turn running in this process. A hub-side
// coordinator turn holds no lease and no attempt, and reconciliation is about
// workers that stopped reporting, so a chat mid-answer must survive it.
func TestConversationReconcileLeavesHubCoordinatorTurns(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "reconcile")
	record := f.seed(t, "reconcile", nil)
	if _, err := f.service.database.db.ExecContext(t.Context(),
		`UPDATE conversations SET execution_json = json_set(execution_json, '$.status', 'running') WHERE id = ?`, record.ID); err != nil {
		t.Fatal(err)
	}
	before, err := f.conversations.store.readConversation(t.Context(), f.service.database.db, record.OrganizationID, record.ProjectID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Execution.Status != conversation.ExecutionRunning {
		t.Fatalf("seeded execution = %#v, want running", before.Execution)
	}

	if err := f.conversations.reconcileExecutions(t.Context()); err != nil {
		t.Fatalf("reconcileExecutions() error = %v", err)
	}

	after, err := f.conversations.store.readConversation(t.Context(), f.service.database.db, record.OrganizationID, record.ProjectID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Execution.Status != conversation.ExecutionRunning || after.Execution.Error != "" {
		t.Fatalf("execution = %#v, want the in-process turn left running", after.Execution)
	}
	if after.EventSeq != before.EventSeq || after.Revision != before.Revision {
		t.Fatalf("reconcile changed state: seq %d->%d revision %d->%d", before.EventSeq, after.EventSeq, before.Revision, after.Revision)
	}
}

// TestConversationRetryOnTheHubCoordinatorPath covers the other half of the
// retry command: with a hub-side coordinator and no linked issue the message
// goes back to `saved`, which is the delivery that coordinator reads, and the
// receipt for the retry key says so (decisions section 10.3).
func TestConversationRetryOnTheHubCoordinatorPath(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "retry")
	record := f.seed(t, "retry", nil)
	f.history(t, &record, conversation.RoleUser, "Did the last one land?", conversation.DeliveryUnknown)
	stranded := f.messages(t, record.ID)[0]
	if stranded.Delivery != conversation.DeliveryUnknown {
		t.Fatalf("seeded message = %#v, want unknown", stranded)
	}

	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/commands", f.token,
		conversation.Command{Key: "retry-saved", Kind: conversation.CommandRetry, MessageID: stranded.ID})
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliverySaved || receipt.MessageID != stranded.ID {
		t.Fatalf("receipt = %#v, want saved for %s", receipt, stranded.ID)
	}
	if got := f.messages(t, record.ID)[0].Delivery; got != conversation.DeliverySaved {
		t.Fatalf("delivery = %q, want saved so the coordinator picks it up", got)
	}
	if items := coordinatorIssues(t, f.service, string(f.project.ID)); len(items) != 0 {
		t.Fatalf("coordinator items = %#v, want none: the hub answers this turn itself", items)
	}
}

// TestConversationCoordinatorHonoursPreferences proves the transitional
// hub-side coordinator applies the conversation's explicit turn preferences
// to its own turn, and that read-only stays true whatever access says: a
// coordinator turn never changes anything (decisions section 14).
func TestConversationCoordinatorHonoursPreferences(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "preferences")
	f.conversations.config.Model = "configured-model"
	f.conversations.config.ReasoningEffort = "medium"

	auto := f.seed(t, "auto", nil)
	f.say(t, &auto, "Anything blocked?")
	f.waitAssistant(t, auto.ID, conversation.DeliveryCompleted)
	if request := f.backend.request(t, 0); request.Model != "configured-model" || request.ReasoningEffort != "medium" || !request.ReadOnly {
		t.Fatalf("auto turn = model %q effort %q read-only %t", request.Model, request.ReasoningEffort, request.ReadOnly)
	}

	explicit := f.seed(t, "explicit", func(record *conversationRecord) {
		record.Preferences = conversation.Preferences{Model: "chosen-model", ReasoningEffort: conversation.EffortHigh, Access: conversation.AccessFull}
	})
	f.say(t, &explicit, "And now?")
	f.waitAssistant(t, explicit.ID, conversation.DeliveryCompleted)
	waitUntil(t, "second turn", func() bool { return f.backend.turns() == 2 })
	if request := f.backend.request(t, 1); request.Model != "chosen-model" || request.ReasoningEffort != conversation.EffortHigh || !request.ReadOnly {
		t.Fatalf("explicit turn = model %q effort %q read-only %t", request.Model, request.ReasoningEffort, request.ReadOnly)
	}
	f.coordinator().Stop()
}
