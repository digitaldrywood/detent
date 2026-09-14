package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationHub scripts the four worker endpoints of one conversation.
type conversationHub struct {
	t             *testing.T
	mu            sync.Mutex
	bindStatus    int
	bindCode      string
	bindPending   []ConversationControl
	bindThread    string
	bindBody      string
	queue         []ConversationControl
	served        int
	afterSeen     []int64
	binds         []ConversationBindRequest
	batches       [][]runner.ConversationTurnEvent
	batchKeys     []string
	reportFailure int
	reportCalls   int
	hangReports   bool
	hangEvents    chan struct{}
	rejectKey     string
	unbinds       []ConversationUnbindRequest
	staleCalls    int
	controlsDeny  int
	denyCalls     int
	controlsWake  chan struct{}
}

func newConversationHub(t *testing.T) (*conversationHub, *nativeExecution) {
	t.Helper()
	hub := &conversationHub{t: t, bindStatus: http.StatusOK, controlsWake: make(chan struct{}, 16), hangEvents: make(chan struct{})}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	// Released before the server is closed so a hung handler cannot block it.
	t.Cleanup(func() { close(hub.hangEvents) })
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	source := &NativeConnector{client: native}
	id := "wi_" + strings.Repeat("c", 32)
	lease := tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), ID: "lease-1", FencingToken: 7, PolicyID: "policy"}
	scheduler.nativeProjects["project"] = source
	scheduler.nativeClaims[id] = nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Hour)}
	execution := &nativeExecution{scheduler: scheduler, claim: nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Hour)}, data: tracker.NativeRunData{
		RunID: "run_1", AttemptID: "attempt_1", LeaseID: lease.ID, FencingToken: lease.FencingToken,
	}}
	return hub, execution
}

func (h *conversationHub) push(controls ...ConversationControl) {
	h.mu.Lock()
	h.queue = append(h.queue, controls...)
	h.mu.Unlock()
	select {
	case h.controlsWake <- struct{}{}:
	default:
	}
}

// denyControls makes every controls poll fail with an authorization error the
// runner cannot recover from.
func (h *conversationHub) denyControls(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.controlsDeny = status
}

// denyCount is how often the controls poll was refused.
func (h *conversationHub) denyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.denyCalls
}

func (h *conversationHub) expireLease() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bindCode = "stale_execution"
}

