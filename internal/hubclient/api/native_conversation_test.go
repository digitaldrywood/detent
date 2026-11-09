package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
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
	rejectType    string
	bindHang      bool
	bindDelay     time.Duration
	unbinds       []ConversationUnbindRequest
	staleCalls    int
	controlsDeny  int
	denyCalls     int
	controlsWake  chan struct{}
}

type conversationExecutionFixture struct{ client *NativeClient }

func newConversationHub(t *testing.T) (*conversationHub, *conversationExecutionFixture) {
	t.Helper()
	if testing.Short() {
		t.Skip("conversation HTTP listener integration")
	}
	hub := &conversationHub{t: t, bindStatus: http.StatusOK, controlsWake: make(chan struct{}, 16), hangEvents: make(chan struct{})}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(hub.hangEvents) })
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client(), SessionTimings: fastSessionTimings()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	return hub, &conversationExecutionFixture{client: native}
}
func (e *conversationExecutionFixture) BindConversation(ctx context.Context, capabilities runner.ConversationCapabilities, resume string) (runner.ConversationSession, error) {
	identity := ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"}
	response, err := e.client.BindConversation(ctx, tracker.NativeWorkItemID("wi_"+strings.Repeat("c", 32)), ConversationBindRequest{ConversationIdentity: identity, RunID: "run_1", Capabilities: capabilities, ThreadID: strings.TrimSpace(resume)})
	if err != nil {
		return nil, err
	}
	return newConversationSession(ctx, e.client, tracker.NativeWorkItemID("wi_"+strings.Repeat("c", 32)), identity, response, func(context.Context) error { return nil }, slog.Default(), nil), nil
}
func fastSessionTimings() SessionTimings {
	return SessionTimings{FlushInterval: 20 * time.Millisecond, ReportBackoff: 10 * time.Millisecond, PollWait: time.Second}
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
	hang := h.bindHang
	delay := h.bindDelay
	h.mu.Unlock()
	if delay > 0 && !sleepContext(r.Context(), delay) {
		return
	}
	if hang {
		select {
		case <-h.hangEvents:
		case <-r.Context().Done():
		}
		return
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
	hang, reject, rejectType := h.hangReports, h.rejectKey, h.rejectType
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
		if (reject != "" && event.Key == reject) || (rejectType != "" && event.Type == rejectType) {
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
	testenv.WaitUntil(t, t.Context(), func() error {
		if condition() {
			return nil
		}
		return fmt.Errorf("timed out waiting for %s", what)
	})
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
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("the control poll did not stop")
	}
}

func TestNativeConversationSessionDeliversControlsInOrder(t *testing.T) {
	hub, execution := newConversationHub(t)
	hub.bindThread = "thread-0"
	hub.bindPending = []ConversationControl{
		{Cursor: 1, Key: "k1", Kind: "message", MessageID: "msg_1", Text: "first"},
		{Cursor: 2, Key: "k2", Kind: "message", MessageID: "msg_2", Text: "second"},
	}
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true, Interrupt: true, Answer: true}, "")
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
		case <-testenv.WaitContext(t, t.Context()).Done():
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

