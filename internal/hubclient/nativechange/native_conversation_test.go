package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
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

func newConversationHub(t *testing.T, timeouts ...time.Duration) (*conversationHub, *nativeExecution) {
	t.Helper()
	hub := &conversationHub{t: t, bindStatus: http.StatusOK, controlsWake: make(chan struct{}, 16), hangEvents: make(chan struct{})}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	// Released before the server is closed so a hung handler cannot block it.
	t.Cleanup(func() { close(hub.hangEvents) })
	httpClient := server.Client()
	if len(timeouts) > 0 {
		httpClient.Timeout = timeouts[0]
	}
	client, err := newTestClient(t, Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	source, _ := NewNativeConnector(native)
	id := "wi_" + strings.Repeat("c", 32)
	lease := tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), ID: "lease-1", FencingToken: 7, PolicyID: "policy"}
	fixtureInstallClaim(t, scheduler, nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Hour)}, policy.Descriptor{ID: "policy"}, "project")
	execution := &nativeExecution{scheduler: authority(scheduler), claim: nativeClaim{source: source, lease: lease, deadline: time.Now().Add(time.Hour)}, data: tracker.NativeRunData{
		RunID: "run_1", AttemptID: "attempt_1", LeaseID: lease.ID, FencingToken: lease.FencingToken,
		Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example-model"},
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
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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

func TestNativeConversationControlCheckRejectsLostLease(t *testing.T) {
	hub, execution := newConversationHub(t)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
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
	fixtureSetNow(execution.scheduler.host.(*Scheduler), time.Now().Add(2*time.Hour))
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

func TestNativeConversationDeclinesQuestionsTheHubDropped(t *testing.T) {
	for _, test := range []struct {
		name   string
		script func(*conversationHub)
	}{
		{name: "hub rejects the question", script: func(h *conversationHub) { h.rejectType = runner.ConversationEventQuestionOpened }},
		{name: "hub stays unavailable", script: func(h *conversationHub) { h.reportFailure = 100 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, execution := newConversationHub(t)
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Answer: true}, "")
			if err != nil {
				t.Fatalf("bind error = %v", err)
			}
			defer func() { _ = session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil) }()
			hub.mu.Lock()
			test.script(hub)
			hub.mu.Unlock()
			control := session.Control(runner.ConversationTurnHooks{})
			events := []runner.ConversationTurnEvent{
				{Type: runner.ConversationEventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"},
				{Type: runner.ConversationEventQuestionOpened, ThreadID: "thread-1", TurnID: "turn-1", RequestID: "req-1", Prompts: json.RawMessage(`[{"id":"approach","header":"","question":"Which?","options":[],"free_text":true}]`)},
			}
			if err := session.Report(t.Context(), events); err != nil {
				t.Fatal(err)
			}
			select {
			case command := <-control.Commands:
				if err := command.Validate(); err != nil {
					t.Fatalf("decline control is invalid: %v", err)
				}
				if command.Kind != runner.AgentControlAnswer || command.RequestID != "req-1" || command.ThreadID != "thread-1" || command.TurnID != "turn-1" {
					t.Fatalf("decline control = %#v", command)
				}
				if answer := command.Answers["approach"]; len(answer) != 1 || !strings.Contains(answer[0], "could not show this question") {
					t.Fatalf("decline answers = %#v", command.Answers)
				}
				if err := command.Check(t.Context()); err != nil {
					t.Fatalf("decline check = %v", err)
				}
				fixtureSetNow(execution.scheduler.host.(*Scheduler), time.Now().Add(2*time.Hour))
				if err := command.Check(t.Context()); !errors.Is(err, runner.ErrStaleConversationControl) {
					t.Fatalf("decline check after lease loss = %v, want stale", err)
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("the dropped question was never declined to the provider")
			}
		})
	}
}

func TestNativeBindConversationDoesNotHoldTheRun(t *testing.T) {
	for _, test := range []struct {
		name    string
		script  func(*conversationHub)
		wantErr error
		timeout time.Duration
	}{
		{name: "hub never answers", script: func(h *conversationHub) { h.bindHang = true }, wantErr: context.DeadlineExceeded, timeout: 100 * time.Millisecond},
		{name: "no conversation", script: func(h *conversationHub) { h.bindStatus, h.bindCode = http.StatusNotFound, "not_found" }, wantErr: runner.ErrNoConversation},
		{name: "bind uses native request budget", script: func(h *conversationHub) { h.bindDelay = 2100 * time.Millisecond }, timeout: 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, execution := newConversationHub(t, test.timeout)
			hub.mu.Lock()
			test.script(hub)
			hub.mu.Unlock()
			started := time.Now()
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
			if elapsed := time.Since(started); elapsed > max(2*time.Second, test.timeout+time.Second) {
				t.Fatalf("bind held the run for %s", elapsed)
			}
			if !errors.Is(err, test.wantErr) || (session != nil) != (test.wantErr == nil) {
				t.Fatalf("bind = %v, %v, want %v", session, err, test.wantErr)
			}
			if session != nil {
				if session.ConversationID() != "conv_1" {
					t.Fatalf("bound another conversation: %s", session.ConversationID())
				}
				if err := session.Close(t.Context(), runner.ConversationOutcomeSucceeded, nil); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNativeConversationBindsBeforeProviderStart(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Resume with current effort")
	h.claim(t, issue.ID)
	execution, ok := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	if !ok {
		t.Fatal("missing native execution")
	}
	client := execution.claim.source.NativeClient().Transport()
	next := fixtureClient(client).requests.Load()
	var registered, earlyPoll atomic.Bool
	polled := make(chan struct{})
	var once sync.Once
	fixtureClient(client).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/controls") {
			if !registered.Load() {
				earlyPoll.Store(true)
			}
			once.Do(func() {
				close(polled)
			})
		}
		response, err := next.RoundTrip(request)
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") && err == nil && response.StatusCode == http.StatusOK {
			registered.Store(true)
		}
		return response, err
	}))
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if err := session.Close(closeCtx, runner.ConversationOutcomeFailed, nil); err != nil {
			t.Error(err)
		}
	})
	identity := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example-model", ReasoningEffort: "high"}
	if err := execution.Start(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polled:
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("conversation polling did not begin after provider registration")
	}
	if earlyPoll.Load() {
		t.Fatal("conversation polled before the final provider identity was registered")
	}
	recovery, err := h.native.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Identity == nil || *recovery.Attempts[0].Identity != identity {
		t.Fatalf("persisted attempt identity: attempts=%+v error=%v", recovery.Attempts, err)
	}
}