func (h *conversationHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test" {
		h.t.Errorf("missing bearer token on %s", r.URL.Path)
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/organizations/org_test/projects/prj_test")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/conversation/bind"):
		h.serveBind(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/conversation/unbind"):
		h.serveUnbind(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/controls"):
		h.serveControls(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/turn-events"):
		h.serveTurnEvents(w, r)
	default:
		h.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"code":"not_found","message":"unknown route"}`, http.StatusNotFound)
	}
}

func (h *conversationHub) serveBind(w http.ResponseWriter, r *http.Request) {
	var request ConversationBindRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		h.t.Errorf("decode bind: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.binds = append(h.binds, request)
	if h.bindStatus != http.StatusOK {
		w.WriteHeader(h.bindStatus)
		_, _ = w.Write([]byte(`{"code":"` + h.bindCode + `","message":"scripted"}`))
		return
	}
	if h.bindBody != "" {
		_, _ = w.Write([]byte(h.bindBody))
		return
	}
	response := ConversationBindResponse{ConversationID: "conv_1", Pending: h.bindPending}
	response.Resume.ThreadID = h.bindThread
	if len(h.bindPending) > 0 {
		response.Cursor = h.bindPending[len(h.bindPending)-1].Cursor
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (h *conversationHub) serveUnbind(w http.ResponseWriter, r *http.Request) {
	var request ConversationUnbindRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		h.t.Errorf("decode unbind: %v", err)
	}
	h.mu.Lock()
	h.unbinds = append(h.unbinds, request)
	h.mu.Unlock()
	_, _ = w.Write([]byte(`{"conversation_id":"conv_1"}`))
}

func (h *conversationHub) serveControls(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Detent-Lease") != "lease-1" || r.Header.Get("X-Detent-Fencing-Token") != "7" || r.Header.Get("X-Detent-Attempt") != "attempt_1" {
		h.t.Errorf("controls poll lacks the owner headers: %v", r.Header)
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait <= 0 || wait > 30 {
		h.t.Errorf("controls poll wait = %d", wait)
	}
	deadline := time.NewTimer(min(time.Duration(wait)*time.Second, 2*time.Second))
	defer deadline.Stop()
	for {
		h.mu.Lock()
		if h.controlsDeny != 0 {
			status := h.controlsDeny
			h.denyCalls++
			h.mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"forbidden","message":"scripted"}`))
			return
		}
		if h.bindCode == "stale_execution" {
			h.staleCalls++
			h.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"stale_execution","message":"scripted"}`))
			return
		}
		h.afterSeen = append(h.afterSeen, after)
		page := ConversationControlsPage{Controls: []ConversationControl{}, Cursor: after}
		for h.served < len(h.queue) {
			control := h.queue[h.served]
			if control.Cursor <= after {
				h.served++
				continue
			}
			page.Controls = append(page.Controls, control)
			page.Cursor = control.Cursor
			h.served++
		}
		if len(page.Controls) > 0 {
			// Re-serve until acknowledged, like the hub's sending state.
			h.served -= len(page.Controls)
		}
		h.mu.Unlock()
		if len(page.Controls) > 0 {
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		select {
		case <-h.controlsWake:
			continue
		case <-deadline.C:
		case <-r.Context().Done():
		}
		_ = json.NewEncoder(w).Encode(page)
		return
	}
}

func (h *conversationHub) serveTurnEvents(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ConversationTurnEventsRequest
		Events []runner.ConversationTurnEvent `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		h.t.Errorf("decode turn events: %v", err)
	}
	h.mu.Lock()
	h.batchKeys = append(h.batchKeys, request.BatchKey)
	h.mu.Unlock()
	if request.LeaseID != "lease-1" || request.FencingToken != 7 || request.AttemptID != "attempt_1" {
		h.t.Errorf("turn events lack the owner identity: %#v", request.ConversationTurnEventsRequest)
	}
	h.mu.Lock()
	h.reportCalls++
	hang, reject := h.hangReports, h.rejectKey
	failing := h.reportFailure > 0
	if failing {
		h.reportFailure--
	}
	h.mu.Unlock()
	if hang {
		// A hub that accepts the request and never answers it.
		select {
		case <-h.hangEvents:
		case <-r.Context().Done():
		}
		return
	}
	if failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"unavailable","message":"scripted"}`))
		return
	}
	for _, event := range request.Events {
		if reject != "" && event.Key == reject {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_event","message":"scripted rejection"}`))
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.batches = append(h.batches, request.Events)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"event_seq":` + strconv.Itoa(len(h.batches)) + `}`))
}

func (h *conversationHub) events() []runner.ConversationTurnEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	all := []runner.ConversationTurnEvent{}
	for _, batch := range h.batches {
		all = append(all, batch...)
	}
	return all
}

func (h *conversationHub) waitFor(t *testing.T, what string, condition func() bool) {
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

func (h *conversationHub) lastAfter() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.afterSeen) == 0 {
		return -1
	}
	return h.afterSeen[len(h.afterSeen)-1]
}

// postedBatchKeys is the idempotency key of every turn-events request the hub
// received, retries included.
func (h *conversationHub) postedBatchKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.batchKeys...)
}

func (h *conversationHub) reportCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reportCalls
}

func (h *conversationHub) batchCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.batches)
}

func (h *conversationHub) pollCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.afterSeen)
}

// staleCount is how often the controls poll was rejected as stale. A worker
// that gives up rejects once; one that spins keeps rejecting.
func (h *conversationHub) staleCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.staleCalls
}

func (h *conversationHub) controlResult(key string) (runner.ConversationTurnEvent, bool) {
	for _, event := range h.events() {
		if event.Type == runner.ConversationEventControlResult && event.Key == key {
			return event, true
		}
	}
	return runner.ConversationTurnEvent{}, false
}

// awaitPollStopped waits for the control poll loop to exit, which is what a
// refusal the runner cannot recover from must cause. Waiting on the loop
// rather than sleeping keeps the proof exact: a loop that kept retrying never
// closes the channel.
func awaitPollStopped(t *testing.T, session *conversationSession) {
	t.Helper()
	select {
	case <-session.pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the control poll did not stop")
	}
}

func useFastConversationTimings(t *testing.T) {
	t.Helper()
	flush, backoff, wait := conversationFlushInterval, conversationReportBackoff, conversationPollWait
	conversationFlushInterval, conversationReportBackoff, conversationPollWait = 20*time.Millisecond, 10*time.Millisecond, time.Second
	t.Cleanup(func() {
		conversationFlushInterval, conversationReportBackoff, conversationPollWait = flush, backoff, wait
	})
}

func TestNativeBindConversationMapsErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{"missing conversation", http.StatusNotFound, "not_found", runner.ErrNoConversation},
		{"stale execution", http.StatusConflict, "stale_execution", ErrStaleConversation},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, execution := newConversationHub(t)
			hub.bindStatus, hub.bindCode = test.status, test.code
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
			if !errors.Is(err, test.want) {
				t.Fatalf("bind error = %v, want %v", err, test.want)
			}
			if session != nil {
				t.Fatal("failed bind returned a session")
			}
			if len(hub.binds) != 1 || hub.binds[0].LeaseID != "lease-1" || hub.binds[0].FencingToken != 7 || hub.binds[0].AttemptID != "attempt_1" || hub.binds[0].RunID != "run_1" || !hub.binds[0].Capabilities.Steer {
				t.Fatalf("bind request = %#v", hub.binds)
			}
		})
	}
}

func TestNativeConversationSessionDeliversControlsInOrder(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.bindThread = "thread-0"
	hub.bindPending = []ConversationControl{
		{Cursor: 1, Key: "k1", Kind: "message", MessageID: "msg_1", Text: "first"},
		{Cursor: 2, Key: "k2", Kind: "message", MessageID: "msg_2", Text: "second"},
	}
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true, Interrupt: true, Answer: true})
	if err != nil {
		t.Fatal(err)
	}
	if session.ConversationID() != "conv_1" || session.ResumeThreadID() != "thread-0" {
		t.Fatalf("session identity = %q %q", session.ConversationID(), session.ResumeThreadID())
	}
	text, keys := session.PendingPrompt()
	if !strings.Contains(text, "first") || !strings.Contains(text, "second") || strings.Join(keys, ",") != "k1,k2" {
		t.Fatalf("pending prompt = %q keys %v", text, keys)
	}
	control := session.Control(runner.ConversationTurnHooks{})
	if control == nil || control.Commands == nil {
		t.Fatal("turn control is not wired")
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}}); err != nil {
		t.Fatal(err)
	}
	hub.push(
		ConversationControl{Cursor: 3, Key: "k3", Kind: "message", MessageID: "msg_3", Text: "steer"},
		ConversationControl{Cursor: 4, Key: "k4", Kind: "interrupt", MessageID: "msg_4"},
		ConversationControl{Cursor: 5, Key: "k5", Kind: "answer", MessageID: "msg_5", RequestID: "req-1", Answers: map[string][]string{"q": {"yes"}}},
	)
	var received []runner.AgentControl
	for len(received) < 3 {
		select {
		case command := <-control.Commands:
			received = append(received, command)
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d controls before timing out", len(received))
		}
	}
	for i, want := range []struct {
		kind    runner.AgentControlKind
		message string
	}{{runner.AgentControlMessage, "msg_3"}, {runner.AgentControlInterrupt, "msg_4"}, {runner.AgentControlAnswer, "msg_5"}} {
		got := received[i]
		if got.Kind != want.kind || got.MessageID != want.message || got.ThreadID != "thread-1" || got.TurnID != "turn-1" {
			t.Fatalf("control %d = %#v", i, got)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("control %d invalid: %v", i, err)
		}
		if err := got.Check(t.Context()); err != nil {
			t.Fatalf("control %d check = %v", i, err)
		}
	}
	if received[2].RequestID != "req-1" || len(received[2].Answers["q"]) != 1 || received[2].Answers["q"][0] != "yes" || received[0].Text != "steer" {
		t.Fatalf("control payloads lost: %#v", received)
	}
	received[0].Reply <- nil
	received[1].Reply <- runner.ErrStaleConversationControl
	hub.waitFor(t, "control results", func() bool { return hub.lastAfter() == 5 })
	hub.mu.Lock()
	afterSeen := append([]int64(nil), hub.afterSeen...)
	hub.mu.Unlock()
	if afterSeen[0] != 2 {
		t.Fatalf("first poll acknowledged cursor %d, want the bind cursor 2", afterSeen[0])
	}
	for i := 1; i < len(afterSeen); i++ {
		if afterSeen[i] < afterSeen[i-1] {
			t.Fatalf("acknowledged cursor moved backwards: %v", afterSeen)
		}
	}
	session.FinishTurn()
	hub.waitFor(t, "unknown result for the unconsumed answer", func() bool {
		_, ok := hub.controlResult("k5")
		return ok
	})
	if err := session.Close(t.Context(), runner.ConversationOutcomeSucceeded, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		key, status string
		errorNeedle string
	}{{"k3", runner.ConversationControlDelivered, ""}, {"k4", runner.ConversationControlRejected, "inactive turn"}, {"k5", runner.ConversationControlUnknown, "turn ended"}} {
		event, ok := hub.controlResult(want.key)
		if !ok {
			t.Fatalf("no control_result for %s in %#v", want.key, hub.events())
		}
		if event.Status != want.status || !strings.Contains(event.Error, want.errorNeedle) {
			t.Fatalf("control_result %s = %#v", want.key, event)
		}
	}
	if len(hub.unbinds) != 1 || hub.unbinds[0].Outcome != "succeeded" || hub.unbinds[0].AttemptID != "attempt_1" || hub.unbinds[0].LeaseID != "lease-1" {
		t.Fatalf("unbind = %#v", hub.unbinds)
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: "completed"}}); err == nil {
		t.Fatal("report after close succeeded")
	}
}

func TestNativeConversationControlCheckRejectsLostLease(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatal(err)
	}
	control := session.Control(runner.ConversationTurnHooks{})
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}}); err != nil {
		t.Fatal(err)
	}
	hub.push(
		ConversationControl{Cursor: 1, Key: "other", Kind: "message", MessageID: "msg_1", Text: "x", Expected: ConversationExpected{AttemptID: "attempt_2"}},
		ConversationControl{Cursor: 2, Key: "mine", Kind: "message", MessageID: "msg_2", Text: "y", Expected: ConversationExpected{AttemptID: "attempt_1", TurnID: "turn-1"}},
	)
	other := <-control.Commands
	mine := <-control.Commands
	if err := other.Check(t.Context()); !errors.Is(err, runner.ErrStaleConversationControl) {
		t.Fatalf("foreign attempt check = %v", err)
	}
	if err := mine.Check(t.Context()); err != nil {
		t.Fatalf("own attempt check = %v", err)
	}
	execution.scheduler.mu.Lock()
	claim := execution.scheduler.nativeClaims[string(execution.claim.lease.WorkItemID)]
	claim.deadline = time.Now().Add(-time.Minute)
	execution.scheduler.nativeClaims[string(execution.claim.lease.WorkItemID)] = claim
	execution.scheduler.mu.Unlock()
	if err := mine.Check(t.Context()); !errors.Is(err, runner.ErrStaleConversationControl) {
		t.Fatalf("expired lease check = %v", err)
	}
	other.Reply <- runner.ErrStaleConversationControl
	mine.Reply <- errors.New("provider closed before acknowledgement")
	if err := session.Close(t.Context(), runner.ConversationOutcomeFailed, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if event, ok := hub.controlResult("other"); !ok || event.Status != runner.ConversationControlRejected {
		t.Fatalf("foreign control result = %#v", event)
	}
	if event, ok := hub.controlResult("mine"); !ok || event.Status != runner.ConversationControlUnknown || !strings.Contains(event.Error, "provider closed") {
		t.Fatalf("failed control result = %#v", event)
	}
	if len(hub.unbinds) != 1 || hub.unbinds[0].Outcome != "failed" || hub.unbinds[0].Error != "boom" {
		t.Fatalf("unbind = %#v", hub.unbinds)
	}
}

func TestNativeConversationReportBatchesAndRetries(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.reportFailure = 1
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := session.Report(ctx, []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "t", TurnID: "turn-1"}}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Hel", "lo", " world"} {
		if err := session.Report(ctx, []runner.ConversationTurnEvent{{Type: runner.ConversationEventDelta, ProviderItemID: "item-1", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Report(ctx, []runner.ConversationTurnEvent{
		{Type: runner.ConversationEventItem, ProviderItemID: "tool-1", Kind: runner.ConversationItemTool, Summary: "ran go test"},
		{Type: runner.ConversationEventDelta, ProviderItemID: "item-1", Text: "!"},
		{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: runner.ConversationTurnCompleted},
	}); err != nil {
		t.Fatal(err)
	}
	hub.waitFor(t, "batched report", func() bool { return hub.batchCount() == 1 })
	events := hub.events()
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	if strings.Join(types, ",") != "turn_started,delta,item,delta,turn_completed" {
		t.Fatalf("event order = %v", types)
	}
	if len(events) < 4 || events[1].Text != "Hello world" || events[3].Text != "!" {
		t.Fatalf("deltas were not coalesced within the batch: %#v", events)
	}
	hub.mu.Lock()
	calls := hub.reportCalls
	hub.mu.Unlock()
	if calls != 2 {
		t.Fatalf("report calls = %d, want one failure and one success", calls)
	}
	if err := session.Close(ctx, runner.ConversationOutcomeInterrupted, nil); err != nil {
		t.Fatal(err)
	}
	if len(hub.unbinds) != 1 || hub.unbinds[0].Outcome != "interrupted" {
		t.Fatalf("unbind = %#v", hub.unbinds)
	}
}

func TestNativeConversationCloseReportsUnconsumedControls(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatal(err)
	}
	control := session.Control(runner.ConversationTurnHooks{QuestionTimeout: time.Minute})
	if control.EffectiveQuestionTimeout() != time.Minute {
		t.Fatalf("question timeout = %v", control.EffectiveQuestionTimeout())
	}
	hub.push(ConversationControl{Cursor: 1, Key: "late", Kind: "message", MessageID: "msg_1", Text: "too late"})
	hub.waitFor(t, "control acknowledged", func() bool { return hub.lastAfter() == 1 })
	if err := session.Close(t.Context(), runner.ConversationOutcomeCancelled, context.Canceled); err != nil {
		t.Fatal(err)
	}
	event, ok := hub.controlResult("late")
	if !ok || event.Status != runner.ConversationControlUnknown || !strings.Contains(event.Error, "execution ended") {
		t.Fatalf("unconsumed control result = %#v", event)
	}
	if len(hub.unbinds) != 1 || hub.unbinds[0].Outcome != "cancelled" {
		t.Fatalf("unbind = %#v", hub.unbinds)
	}
	select {
	case _, open := <-control.Commands:
		if open {
			t.Fatal("closed session still queues controls")
		}
	default:
		t.Fatal("closed session left the command channel open")
	}
}

func TestNativeConversationPollStopsOnStaleExecution(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	bound, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := bound.(*conversationSession)
	if !ok {
		t.Fatalf("bound session = %T, want the native session", bound)
	}
	control := session.Control(runner.ConversationTurnHooks{})
	hub.waitFor(t, "the first controls poll", func() bool { return hub.pollCount() > 0 })

	// The hub revokes this attempt's ownership. Polling must stop rather than
	// spin on the rejection, and a control queued afterwards is never handed
	// to the turn.
	hub.expireLease()
	hub.push(ConversationControl{Cursor: 1, Key: "revoked", Kind: "message", MessageID: "msg_1", Text: "ignored"})
	awaitPollStopped(t, session)
	if rejections := hub.staleCount(); rejections != 1 {
		t.Fatalf("polling continued after stale_execution: %d rejections, want 1", rejections)
	}
	select {
	case command := <-control.Commands:
		t.Fatalf("a control was delivered after the execution went stale: %#v", command)
	default:
	}

	// Unbinding still reports the run outcome even though polling gave up.
	if err := session.Close(t.Context(), runner.ConversationOutcomeFailed, nil); err != nil {
		t.Fatal(err)
	}
	if len(hub.unbinds) != 1 || hub.unbinds[0].Outcome != "failed" {
		t.Fatalf("unbind = %#v", hub.unbinds)
	}
}

// coordinatorBindBody is a bind response for a coordinator work item whose
// provider thread was produced by another runner: no resume thread, a bounded
// transcript instead.
const coordinatorBindBody = `{"conversation_id":"conv_1","coordinator":true,"resume":{"thread_id":"","transcript":[` +
	`{"role":"user","kind":"text","text":"How is the release going?"},` +
	`{"role":"assistant","kind":"text","text":"Two issues are in review."}` +
	`]},"pending":[{"cursor":4,"key":"k1","kind":"message","message_id":"msg_1","text":"Anything blocked?"}],"cursor":4}`

func TestNativeConversationBindDecodesCoordinatorAndTranscript(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.bindBody = coordinatorBindBody
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	defer func() {
		if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
			t.Errorf("close error = %v", err)
		}
	}()
	if !session.Coordinator() {
		t.Fatal("session did not decode coordinator: true")
	}
	if session.ResumeThreadID() != "" {
		t.Fatalf("resume thread = %q, want empty when the hub sends a transcript", session.ResumeThreadID())
	}
	if session.ResumeMode() != runner.ConversationResumeTranscript {
		t.Fatalf("resume mode = %q, want transcript", session.ResumeMode())
	}
	prompt, keys := session.PendingPrompt()
	transcript := strings.Index(prompt, "<transcript>")
	follow := strings.Index(prompt, "<pending-follow-ups>")
	if transcript != 0 && !strings.HasPrefix(prompt, "Prior messages") {
		t.Fatalf("pending prompt does not lead with the transcript block: %q", prompt)
	}
	if transcript < 0 || follow < transcript || !strings.Contains(prompt, "[assistant] Two issues are in review.") {
		t.Fatalf("pending prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "</transcript>") || !strings.Contains(prompt, "</pending-follow-ups>") {
		t.Fatalf("data blocks are not closed: %q", prompt)
	}
	if !strings.Contains(prompt, "Anything blocked?") {
		t.Fatalf("pending prompt lost the follow-up: %q", prompt)
	}
	if strings.Join(keys, ",") != "k1" {
		t.Fatalf("pending keys = %v, want [k1]", keys)
	}
}

func TestNativeConversationBindKeepsResumeThreadOverTranscript(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.bindBody = `{"conversation_id":"conv_1","resume":{"thread_id":"thread-9","transcript":[{"role":"user","kind":"text","text":"earlier"}]},"pending":[],"cursor":0}`
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	defer func() {
		if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
			t.Errorf("close error = %v", err)
		}
	}()
	if session.ResumeThreadID() != "thread-9" || session.Coordinator() {
		t.Fatalf("resume thread = %q coordinator = %v", session.ResumeThreadID(), session.Coordinator())
	}
	if prompt, _ := session.PendingPrompt(); prompt != "" {
		t.Fatalf("pending prompt = %q, want empty: the provider thread already carries the history", prompt)
	}
}

func TestNativeConversationPostStatusPostsItemWithData(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-c", TurnID: "turn-c"}}); err != nil {
		t.Fatalf("report turn_started: %v", err)
	}
	proposal := map[string]any{"project_id": "prj_1", "title": "Ship it"}
	if err := session.PostStatus(t.Context(), map[string]any{"proposal": proposal}, "Proposed issue: Ship it"); err != nil {
		t.Fatalf("post status: %v", err)
	}
	if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
		t.Fatalf("close error = %v", err)
	}
	var status runner.ConversationTurnEvent
	for _, event := range hub.events() {
		if event.Type == runner.ConversationEventItem {
			status = event
		}
	}
	if status.Kind != runner.ConversationItemStatus || status.ThreadID != "thread-c" || status.TurnID != "turn-c" {
		t.Fatalf("status item = %#v", status)
	}
	if status.Summary != "Proposed issue: Ship it" {
		t.Fatalf("status summary = %q", status.Summary)
	}
	decoded, _ := status.Data["proposal"].(map[string]any)
	if decoded["title"] != "Ship it" || decoded["project_id"] != "prj_1" {
		t.Fatalf("status data = %#v", status.Data)
	}
}

func TestSchedulerCoordinatorReader(t *testing.T) {
	_, execution := newConversationHub(t)
	scheduler := execution.scheduler
	item := string(execution.claim.lease.WorkItemID)
	reader := scheduler.CoordinatorReader(item)
	if reader == nil {
		t.Fatal("claimed work item has no coordinator reader")
	}
	if reader != runner.CoordinatorHubReader(execution.claim.source.client) {
		t.Fatalf("coordinator reader = %#v, want the claim's native client", reader)
	}
	if got := scheduler.CoordinatorReader("wi_unknown"); got != nil {
		t.Fatalf("unclaimed work item returned a reader: %#v", got)
	}
	// The tools report the hub's own project id, so a proposal can name the
	// project an issue read reported.
	if got := scheduler.CoordinatorProject(item); got != "prj_test" {
		t.Fatalf("coordinator project = %q, want the claim's hub project", got)
	}
	if got := scheduler.CoordinatorProject("wi_unknown"); got != "" {
		t.Fatalf("unclaimed work item returned project %q", got)
	}
}

// The session tells the runner how the conversation's history reached it, so
// the runner can log a transcript hand-over instead of a resumed thread.
func TestNativeConversationResumeMode(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "thread",
			body: `{"conversation_id":"conv_1","resume":{"thread_id":"thread-9","transcript":[]},"pending":[],"cursor":0}`,
			want: runner.ConversationResumeThread,
		},
		{
			name: "transcript",
			body: `{"conversation_id":"conv_1","resume":{"thread_id":"","transcript":[{"role":"user","kind":"text","text":"earlier"}]},"pending":[],"cursor":0}`,
			want: runner.ConversationResumeTranscript,
		},
		{
			name: "nothing to resume",
			body: `{"conversation_id":"conv_1","resume":{"thread_id":"","transcript":[]},"pending":[],"cursor":0}`,
			want: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			useFastConversationTimings(t)
			hub, execution := newConversationHub(t)
			hub.bindBody = test.body
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
			if err != nil {
				t.Fatalf("bind error = %v", err)
			}
			defer func() {
				if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
					t.Errorf("close error = %v", err)
				}
			}()
			if got := session.ResumeMode(); got != test.want {
				t.Fatalf("resume mode = %q, want %q", got, test.want)
			}
		})
	}
}

// The session remembers the status of the last turn it reported so the runner
// can unbind a turn the provider interrupted as interrupted.
func TestNativeConversationTracksLastTurnStatus(t *testing.T) {
	useFastConversationTimings(t)
	_, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	if got := session.LastTurnStatus(); got != "" {
		t.Fatalf("last turn status before any turn = %q, want empty", got)
	}
	for _, test := range []struct {
		name   string
		events []runner.ConversationTurnEvent
		want   string
	}{
		{
			name:   "a started turn has no status yet",
			events: []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "t", TurnID: "turn-1"}},
			want:   "",
		},
		{
			name:   "completed",
			events: []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: runner.ConversationTurnCompleted}},
			want:   runner.ConversationTurnCompleted,
		},
		{
			name:   "interrupted wins as the newest turn",
			events: []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-2", Status: runner.ConversationTurnInterrupted}},
			want:   runner.ConversationTurnInterrupted,
		},
		{
			name:   "failed",
			events: []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-3", Status: runner.ConversationTurnFailed}},
			want:   runner.ConversationTurnFailed,
		},
	} {
		if err := session.Report(t.Context(), test.events); err != nil {
			t.Fatalf("%s: report = %v", test.name, err)
		}
		if got := session.LastTurnStatus(); got != test.want {
			t.Fatalf("%s: last turn status = %q, want %q", test.name, got, test.want)
		}
	}
	if err := session.Close(context.Background(), runner.ConversationOutcomeFailed, nil); err != nil {
		t.Fatalf("close error = %v", err)
	}
}

// A hub that accepts the report and never answers must not hold the run's
// teardown past the deadline the runner gave Close.
func TestNativeConversationCloseHonoursDeadline(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.hangReports = true
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "t", TurnID: "turn-1"}}); err != nil {
		t.Fatalf("report = %v", err)
	}
	hub.waitFor(t, "the report to reach the hub", func() bool { return hub.reportCallCount() > 0 })

	deadline := 300 * time.Millisecond
	closeCtx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	err = session.Close(closeCtx, runner.ConversationOutcomeFailed, errors.New("hub is down"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("close against a hung hub reported no error")
	}
	// Generous: the point is that Close does not wait out the report retries,
	// which are three attempts of a 15 second timeout each.
	if elapsed > 5*time.Second {
		t.Fatalf("close took %v, want it bounded by the caller's %v deadline", elapsed, deadline)
	}
}

// A batch the hub refuses for one of its events must not take the other
// events down with it: the client retries them one by one, in order.
func TestNativeConversationRejectedEventDropsOnlyItself(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.rejectKey = "bad"
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{
		{Type: runner.ConversationEventControlResult, Key: "k1", Status: runner.ConversationControlDelivered},
		{Type: runner.ConversationEventControlResult, Key: "bad", Status: runner.ConversationControlDelivered},
		{Type: runner.ConversationEventControlResult, Key: "k3", Status: runner.ConversationControlUnknown},
	}); err != nil {
		t.Fatalf("report = %v", err)
	}
	hub.waitFor(t, "the rejected batch to be retried event by event", func() bool { return hub.reportCallCount() >= 4 })
	if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
		t.Fatalf("close error = %v", err)
	}
	keys := make([]string, 0, 2)
	for _, event := range hub.events() {
		keys = append(keys, event.Key)
	}
	if strings.Join(keys, ",") != "k1,k3" {
		t.Fatalf("accepted events = %v, want the batch minus the rejected event, in order", keys)
	}
	if calls := hub.reportCallCount(); calls != 4 {
		t.Fatalf("report calls = %d, want the batch plus one call per event", calls)
	}
}

// Every turn-events request carries a client-generated idempotency key. A
// retry of the same batch reuses it so the hub can drop the duplicate; a
// different batch gets its own.
func TestNativeConversationReportsCarryBatchKeys(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	hub.reportFailure = 1
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "t", TurnID: "turn-1"}}); err != nil {
		t.Fatal(err)
	}
	hub.waitFor(t, "the retried batch", func() bool { return hub.batchCount() == 1 })
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: runner.ConversationTurnCompleted}}); err != nil {
		t.Fatal(err)
	}
	hub.waitFor(t, "the second batch", func() bool { return hub.batchCount() == 2 })
	if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
		t.Fatal(err)
	}
	keys := hub.postedBatchKeys()
	if len(keys) != 3 {
		t.Fatalf("turn-events requests = %d, want the failed attempt, its retry and the second batch", len(keys))
	}
	for i, key := range keys {
		if key == "" || len(key) > 128 {
			t.Fatalf("batch key %d = %q, want 1 to 128 bytes", i, key)
		}
	}
	if keys[0] != keys[1] {
		t.Fatalf("retry key = %q, want the first attempt's key %q", keys[1], keys[0])
	}
	if keys[2] == keys[0] {
		t.Fatalf("the second batch reused the first batch's key %q", keys[2])
	}
}

// A hub that refuses the poll for who the runner is will refuse it forever:
// polling stops instead of retrying for the whole run, and the attempt still
// unbinds with its outcome.
func TestNativeConversationPollStopsOnAuthorizationFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			useFastConversationTimings(t)
			hub, execution := newConversationHub(t)
			bound, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
			if err != nil {
				t.Fatal(err)
			}
			session, ok := bound.(*conversationSession)
			if !ok {
				t.Fatalf("bound session = %T, want the native session", bound)
			}
			control := session.Control(runner.ConversationTurnHooks{})
			hub.denyControls(status)
			hub.push(ConversationControl{Cursor: 1, Key: "denied", Kind: "message", MessageID: "msg_1", Text: "ignored"})
			awaitPollStopped(t, session)
			if refusals := hub.denyCount(); refusals != 1 {
				t.Fatalf("polling continued after %d: %d refusals, want 1", status, refusals)
			}
			select {
			case command := <-control.Commands:
				t.Fatalf("a control was delivered after the refusal: %#v", command)
			default:
			}
			if err := session.Close(t.Context(), runner.ConversationOutcomeFailed, nil); err != nil {
				t.Fatal(err)
			}
			if len(hub.unbinds) != 1 || hub.unbinds[0].Outcome != "failed" {
				t.Fatalf("unbind = %#v", hub.unbinds)
			}
		})
	}
}

// The end of a turn leaves no control queued for the next one: a hand-over
// that was still in flight when the turn ended is drained with the rest, so a
// control its waiter reports unknown is never handed over twice.
func TestNativeConversationTurnEndLeavesNoControlQueued(t *testing.T) {
	useFastConversationTimings(t)
	hub, execution := newConversationHub(t)
	bound, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := bound.(*conversationSession)
	if !ok {
		t.Fatalf("bound session = %T, want the native session", bound)
	}
	defer func() {
		if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
			t.Errorf("close error = %v", err)
		}
	}()
	for i := range 20 {
		key := "k" + strconv.Itoa(i)
		// A full queue holds the hand-over open across the end of the turn,
		// which is the interleaving that must not leave the control behind.
		for range conversationCommandQueueSize {
			session.commands <- runner.AgentControl{}
		}
		delivered := make(chan bool, 1)
		go func() {
			delivered <- session.deliver(t.Context(), ConversationControl{Cursor: int64(i + 1), Key: key, Kind: "message", MessageID: key, Text: "x"})
		}()
		// The cursor advances under the same lock that reads the live turn, so
		// waiting for it proves the hand-over took the turn that is about to
		// end and is now blocked on the full queue.
		hub.waitFor(t, "the hand-over to take the live turn", func() bool {
			session.mu.Lock()
			defer session.mu.Unlock()
			return session.cursor == int64(i+1)
		})
		session.FinishTurn()
		if !<-delivered {
			t.Fatalf("control %s was not handed over", key)
		}
		if queued := len(session.commands); queued != 0 {
			t.Fatalf("control %s left %d entries queued after the turn ended", key, queued)
		}
	}
	hub.waitFor(t, "the unknown control results", func() bool {
		event, ok := hub.controlResult("k19")
		return ok && event.Status == runner.ConversationControlUnknown
	})
}