func TestNativeConversationReportBatchesAndRetries(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("decline question behind prefix=%t", queued), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fourth, release, finished, providerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				var requests []ConversationTurnEventsRequest
				committed := make(map[string]struct{})
				var accepted []runner.ConversationTurnEvent
				client, err := New(Config{URL: "https://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					var batch ConversationTurnEventsRequest
					if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
						return nil, err
					}
					requests = append(requests, batch)
					if _, ok := committed[batch.BatchKey]; !ok {
						committed[batch.BatchKey] = struct{}{}
						accepted = append(accepted, batch.Events...)
					}
					if len(requests) < 4 {
						return nil, io.ErrUnexpectedEOF
					}
					if len(requests) == 4 {
						close(fourth)
						select {
						case <-release:
						case <-request.Context().Done():
							return nil, request.Context().Err()
						}
					}
					return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"event_seq":1}`)), Request: request}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				question := runner.ConversationTurnEvent{Type: runner.ConversationEventQuestionOpened, ThreadID: "thread-1", TurnID: "turn-1", RequestID: "req-1", Prompts: json.RawMessage(`[{"id":"approach","question":"Which?","options":[],"free_text":true}]`)}
				batch := []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}}
				session := &conversationSession{lifetimeDone: ctx.Done(), lifetimeCause: func() error { return context.Cause(ctx) }, client: native, id: "conv_test", identity: ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"}, commands: make(chan runner.AgentControl, 64), logger: slog.Default()}
				if queued {
					session.events = []runner.ConversationTurnEvent{question}
				} else {
					batch = append(batch, question)
				}
				resumed := make(chan runner.AgentControl, 1)
				go func() {
					defer close(providerDone)
					select {
					case command := <-session.commands:
						resumed <- command
					case <-ctx.Done():
					}
				}()
				result := make(chan error, 1)
				go func() { defer close(finished); result <- session.post(ctx, batch) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-release:
					default:
						close(release)
					}
					<-finished
					<-providerDone
				})
				select {
				case <-fourth:
				case err := <-result:
					t.Fatalf("discarded the uncertain prefix before recovery: %v", err)
				}
				synctest.Wait()
				select {
				case command := <-resumed:
					if err := command.Validate(); err != nil {
						t.Fatal(err)
					}
					if command.Kind != runner.AgentControlAnswer || command.RequestID != "req-1" || command.ThreadID != "thread-1" || command.TurnID != "turn-1" || len(command.Answers["approach"]) != 1 || !strings.Contains(command.Answers["approach"][0], "could not show this question") {
						t.Fatalf("provider did not resume with the matching decline: %+v", command)
					}
				default:
					t.Fatal("provider remained blocked behind an unavailable prefix")
				}
				close(release)
				if err := <-result; err != nil {
					t.Fatal(err)
				}
				if len(requests) != 4 || len(committed) != 1 || !reflect.DeepEqual(accepted, batch) {
					t.Fatalf("uncertain batch duplicated or lost content: calls=%d receipts=%d events=%+v", len(requests), len(committed), accepted)
				}
				for _, request := range requests {
					if request.BatchKey == "" || request.BatchKey != requests[0].BatchKey || !reflect.DeepEqual(request.Events, batch) {
						t.Fatalf("question decline changed an uncertain batch: %+v", requests)
					}
				}
				if len(session.commands) != 0 || len(session.events) != 0 {
					t.Fatalf("question was queued again after decline: controls=%d events=%+v", len(session.commands), session.events)
				}
			})
		})
	}
	for _, failures := range []int{3, 8} {
		t.Run(fmt.Sprintf("recover after %d transient responses", failures), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub := &conversationHub{t: t, reportFailure: failures}
				client, err := New(Config{URL: "https://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					hub.ServeHTTP(response, request)
					return response.Result(), nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				session := &conversationSession{client: native, id: "conv-1", identity: ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"}}
				events := []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "t", TurnID: "turn-1"}}
				if err := session.postBatch(t.Context(), events, nil); err != nil {
					t.Fatalf("discarded a batch before the Hub recovered: %v", err)
				}
				if got := hub.events(); !reflect.DeepEqual(got, events) {
					t.Fatalf("recovered events = %+v, want %+v", got, events)
				}
				keys := hub.postedBatchKeys()
				if len(keys) != failures+1 {
					t.Fatalf("requests = %d, want %d", len(keys), failures+1)
				}
				for _, key := range keys {
					if key == "" || key != keys[0] {
						t.Fatalf("retry changed batch key: %q", keys)
					}
				}
			})
		})
	}
	hub, execution := newConversationHub(t)
	hub.reportFailure = 1
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{}, "")
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
	session.FinishTurn()
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
	hub, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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
	hub, execution := newConversationHub(t)
	bound, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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

// transcriptBindBody is a bind response for a conversation whose provider
// thread was produced by another runner: no resume thread, a bounded
// transcript instead.
const transcriptBindBody = `{"conversation_id":"conv_1","resume":{"thread_id":"","thread_origin":"worker","transcript":[` +
	`{"role":"user","kind":"text","text":"How is the release going?"},` +
	`{"role":"assistant","kind":"text","text":"Two issues are in review."}` +
	`]},"pending":[{"cursor":4,"key":"k1","kind":"message","message_id":"msg_1","text":"Anything blocked?"}],"cursor":4}`

func TestNativeConversationBindDecodesTranscript(t *testing.T) {
	hub, execution := newConversationHub(t)
	hub.bindBody = transcriptBindBody
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	defer func() {
		if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
			t.Errorf("close error = %v", err)
		}
	}()
	if origin := session.(interface{ ThreadOrigin() string }).ThreadOrigin(); origin != "worker" {
		t.Fatalf("thread origin = %q, want worker", origin)
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
	hub, execution := newConversationHub(t)
	hub.bindBody = `{"conversation_id":"conv_1","resume":{"thread_id":"thread-9","transcript":[{"role":"user","kind":"text","text":"earlier"}]},"pending":[],"cursor":0}`
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	defer func() {
		if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
			t.Errorf("close error = %v", err)
		}
	}()
	if session.ResumeThreadID() != "thread-9" {
		t.Fatalf("resume thread = %q", session.ResumeThreadID())
	}
	if prompt, _ := session.PendingPrompt(); prompt != "" {
		t.Fatalf("pending prompt = %q, want empty: the provider thread already carries the history", prompt)
	}
}

// The session tells the runner how the conversation's history reached it, so
// the runner can log a transcript hand-over instead of a resumed thread.

// The session remembers the status of the last turn it reported so the runner
// can unbind a turn the provider interrupted as interrupted.

func TestNativeConversationTracksLastTurnStatus(t *testing.T) {
	_, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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
	if testing.Short() {
		t.Skip("network listener integration")
	}

	hub, execution := newConversationHub(t)
	hub.hangReports = true
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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
	if elapsed > 5*time.Second {
		t.Fatalf("close took %v, want it bounded by the caller's %v deadline", elapsed, deadline)
	}
}

// A batch the hub refuses for one of its events must not take the other
// events down with it: the client retries them one by one, in order.

func TestNativeConversationRejectedEventDropsOnlyItself(t *testing.T) {
	hub, execution := newConversationHub(t)
	hub.rejectKey = "bad"
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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

func TestNativeConversationPublicationConflictPreservesBatch(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       string
		multiple   bool
		persistent bool
		wantCalls  int
		wantError  bool
	}{
		{name: "concurrent publication", code: "revision_conflict", wantCalls: 2},
		{name: "concurrent publication preserves multiple events", code: "revision_conflict", multiple: true, wantCalls: 2},
		{name: "persistent conflict stops on cancellation", code: "revision_conflict", multiple: true, persistent: true, wantCalls: 3, wantError: true},
		{name: "lost execution", code: "stale_execution", wantCalls: 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var requests []ConversationTurnEventsRequest
				client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					var batch ConversationTurnEventsRequest
					if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
						return nil, err
					}
					requests = append(requests, batch)
					if test.persistent && len(requests) == test.wantCalls {
						cancel()
					}
					status, body := http.StatusAccepted, `{"event_seq":1}`
					if len(requests) == 1 || test.persistent {
						status, body = http.StatusConflict, fmt.Sprintf(`{"code":%q,"message":"publication refused"}`, test.code)
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				session := &conversationSession{client: native, id: "conv_test", identity: ConversationIdentity{LeaseID: "lease_test", FencingToken: 1, AttemptID: "attempt_test"}}
				batch := []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread_test", TurnID: "turn_test"}}
				if test.multiple {
					batch = append(batch, runner.ConversationTurnEvent{Type: runner.ConversationEventControlResult, Key: "continue_test", Status: runner.ConversationControlDelivered})
				}
				err = session.post(ctx, batch)
				if test.persistent && !errors.Is(err, context.Canceled) {
					t.Fatalf("persistent conflict lost cancellation: %v", err)
				}
				if (err != nil) != test.wantError || len(requests) != test.wantCalls {
					t.Fatalf("report calls=%d error=%v, want calls=%d error=%t", len(requests), err, test.wantCalls, test.wantError)
				}
				for _, request := range requests {
					if request.BatchKey == "" || request.BatchKey != requests[0].BatchKey || !reflect.DeepEqual(request.Events, batch) || request.ConversationIdentity != session.identity {
						t.Fatalf("publication changed its batch or authority: %+v", requests)
					}
				}
			})
		})
	}
}

// Every turn-events request carries a client-generated idempotency key. A
// retry of the same batch reuses it so the hub can drop the duplicate; a
// different batch gets its own.

func TestNativeConversationReportsCarryBatchKeys(t *testing.T) {
	hub, execution := newConversationHub(t)
	hub.reportFailure = 1
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{}, "")
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
	session.FinishTurn()
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
			hub, execution := newConversationHub(t)
			bound, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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
	if testing.Short() {
		t.Skip("network listener integration")
	}

	hub, execution := newConversationHub(t)
	bound, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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

func TestTrimConversationEvents(t *testing.T) {
	t.Parallel()
	delta := func(text string) runner.ConversationTurnEvent {
		return runner.ConversationTurnEvent{Type: runner.ConversationEventDelta, ProviderItemID: "item", Text: text}
	}
	started := runner.ConversationTurnEvent{Type: runner.ConversationEventTurnStarted, TurnID: "turn-1"}
	completed := runner.ConversationTurnEvent{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: "completed"}
	tests := []struct {
		name        string
		events      []runner.ConversationTurnEvent
		limit       int
		wantTypes   []string
		wantDropped int
	}{
		{name: "under the limit", events: []runner.ConversationTurnEvent{started, delta("ab"), completed}, limit: 10, wantTypes: []string{"turn_started", "delta", "turn_completed"}},
		{name: "oldest deltas go first", events: []runner.ConversationTurnEvent{started, delta("aaaa"), delta("bbbb"), delta("cc"), completed}, limit: 6, wantTypes: []string{"turn_started", "delta", "delta", "turn_completed"}, wantDropped: 1},
		{name: "non-delta events are never dropped", events: []runner.ConversationTurnEvent{started, {Type: runner.ConversationEventItem, Summary: "long summary"}, completed}, limit: 1, wantTypes: []string{"turn_started", "item", "turn_completed"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			kept, dropped := trimConversationEvents(test.events, test.limit)
			types := make([]string, 0, len(kept))
			for _, event := range kept {
				types = append(types, event.Type)
			}
			if strings.Join(types, ",") != strings.Join(test.wantTypes, ",") || dropped != test.wantDropped {
				t.Fatalf("kept %v dropped %d, want %v dropped %d", types, dropped, test.wantTypes, test.wantDropped)
			}
		})
	}
}

func TestNativeConversationBoundsEventsWhileTheHubStalls(t *testing.T) {
	for _, test := range []struct {
		name    string
		bytes   int
		want    int
		recover bool
	}{
		{name: "control results recover at count capacity", bytes: 1 << 20, want: 500, recover: true},
		{name: "control results cancel at count capacity", bytes: 1 << 20, want: 500},
		{name: "control results recover at byte capacity", bytes: 1024, want: 2, recover: true},
		{name: "control results cancel at byte capacity", bytes: 1024, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				total := 2*test.want + 3
				blocked, release, acceptedAll := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var accepted []runner.ConversationTurnEvent
				attempts := 0
				receipts := make(map[string]struct{})
				client, err := New(Config{URL: "https://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					var batch ConversationTurnEventsRequest
					if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
						return nil, err
					}
					attempts++
					if attempts < 4 {
						return nil, io.ErrUnexpectedEOF
					}
					if attempts == 4 {
						close(blocked)
						select {
						case <-release:
						case <-request.Context().Done():
							return nil, request.Context().Err()
						}
					}
					if _, found := receipts[batch.BatchKey]; !found {
						receipts[batch.BatchKey] = struct{}{}
						accepted = append(accepted, batch.Events...)
						if len(accepted) == total+2 {
							close(acceptedAll)
						}
					}
					return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				session := &conversationSession{
					lifetimeDone: ctx.Done(), lifetimeCause: func() error { return context.Cause(ctx) }, client: native, id: "conv_test",
					identity: ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"},
					timings:  SessionTimings{MaxQueuedBytes: test.bytes}, logger: slog.Default(),
					thread: "thread-1", turn: "turn-1", turnEnded: make(chan struct{}), turnWaiters: &sync.WaitGroup{},
					commands: make(chan runner.AgentControl, 64), kick: make(chan struct{}, 1), stop: make(chan struct{}), closed: make(chan struct{}),
				}
				prefixDone, producerDone, providerDone, consumedAll := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				prefixResult, producerResult := make(chan error, 1), make(chan bool, 1)
				received := make(chan string, total)
				go func() {
					defer close(prefixDone)
					prefixResult <- session.post(ctx, []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}})
				}()
				go func() {
					defer close(providerDone)
					for {
						select {
						case command := <-session.commands:
							if err := command.Validate(); err != nil {
								t.Errorf("invalid provider control: %v", err)
							}
							received <- command.MessageID
							command.Reply <- fmt.Errorf("%w: %s", runner.ErrInvalidConversationControl, strings.Repeat("界", 1024))
							if len(received) == total {
								close(consumedAll)
							}
						case <-ctx.Done():
							return
						}
					}
				}()
				go func() {
					defer close(producerDone)
					for i := range total {
						if !session.deliver(ctx, ConversationControl{Cursor: int64(i + 1), Key: "control-" + strconv.Itoa(i), Kind: "message", MessageID: "msg_" + strconv.Itoa(i), Text: "Update the example.", Expected: ConversationExpected{AttemptID: "attempt_1"}}) {
							producerResult <- false
							return
						}
					}
					producerResult <- true
				}()
				t.Cleanup(func() {
					cancel()
					select {
					case <-release:
					default:
						close(release)
					}
					<-producerDone
					session.FinishTurn()
					<-providerDone
					<-prefixDone
					session.loops.Wait()
				})
				<-blocked
				synctest.Wait()
				session.mu.Lock()
				cursor := session.cursor
				session.mu.Unlock()
				if cursor != int64(test.want) || len(received) != test.want {
					t.Fatalf("unavailable reports admitted %d controls at cursor %d, want %d", len(received), cursor, test.want)
				}
				select {
				case <-producerDone:
					t.Fatal("control polling ignored report backpressure")
				default:
				}
				if !test.recover {
					cancel()
					<-producerDone
					if <-producerResult {
						t.Fatal("cancelled delivery acknowledged controls without report capacity")
					}
					return
				}
				close(release)
				if err := <-prefixResult; err != nil {
					t.Fatal(err)
				}
				session.loops.Add(1)
				go session.reportLoop(ctx, ctx)
				<-producerDone
				if !<-producerResult {
					t.Fatal("recovered report capacity did not resume control delivery")
				}
				<-consumedAll
				if err := session.Report(ctx, []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: "completed"}}); err != nil {
					t.Fatal(err)
				}
				session.FinishTurn()
				<-acceptedAll
				if len(accepted) != total+2 || accepted[0].Type != runner.ConversationEventTurnStarted || accepted[len(accepted)-1].Type != runner.ConversationEventTurnCompleted {
					t.Fatal("recovery lost events or completed before all control results")
				}
				seen := make(map[string]bool)
				for _, event := range accepted[1 : len(accepted)-1] {
					if event.Type != runner.ConversationEventControlResult || event.Status != runner.ConversationControlRejected || seen[event.Key] || len(event.Error) > 500 || !utf8.ValidString(event.Error) || !strings.HasPrefix(event.Error, "invalid conversation control") {
						t.Fatalf("control result lost identity, bounded reason or outcome: %+v", event)
					}
					seen[event.Key] = true
				}
				for i := range total {
					if !seen["control-"+strconv.Itoa(i)] {
						t.Fatalf("recovery lost control result %d", i)
					}
				}
			})
		})
	}
	t.Run("outstanding controls reserve ordinary report capacity", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session := &conversationSession{
				lifetimeDone: ctx.Done(), lifetimeCause: func() error { return context.Cause(ctx) }, timings: SessionTimings{MaxQueuedBytes: 1000}, logger: slog.Default(),
				thread: "thread-1", turn: "turn-1", turnEnded: make(chan struct{}), turnWaiters: &sync.WaitGroup{},
				commands: make(chan runner.AgentControl, 2), kick: make(chan struct{}, 1), stop: make(chan struct{}),
			}
			finished, result := make(chan struct{}), make(chan error, 1)
			started := false
			t.Cleanup(func() {
				cancel()
				session.FinishTurn()
				if started {
					<-finished
				}
			})
			for i := range 2 {
				if !session.deliver(ctx, ConversationControl{Cursor: int64(i + 1), Key: strconv.Itoa(i), Kind: "message", MessageID: "msg_" + strconv.Itoa(i), Text: "Update the example."}) {
					t.Fatal("initial control delivery failed")
				}
			}
			started = true
			go func() {
				defer close(finished)
				result <- session.Report(context.Background(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventItem, ProviderItemID: "tool-1", Kind: runner.ConversationItemTool, Summary: "Queued tool result"}})
			}()
			synctest.Wait()
			select {
			case err := <-result:
				t.Fatalf("ordinary producer consumed reserved control capacity: %v", err)
			default:
			}
			session.FinishTurn()
			if err := <-result; err != nil {
				t.Fatalf("settled controls did not restore ordinary report capacity: %v", err)
			}
			var controls, tools int
			for _, event := range session.events {
				switch event.Type {
				case runner.ConversationEventControlResult:
					if event.Status != runner.ConversationControlUnknown {
						t.Fatal("unconsumed control lost its unknown outcome")
					}
					controls++
				case runner.ConversationEventItem:
					if event.ProviderItemID != "tool-1" {
						t.Fatal("ordinary producer lost its queued result")
					}
					tools++
				}
			}
			if controls != 2 || tools != 1 {
				t.Fatalf("settled results: controls=%d tools=%d", controls, tools)
			}
		})
	})
	t.Run("cancelled flush preserves queue under production", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			lifetime, stopProducer := context.WithCancelCause(t.Context())
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			original := runner.ConversationTurnEvent{Type: runner.ConversationEventItem, ProviderItemID: "original", Kind: runner.ConversationItemTool, Summary: "Original queued work"}
			session := &conversationSession{lifetimeDone: lifetime.Done(), lifetimeCause: func() error { return context.Cause(lifetime) }, events: []runner.ConversationTurnEvent{original}, kick: make(chan struct{}, 1), stop: make(chan struct{}), logger: slog.Default()}
			events := make([]runner.ConversationTurnEvent, 1000)
			for i := range events {
				events[i] = runner.ConversationTurnEvent{Type: runner.ConversationEventItem, ProviderItemID: "later-" + strconv.Itoa(i), Kind: runner.ConversationItemTool, Summary: "More queued work"}
			}
			finished := make(chan struct{})
			var reportErr error
			go func() { defer close(finished); reportErr = session.Report(context.Background(), events) }()
			t.Cleanup(func() { stopProducer(context.Canceled); <-finished })
			synctest.Wait()
			if err := session.flush(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled flush = %v", err)
			}
			session.mu.Lock()
			preserved := len(session.events) > 0 && reflect.DeepEqual(session.events[0], original)
			session.mu.Unlock()
			if !preserved {
				t.Fatal("cancelled flush kept draining producer records")
			}
			cause := errors.New("execution cancelled")
			stopProducer(cause)
			<-finished
			if !errors.Is(reportErr, cause) {
				t.Errorf("producer cancellation cause = %v, want %v", reportErr, cause)
			}
		})
	})
	t.Run("question callback refuses full queues", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			session := &conversationSession{lifetimeDone: ctx.Done(), lifetimeCause: func() error { return context.Cause(ctx) }, events: make([]runner.ConversationTurnEvent, 500), commands: make(chan runner.AgentControl, 64), kick: make(chan struct{}, 1), stop: make(chan struct{}), logger: slog.Default()}
			for i := range session.events {
				session.events[i] = runner.ConversationTurnEvent{Type: runner.ConversationEventItem, ProviderItemID: strconv.Itoa(i), Kind: runner.ConversationItemTool, Summary: "Queued work"}
			}
			for range 64 {
				session.commands <- runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread-1", TurnID: "turn-1", Text: "Queued message", Check: func(context.Context) error { return nil }, Reply: make(chan error, 1)}
			}
			control := session.Control(runner.ConversationTurnHooks{InputRequested: func(request runner.AgentInputRequest) error {
				return session.Report(context.Background(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventQuestionOpened, RequestID: request.ID, ThreadID: request.ThreadID, TurnID: request.TurnID}})
			}})
			result, finished := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(finished)
				result <- control.InputRequested(runner.AgentInputRequest{ID: "req-full", ThreadID: "thread-1", TurnID: "turn-1"})
			}()
			t.Cleanup(func() { cancel(); <-finished })
			synctest.Wait()
			select {
			case err := <-result:
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("full-queue callback lost its infrastructure refusal: %v", err)
				}
			default:
				t.Fatal("question callback blocked behind a full report and control queue")
			}
			if len(session.events) != 500 || len(session.commands) != 64 {
				t.Fatal("refused question changed queued events or emitted a duplicate provider control")
			}
		})
	})
	for _, test := range []struct {
		name            string
		recoverHub      bool
		cancelExecution bool
		events          int
		summary         string
		wantCount       int
		wantBytes       int
	}{
		{name: "ordered events recover", recoverHub: true, events: 5, summary: strings.Repeat("x", 64), wantCount: 2, wantBytes: 128},
		{name: "ordered events stop", events: 5, summary: strings.Repeat("x", 64), wantCount: 2, wantBytes: 128},
		{name: "execution cancels background callback", cancelExecution: true, events: 5, summary: strings.Repeat("x", 64), wantCount: 2, wantBytes: 128},
		{name: "empty text frames recover", recoverHub: true, events: 1001, wantCount: 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub := &conversationHub{t: t, reportFailure: 10000}
				posted := make(chan struct{}, 1)
				client, err := New(Config{URL: "https://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if strings.HasSuffix(request.URL.Path, "/controls") {
						<-request.Context().Done()
						return nil, request.Context().Err()
					}
					if err := request.Context().Err(); err != nil {
						return nil, err
					}
					if strings.HasSuffix(request.URL.Path, "/turn-events") {
						select {
						case posted <- struct{}{}:
						default:
						}
					}
					response := httptest.NewRecorder()
					hub.ServeHTTP(response, request)
					return response.Result(), nil
				})}, SessionTimings: SessionTimings{MaxQueuedBytes: 128}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				lifetime, cancelExecution := context.WithCancel(t.Context())
				defer cancelExecution()
				session := newConversationSession(lifetime, native, tracker.NativeWorkItemID("wi_"+strings.Repeat("c", 32)), ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"}, ConversationBindResponse{ConversationID: "conv_test"}, nil, slog.Default(), nil)
				t.Cleanup(func() { _ = session.Close(context.Background(), runner.ConversationOutcomeInterrupted, nil) })
				events := make([]runner.ConversationTurnEvent, test.events)
				for i := range events {
					events[i] = runner.ConversationTurnEvent{Type: runner.ConversationEventItem, ProviderItemID: strconv.Itoa(i), Kind: runner.ConversationItemTool, Summary: test.summary}
				}
				reported := make(chan error, 1)
				go func() { reported <- session.Report(context.Background(), events) }()
				<-posted
				synctest.Wait()
				session.mu.Lock()
				count, queued := len(session.events), conversationQueuedBytes(session.events)
				session.mu.Unlock()
				if count != test.wantCount || queued != test.wantBytes {
					t.Fatalf("outage queue = %d events/%d bytes, want %d/%d", count, queued, test.wantCount, test.wantBytes)
				}
				select {
				case err := <-reported:
					t.Fatalf("producer did not wait for report capacity: %v", err)
				default:
				}
				if test.cancelExecution {
					cancelExecution()
					synctest.Wait()
					select {
					case err := <-reported:
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("background callback ignored execution cancellation: %v", err)
						}
					default:
						t.Fatal("background callback remained blocked after execution cancellation")
					}
					hub.mu.Lock()
					hub.reportFailure = 0
					hub.mu.Unlock()
					if err := session.Close(context.Background(), runner.ConversationOutcomeInterrupted, nil); err != nil {
						t.Fatal(err)
					}
				} else if test.recoverHub {
					hub.mu.Lock()
					hub.reportFailure = 0
					hub.mu.Unlock()
					if err := <-reported; err != nil {
						t.Fatal(err)
					}
					if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
						t.Fatal(err)
					}
					if got := hub.events(); !reflect.DeepEqual(got, events) {
						t.Fatalf("recovered ordered events = %+v, want %+v", got, events)
					}
				} else {
					if err := session.Close(context.Background(), runner.ConversationOutcomeInterrupted, nil); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("persistent outage shutdown = %v, want deadline exceeded", err)
					}
					if err := <-reported; !errors.Is(err, ErrStaleConversation) {
						t.Fatalf("blocked producer shutdown = %v, want stale conversation", err)
					}
				}
			})
		})
	}
	for _, test := range []struct {
		name            string
		frames          int
		summary         string
		completionError string
		wantAccepted    int
	}{
		{name: "held byte limit", frames: 3, summary: strings.Repeat("x", 64), wantAccepted: 3},
		{name: "held frame limit", frames: 500, wantAccepted: 500},
		{name: "oversized completion", completionError: strings.Repeat("x", 129)},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub := &conversationHub{t: t}
				client, err := New(Config{URL: "https://hub.example", TokenSource: func() string { return "test" }, HTTPClient: providerHandlerClient(hub)})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				session := &conversationSession{lifetimeDone: t.Context().Done(), lifetimeCause: func() error { return context.Cause(t.Context()) }, client: native, id: "conv_test", identity: ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"}, timings: SessionTimings{MaxQueuedBytes: 128}, turnEnded: make(chan struct{}), turnWaiters: &sync.WaitGroup{}, commands: make(chan runner.AgentControl), kick: make(chan struct{}, 1), logger: slog.Default()}
				if !session.reserveControlResult(t.Context()) {
					t.Fatal("initial control result has no report capacity")
				}
				session.waiters.Add(1)
				session.turnWaiters.Add(1)
				go session.await("control-1", runner.AgentControl{Reply: make(chan error)}, session.turnEnded, session.turnWaiters)
				t.Cleanup(func() {
					session.mu.Lock()
					ended := session.turnEnded
					session.mu.Unlock()
					select {
					case <-ended:
					default:
						close(ended)
					}
					session.waiters.Wait()
				})
				events := []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, TurnID: "turn-1", Status: runner.ConversationTurnCompleted, Error: test.completionError}}
				for i := range test.frames {
					events = append(events, runner.ConversationTurnEvent{Type: runner.ConversationEventItem, ProviderItemID: strconv.Itoa(i), Kind: runner.ConversationItemTool, Summary: test.summary})
				}
				err = session.Report(t.Context(), events)
				if err == nil || errors.Is(err, ErrUnavailable) {
					t.Fatalf("held overflow = %v, want a non-retryable protocol refusal", err)
				}
				if !reflect.DeepEqual(session.held, events[:test.wantAccepted]) && (len(session.held) != 0 || test.wantAccepted != 0) {
					t.Fatalf("partial admission lost accepted prefix: %+v", session.held)
				}
				session.FinishTurn()
				session.waiters.Wait()
				if err := session.flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				got := hub.events()
				if len(got) != test.wantAccepted+1 || got[0].Type != runner.ConversationEventControlResult || got[0].Key != "control-1" || got[0].Status != runner.ConversationControlUnknown || !reflect.DeepEqual(got[1:], events[:test.wantAccepted]) {
					t.Fatalf("overflow reordered completion/control receipts: %+v", got)
				}
				started := runner.ConversationTurnEvent{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-2", TurnID: "turn-2"}
				if err := session.Report(t.Context(), []runner.ConversationTurnEvent{started}); err != nil {
					t.Fatalf("FinishTurn did not restore admission: %v", err)
				}
				if err := session.flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				got = hub.events()
				if !reflect.DeepEqual(got[len(got)-1], started) {
					t.Fatalf("next turn was lost: %+v", got)
				}
			})
		})
	}
	const queueLimit = 4096
	hub, execution := newConversationHub(t)
	execution.client.client.sessionTimings.MaxQueuedBytes = queueLimit
	hub.mu.Lock()
	hub.hangReports = true
	hub.mu.Unlock()
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
	if err != nil {
		t.Fatalf("bind error = %v", err)
	}
	native := session.(*conversationSession)
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}}); err != nil {
		t.Fatal(err)
	}
	hub.waitFor(t, "the first post to stall", func() bool { return hub.reportCallCount() > 0 })
	for index := range 200 {
		event := runner.ConversationTurnEvent{Type: runner.ConversationEventDelta, ProviderItemID: "item-" + strconv.Itoa(index%3), Text: strings.Repeat("x", 100)}
		if err := session.Report(t.Context(), []runner.ConversationTurnEvent{event}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	session.FinishTurn()
	native.mu.Lock()
	queued := conversationQueuedBytes(native.events)
	last := native.events[len(native.events)-1]
	native.mu.Unlock()
	if queued > queueLimit {
		t.Fatalf("queued %d bytes while the hub stalls, want at most %d", queued, queueLimit)
	}
	if last.Type != runner.ConversationEventTurnCompleted {
		t.Fatalf("last queued event = %#v, want the turn completion kept", last)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = session.Close(closeCtx, runner.ConversationOutcomeSucceeded, nil)
}

func TestNativeConversationReportsControlResultsBeforeTheTurnEnds(t *testing.T) {
	for _, test := range []struct {
		name       string
		reply      error
		consume    bool
		wantStatus string
	}{
		{name: "control acknowledged after the provider ended the turn", consume: true, wantStatus: runner.ConversationControlDelivered},
		{name: "control refused after the provider ended the turn", consume: true, reply: runner.ErrStaleConversationControl, wantStatus: runner.ConversationControlRejected},
		{name: "control never consumed", wantStatus: runner.ConversationControlUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, execution := newConversationHub(t)
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Interrupt: true}, "")
			if err != nil {
				t.Fatal(err)
			}
			native := session.(*conversationSession)
			control := session.Control(runner.ConversationTurnHooks{})
			if err := session.Report(t.Context(), []runner.ConversationTurnEvent{{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}}); err != nil {
				t.Fatal(err)
			}
			hub.push(ConversationControl{Cursor: 1, Key: "k1", Kind: "interrupt", MessageID: "msg_1"})
			var command runner.AgentControl
			select {
			case command = <-control.Commands:
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("the control never reached the turn")
			}
			completed := runner.ConversationTurnEvent{Type: runner.ConversationEventTurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Status: "interrupted"}
			if err := session.Report(t.Context(), []runner.ConversationTurnEvent{completed}); err != nil {
				t.Fatal(err)
			}
			native.mu.Lock()
			held := len(native.held)
			native.mu.Unlock()
			if held != 1 {
				t.Fatalf("held events = %d, want the turn end held until the turn finishes", held)
			}
			if test.consume {
				command.Reply <- test.reply
			} else {
				native.drainCommands()
			}
			session.FinishTurn()
			if err := session.Close(context.Background(), runner.ConversationOutcomeInterrupted, nil); err != nil {
				t.Fatal(err)
			}
			var order []string
			for _, event := range hub.events() {
				switch event.Type {
				case runner.ConversationEventControlResult:
					if event.Status != test.wantStatus {
						t.Fatalf("control result = %#v, want %s", event, test.wantStatus)
					}
					order = append(order, event.Type)
				case runner.ConversationEventTurnCompleted:
					order = append(order, event.Type)
				}
			}
			if strings.Join(order, ",") != "control_result,turn_completed" {
				t.Fatalf("posted order = %v, want the control result before the turn end", order)
			}
		})
	}
}