func TestNativeBindConversationSendsTheResumeThread(t *testing.T) {
	for _, test := range []struct {
		name   string
		thread string
		want   string
	}{
		{name: "fresh run", thread: "", want: ""},
		{name: "local resume thread", thread: " thread-local ", want: "thread-local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, execution := newConversationHub(t)
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, test.thread)
			if err != nil {
				t.Fatalf("bind error = %v", err)
			}
			_ = session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil)
			hub.mu.Lock()
			defer hub.mu.Unlock()
			if len(hub.binds) != 1 || hub.binds[0].ThreadID != test.want {
				t.Fatalf("bind requests = %#v, want thread %q", hub.binds, test.want)
			}
		})
	}
}

// The hub refuses turn events once a turn has ended, so a control's result
// must be posted before the turn's end, however late its waiter runs.

func TestNativeConversationResumeMode(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		want     string
		ordinary bool
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
			name:     "ordinary worker binding",
			body:     `{"conversation_id":"conv_1","continuation":false,"resume":{},"pending":[],"cursor":0}`,
			ordinary: true,
		},
		{
			name: "explicit continuation",
			body: `{"conversation_id":"conv_1","continuation":true,"resume":{},"pending":[],"cursor":0}`,
		},
		{
			name: "unknown continuation retains conservative owner",
			body: `{"conversation_id":"conv_1","continuation":null,"resume":{},"pending":[],"cursor":0}`,
		},
		{
			name: "nothing to resume",
			body: `{"conversation_id":"conv_1","resume":{"thread_id":"","transcript":[]},"pending":[],"cursor":0}`,
			want: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, execution := newConversationHub(t)
			hub.bindBody = test.body
			session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
			if err != nil {
				t.Fatalf("bind error = %v", err)
			}
			defer func() {
				if err := session.Close(context.Background(), runner.ConversationOutcomeSucceeded, nil); err != nil {
					t.Errorf("close error = %v", err)
				}
			}()
			if session.Continuation() == test.ordinary || execution.conversationContinuation != session.Continuation() {
				t.Fatalf("bound continuation ownership diverged: session=%t execution=%t ordinary=%t", session.Continuation(), execution.conversationContinuation, test.ordinary)
			}
			if got := session.ResumeMode(); got != test.want {
				t.Fatalf("resume mode = %q, want %q", got, test.want)
			}
		})
	}
}
