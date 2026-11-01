package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

// conversationWire is a context-aware transport double. It records provider
// writes and asserts the single-reader contract: the wrapper must never have
// more than one Receive outstanding against the underlying transport.
type conversationWire struct {
	incoming chan Message
	mu       sync.Mutex
	sent     []Message

	activeReceives  atomic.Int32
	maxReceives     atomic.Int32
	receiveCalls    atomic.Int32
	closeCalls      atomic.Int32
	closeErr        error
	processIdentity string
	// onSend, when set, observes every provider write; doubles use it to
	// release follow-up frames only after the wrapper has answered.
	onSend func(Message)
}

func newConversationWire(buffer int) *conversationWire {
	return &conversationWire{incoming: make(chan Message, buffer)}
}

func (w *conversationWire) Send(_ context.Context, m Message) error {
	w.mu.Lock()
	w.sent = append(w.sent, m)
	onSend := w.onSend
	w.mu.Unlock()
	if onSend != nil {
		onSend(m)
	}
	return nil
}

func (w *conversationWire) Receive(ctx context.Context) (Message, error) {
	w.receiveCalls.Add(1)
	active := w.activeReceives.Add(1)
	defer w.activeReceives.Add(-1)
	for {
		current := w.maxReceives.Load()
		if active <= current || w.maxReceives.CompareAndSwap(current, active) {
			break
		}
	}
	select {
	case m := <-w.incoming:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (w *conversationWire) Close(context.Context) error {
	w.closeCalls.Add(1)
	return w.closeErr
}

func (w *conversationWire) ProcessIdentity() string { return w.processIdentity }

func (w *conversationWire) sentMessages() []Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Message(nil), w.sent...)
}

func conversationFrame(method string, id int, params string) Message {
	m := Message{Method: method, Params: json.RawMessage(params)}
	if id > 0 {
		m.ID = requestID(id)
	}
	return m
}

func passingCheck(context.Context) error { return nil }

func closeConversationTransport(t *testing.T, w *conversationTransport) {
	t.Helper()
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestConversationControlCorrelatesRequestsAndWaitsForHuman(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wire := newConversationWire(8)
	commands := make(chan runner.AgentControl, 4)
	questions := make(chan runner.AgentInputRequest, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{
		Commands:       commands,
		InputRequested: func(q runner.AgentInputRequest) error { questions <- q; return nil },
	}, "", nil)
	defer closeConversationTransport(t, w)

	if err := w.Send(ctx, conversationFrame("turn/start", 3, `{"threadId":"primary"}`)); err != nil {
		t.Fatal(err)
	}
	wire.incoming <- conversationFrame("turn/started", 0, `{"threadId":"primary","turn":{"id":"turn"}}`)
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	wire.incoming <- conversationFrame("turn/started", 0, `{"threadId":"auxiliary","turn":{"id":"other"}}`)
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	steer := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "primary", TurnID: "turn", MessageID: "m1", Text: "follow up", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- steer
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	// Server requests and client replies have independent numeric ID spaces.
	wire.incoming <- conversationFrame("item/tool/requestUserInput", 1001, `{"threadId":"primary","turnId":"turn","questions":[{"id":"q"}]}`)
	received := make(chan error, 1)
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	go func() { _, err := w.Receive(short); received <- err }()
	var question runner.AgentInputRequest
	select {
	case question = <-questions:
	case <-ctx.Done():
		t.Fatal("question swallowed by overlapping response ID")
	}
	if question.ID != "1001" || question.ThreadID != "primary" || question.TurnID != "turn" || string(question.Questions) != `[{"id":"q"}]` {
		t.Fatalf("unexpected question: %+v", question)
	}
	select {
	case <-steer.Reply:
		t.Fatal("server request falsely acknowledged steer")
	default:
	}
	<-short.Done() // Human answers after the ordinary read deadline.
	answer := runner.AgentControl{Kind: runner.AgentControlAnswer, ThreadID: "primary", TurnID: "turn", RequestID: "1001", Answers: map[string][]string{"q": {"yes"}}, Check: passingCheck, Reply: make(chan error, 1)}
	commands <- answer
	if err := <-received; err != nil {
		t.Fatalf("human wait inherited read deadline: %v", err)
	}
	if err := <-answer.Reply; err != nil {
		t.Fatal(err)
	}
	wire.incoming <- Message{ID: requestID(1001), Result: json.RawMessage(`{}`)}
	wire.incoming <- Message{Method: "test/tick"}
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-steer.Reply; err != nil {
		t.Fatal(err)
	}
	duplicate := runner.AgentControl{Kind: runner.AgentControlAnswer, ThreadID: "primary", TurnID: "turn", RequestID: "1001", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- duplicate
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(<-duplicate.Reply, runner.ErrStaleConversationControl) {
		t.Fatal("duplicate answer accepted")
	}

	sent := wire.sentMessages()
	if len(sent) != 3 {
		t.Fatalf("unexpected provider writes: %d", len(sent))
	}
	assertRequest(t, sent[0], 3, "turn/start")
	assertRequest(t, sent[1], 1001, "turn/steer")
	assertJSONContains(t, sent[1].Params, "threadId", "primary")
	assertJSONContains(t, sent[1].Params, "expectedTurnId", "turn")
	assertJSONContains(t, sent[1].Params, "clientUserMessageId", "m1")
	assertResponseID(t, sent[2], 1001)
	assertJSONContains(t, sent[2].Result, "answers.q.answers", []any{"yes"})
	if got := wire.maxReceives.Load(); got != 1 {
		t.Fatalf("concurrent underlying Receive calls = %d, want exactly one reader", got)
	}
}

func TestConversationControlRechecksBeforeWriting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	wire := newConversationWire(1)
	commands := make(chan runner.AgentControl, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
	defer closeConversationTransport(t, w)
	w.bind("thread", "turn")

	stale := errors.New("stale owner")
	cmd := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Reply: make(chan error, 1), Check: func(context.Context) error { return stale }}
	commands <- cmd
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(<-cmd.Reply, stale) {
		t.Fatal("ownership error lost")
	}
	if sent := wire.sentMessages(); len(sent) != 0 {
		t.Fatal("stale command written")
	}
}

func TestConversationControlRejectsInvalidCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		control runner.AgentControl
		wantErr error
	}{
		{name: "unbuffered reply is dropped", control: runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Check: passingCheck, Reply: make(chan error)}},
		{name: "missing check", control: runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Reply: make(chan error, 1)}, wantErr: runner.ErrInvalidConversationControl},
		{name: "unsupported kind", control: runner.AgentControl{Kind: runner.AgentControlKind("steer"), ThreadID: "thread", TurnID: "turn", Check: passingCheck, Reply: make(chan error, 1)}, wantErr: runner.ErrUnsupportedConversationControl},
		{name: "wrong turn", control: runner.AgentControl{Kind: runner.AgentControlInterrupt, ThreadID: "thread", TurnID: "other", Check: passingCheck, Reply: make(chan error, 1)}, wantErr: runner.ErrStaleConversationControl},
		{name: "wrong thread", control: runner.AgentControl{Kind: runner.AgentControlInterrupt, ThreadID: "other", TurnID: "turn", Check: passingCheck, Reply: make(chan error, 1)}, wantErr: runner.ErrStaleConversationControl},
		{name: "unknown request", control: runner.AgentControl{Kind: runner.AgentControlAnswer, ThreadID: "thread", TurnID: "turn", RequestID: "9", Check: passingCheck, Reply: make(chan error, 1)}, wantErr: runner.ErrStaleConversationControl},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			wire := newConversationWire(1)
			commands := make(chan runner.AgentControl, 1)
			w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
			defer closeConversationTransport(t, w)
			w.bind("thread", "turn")

			commands <- tt.control
			msg, err := w.Receive(ctx)
			if err != nil {
				t.Fatalf("Receive() error = %v", err)
			}
			if msg.Method != conversationControlProgressMethod {
				t.Fatalf("Receive() method = %q, want %q", msg.Method, conversationControlProgressMethod)
			}
			if tt.wantErr != nil {
				select {
				case got := <-tt.control.Reply:
					if !errors.Is(got, tt.wantErr) {
						t.Fatalf("reply = %v, want %v", got, tt.wantErr)
					}
				case <-ctx.Done():
					t.Fatal("reply not delivered")
				}
			}
			if sent := wire.sentMessages(); len(sent) != 0 {
				t.Fatalf("invalid command written: %+v", sent)
			}
		})
	}
}

func TestConversationControlInterruptAndProviderRejection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	wire := newConversationWire(2)
	commands := make(chan runner.AgentControl, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
	defer closeConversationTransport(t, w)
	w.bind("thread", "turn")

	interrupt := runner.AgentControl{Kind: runner.AgentControlInterrupt, ThreadID: "thread", TurnID: "turn", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- interrupt
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	sent := wire.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("provider writes = %d, want 1", len(sent))
	}
	assertRequest(t, sent[0], 1001, "turn/interrupt")
	assertJSONContains(t, sent[0].Params, "threadId", "thread")
	assertJSONContains(t, sent[0].Params, "turnId", "turn")

	wire.incoming <- Message{ID: requestID(1001), Error: &RPCError{Code: -1, Message: "turn already finished"}}
	wire.incoming <- Message{Method: "test/tick"}
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	err := <-interrupt.Reply
	if err == nil || !strings.Contains(err.Error(), "turn already finished") {
		t.Fatalf("provider rejection lost: %v", err)
	}
}

func TestConversationControlTurnCompletedClearsBinding(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	wire := newConversationWire(2)
	commands := make(chan runner.AgentControl, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
	defer closeConversationTransport(t, w)
	w.bind("thread", "turn")

	wire.incoming <- conversationFrame("turn/completed", 0, `{"threadId":"thread","turn":{"id":"turn"}}`)
	msg, err := w.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "turn/completed" {
		t.Fatalf("Receive() method = %q, want turn/completed to pass through", msg.Method)
	}
	late := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: "late", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- late
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(<-late.Reply, runner.ErrStaleConversationControl) {
		t.Fatal("command accepted after turn completed")
	}
}

func TestConversationControlBindsFromTurnStartResponse(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	wire := newConversationWire(2)
	commands := make(chan runner.AgentControl, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
	defer closeConversationTransport(t, w)

	if err := w.Send(ctx, conversationFrame("turn/start", turnStartRequestID, `{"threadId":"thread"}`)); err != nil {
		t.Fatal(err)
	}
	wire.incoming <- Message{ID: requestID(turnStartRequestID), Result: json.RawMessage(`{"turn":{"id":"turn"}}`)}
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	steer := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: "go", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- steer
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-steer.Reply:
		t.Fatalf("steer rejected before provider acknowledgement: %v", err)
	default:
	}
	sent := wire.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("provider writes = %d, want turn/start and turn/steer", len(sent))
	}
	assertRequest(t, sent[1], 1001, "turn/steer")
}

func TestConversationControlQuestionTimeoutExpires(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wire := newConversationWire(2)
	asked := make(chan runner.AgentInputRequest, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{
		Commands:        make(chan runner.AgentControl),
		InputRequested:  func(q runner.AgentInputRequest) error { asked <- q; return nil },
		QuestionTimeout: 30 * time.Millisecond,
	}, "", nil)
	defer closeConversationTransport(t, w)
	w.bind("thread", "turn")

	wire.incoming <- conversationFrame("item/tool/requestUserInput", 7, `{"threadId":"thread","turnId":"turn","questions":[]}`)
	started := time.Now()
	_, err := w.Receive(ctx)
	if !errors.Is(err, runner.ErrConversationQuestionExpired) {
		t.Fatalf("Receive() error = %v, want %v", err, runner.ErrConversationQuestionExpired)
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
		t.Fatalf("question expired after %s, before the configured timeout", elapsed)
	}
	if ctx.Err() != nil {
		t.Fatal("caller context expired instead of the question timeout")
	}
	select {
	case <-asked:
	default:
		t.Fatal("InputRequested hook not invoked")
	}
}

func TestConversationControlInputRequestedErrorFailsTurn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	wire := newConversationWire(1)
	hookErr := errors.New("hub unavailable")
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{
		InputRequested: func(runner.AgentInputRequest) error { return hookErr },
	}, "", nil)
	defer closeConversationTransport(t, w)
	w.bind("thread", "turn")

	wire.incoming <- conversationFrame("item/tool/requestUserInput", 7, `{"threadId":"thread","turnId":"turn"}`)
	if _, err := w.Receive(ctx); !errors.Is(err, hookErr) {
		t.Fatalf("Receive() error = %v, want %v", err, hookErr)
	}
}

func TestConversationControlMismatchedQuestionFallsThrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		control *runner.AgentConversationControl
		params  string
		wantLog string
	}{
		{
			name:    "other thread",
			control: &runner.AgentConversationControl{Commands: make(chan runner.AgentControl), InputRequested: func(runner.AgentInputRequest) error { return errors.New("must not be asked") }},
			params:  `{"threadId":"thread-2","turnId":"turn-1"}`,
			wantLog: "thread-2",
		},
		{
			name:    "other turn",
			control: &runner.AgentConversationControl{Commands: make(chan runner.AgentControl), InputRequested: func(runner.AgentInputRequest) error { return errors.New("must not be asked") }},
			params:  `{"threadId":"thread-1","turnId":"turn-9"}`,
			wantLog: "turn-9",
		},
		{
			name:    "no hook",
			control: &runner.AgentConversationControl{Commands: make(chan runner.AgentControl)},
			params:  `{"threadId":"thread-1","turnId":"turn-1"}`,
			wantLog: "no input hook",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			transport := newFakeAppServerTransport([]Message{
				responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
				responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
				responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
				responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
				notificationMessage(t, "turn/started", `{"threadId":"thread-1","turn":{"id":"turn-1"}}`),
				serverRequestMessage(t, 40, "item/tool/requestUserInput", tt.params),
				notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
			})
			server, err := NewAppServer(staticTransportFactory{transport: transport},
				WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
				WithReadTimeout(time.Second),
				WithTurnTimeout(time.Second),
			)
			if err != nil {
				t.Fatalf("NewAppServer() error = %v", err)
			}
			result, err := server.RunTurn(t.Context(), RunTurnRequest{
				Workspace:           t.TempDir(),
				Prompt:              "ask",
				ConversationControl: tt.control,
			}, nil)
			if err != nil {
				t.Fatalf("RunTurn() error = %v, want the mismatched question to be declined", err)
			}
			if result.TurnID != "turn-1" {
				t.Fatalf("TurnID = %q, want turn-1", result.TurnID)
			}
			var declined *Message
			for _, msg := range transport.sentMessages() {
				if msg.Method == "" && string(msg.ID) == "40" {
					declined = &msg
					break
				}
			}
			if declined == nil {
				t.Fatalf("empty-answer response for request 40 not written: %+v", transport.sentMessages())
			}
			assertResponseResultContains(t, *declined, 40, "answers", map[string]any{})
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), tt.wantLog) {
				t.Fatalf("mismatched question not logged at warn with %q: %s", tt.wantLog, logs.String())
			}
		})
	}
}

func TestAgentBackendPassesConversationControl(t *testing.T) {
	t.Parallel()

	// A real app-server blocks the turn on requestUserInput until it is
	// answered, so the double releases turn/completed only after the answer
	// response has been written.
	transport := newConversationWire(8)
	for _, msg := range []Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		serverRequestMessage(t, 40, "item/tool/requestUserInput", `{"threadId":"thread-1","turnId":"turn-1","questions":[{"id":"q"}]}`),
	} {
		transport.incoming <- msg
	}
	transport.onSend = func(msg Message) {
		if msg.Method == "" && string(msg.ID) == "40" {
			transport.incoming <- notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)
		}
	}
	server, err := NewAppServer(staticTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}
	backend, err := NewAgentBackend(server, Options{ApprovalPolicy: "never"})
	if err != nil {
		t.Fatalf("NewAgentBackend() error = %v", err)
	}
	var live runner.AgentLiveBackend = backend
	if !live.SupportsLiveControl() {
		t.Fatal("SupportsLiveControl() = false, want true")
	}
	commands := make(chan runner.AgentControl, 1)
	asked := make(chan runner.AgentInputRequest, 1)
	answered := make(chan error, 1)
	_, err = backend.RunTurn(t.Context(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		TempDir:   t.TempDir(),
		Prompt:    "Ship it",
		ConversationControl: &runner.AgentConversationControl{
			Commands: commands,
			InputRequested: func(q runner.AgentInputRequest) error {
				asked <- q
				commands <- runner.AgentControl{Kind: runner.AgentControlAnswer, ThreadID: q.ThreadID, TurnID: q.TurnID, RequestID: q.ID, Answers: map[string][]string{"q": {"ok"}}, Check: passingCheck, Reply: answered}
				return nil
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	select {
	case q := <-asked:
		if q.ID != "40" || q.ThreadID != "thread-1" || q.TurnID != "turn-1" {
			t.Fatalf("unexpected question: %+v", q)
		}
	default:
		t.Fatal("live control not wired through the backend: question hook never invoked")
	}
	if err := <-answered; err != nil {
		t.Fatalf("answer reply = %v", err)
	}
	var sawAnswer bool
	for _, msg := range transport.sentMessages() {
		if msg.Method == "" && string(msg.ID) == "40" {
			assertJSONContains(t, msg.Result, "answers.q.answers", []any{"ok"})
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Fatalf("human answer not written to the provider: %+v", transport.sentMessages())
	}
}

func TestConversationControlCloseJoinsPumpAndFailsPendingAcks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	wire := newConversationWire(1)
	commands := make(chan runner.AgentControl, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
	w.bind("thread", "turn")

	steer := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: "hi", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- steer
	if _, err := w.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	select {
	case <-w.done:
	default:
		t.Fatal("Close returned before the pump exited")
	}
	select {
	case err := <-steer.Reply:
		if err == nil {
			t.Fatal("pending acknowledgement resolved as success after close")
		}
	default:
		t.Fatal("pending acknowledgement not failed on close")
	}
	if got := wire.closeCalls.Load(); got != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", got)
	}
	if _, err := w.Receive(ctx); err == nil {
		t.Fatal("Receive() after Close succeeded")
	}
}

func TestConversationControlConcurrentSendReceiveClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wire := newConversationWire(64)
	commands := make(chan runner.AgentControl, 64)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{
		Commands:       commands,
		InputRequested: func(runner.AgentInputRequest) error { return nil },
	}, "", nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, err := w.Receive(ctx); err != nil {
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = w.Send(ctx, conversationFrame("turn/start", turnStartRequestID, `{"threadId":"thread"}`))
			select {
			case wire.incoming <- conversationFrame("turn/started", 0, `{"threadId":"thread","turn":{"id":"turn"}}`):
			case <-stop:
				return
			}
			select {
			case commands <- runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: "x", Check: passingCheck, Reply: make(chan error, 1)}:
			case <-stop:
				return
			}
			select {
			case wire.incoming <- conversationFrame("turn/completed", 0, `{"threadId":"thread","turn":{"id":"turn"}}`):
			case <-stop:
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	close(stop)
	wg.Wait()
	if got := wire.maxReceives.Load(); got != 1 {
		t.Fatalf("concurrent underlying Receive calls = %d, want exactly one reader", got)
	}
	if _, err := w.Receive(ctx); err == nil {
		t.Fatal("Receive() after Close succeeded")
	}
}

func TestConversationTransportForwardsCapabilities(t *testing.T) {
	t.Parallel()

	wire := newConversationWire(1)
	wire.processIdentity = "4242"
	w := newConversationTransport(t.Context(), wire, &runner.AgentConversationControl{}, "", nil)
	defer closeConversationTransport(t, w)

	if got := transportProcessIdentity(w); got != "4242" {
		t.Fatalf("ProcessIdentity() = %q, want 4242", got)
	}
	if got := transportWorkerProcess(w); got.PID != 0 {
		t.Fatalf("WorkerProcess() = %+v, want zero identity from a wire without one", got)
	}
	markTransportReady(w, time.Now())
	if evidence := w.StartupProcessEvidence(); evidence.Ready {
		t.Fatalf("StartupProcessEvidence() = %+v, want zero evidence from a wire without one", evidence)
	}
}

// A control that arrives before the provider reports the turn must stay
// queued: consuming it early would reject it permanently, because the
// binding has no turn to address yet.
func TestConversationControlQueuesControlsUntilTheTurnIsLive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wire := newConversationWire(2)
	commands := make(chan runner.AgentControl, 1)
	w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
	defer closeConversationTransport(t, w)

	if err := w.Send(ctx, conversationFrame("turn/start", turnStartRequestID, `{"threadId":"thread"}`)); err != nil {
		t.Fatal(err)
	}
	steer := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: "go", Check: passingCheck, Reply: make(chan error, 1)}
	commands <- steer

	// The turn has not started: the wrapper waits rather than consuming the
	// control and answering the caller with a progress frame.
	early, cancelEarly := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelEarly()
	if message, err := w.Receive(early); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive before the turn was live = %#v, %v; want the control left queued", message, err)
	}
	select {
	case err := <-steer.Reply:
		t.Fatalf("control settled before the turn was live: %v", err)
	default:
	}
	if sent := wire.sentMessages(); len(sent) != 1 {
		t.Fatalf("provider writes = %d, want only turn/start", len(sent))
	}

	wire.incoming <- conversationFrame("turn/started", 0, `{"threadId":"thread","turn":{"id":"turn"}}`)
	started, err := w.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if started.Method != "turn/started" {
		t.Fatalf("first message = %q, want turn/started", started.Method)
	}
	progress, err := w.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Method != conversationControlProgressMethod {
		t.Fatalf("second message = %q, want the control progress frame", progress.Method)
	}
	select {
	case err := <-steer.Reply:
		t.Fatalf("control rejected once the turn was live: %v", err)
	default:
	}
	sent := wire.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("provider writes = %d, want turn/start and turn/steer", len(sent))
	}
	assertRequest(t, sent[1], 1001, "turn/steer")
}

func TestConversationControlTranslatesAnswerIDs(t *testing.T) {
	t.Parallel()

	transport := newConversationWire(8)
	for _, msg := range []Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		serverRequestMessage(t, 40, "item/tool/requestUserInput", `{"threadId":"thread-1","turnId":"turn-1","questions":[{"id":"provider-id"}]}`),
	} {
		transport.incoming <- msg
	}
	transport.onSend = func(msg Message) {
		if msg.Method == "" && string(msg.ID) == "40" {
			transport.incoming <- notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)
		}
	}
	server, err := NewAppServer(staticTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}
	commands := make(chan runner.AgentControl, 1)
	answered := make(chan error, 1)
	_, err = server.RunTurn(t.Context(), RunTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "Ask",
		ConversationControl: &runner.AgentConversationControl{
			Commands: commands,
			InputRequested: func(q runner.AgentInputRequest) error {
				commands <- runner.AgentControl{Kind: runner.AgentControlAnswer, ThreadID: q.ThreadID, TurnID: q.TurnID, RequestID: q.ID, Answers: map[string][]string{"q1": {"ok"}}, Check: passingCheck, Reply: answered}
				return nil
			},
			TranslateAnswers: func(requestID string, answers map[string][]string) map[string][]string {
				if requestID != "40" {
					return answers
				}
				return map[string][]string{"provider-id": answers["q1"]}
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("answer reply = %v", err)
	}
	for _, msg := range transport.sentMessages() {
		if msg.Method == "" && string(msg.ID) == "40" {
			assertJSONContains(t, msg.Result, "answers.provider-id.answers", []any{"ok"})
			assertJSONOmits(t, msg.Result, "answers.q1")
			return
		}
	}
	t.Fatalf("answer not written to the provider: %+v", transport.sentMessages())
}

func TestConversationControlSteerCarriesAttachmentsUntilAcknowledged(t *testing.T) {
	t.Parallel()
	png := []byte("\x89PNG\r\n\x1a\nx")
	tests := []struct {
		name     string
		text     string
		files    []runner.AgentAttachment
		wantText []string
		images   int
		settle   func(t *testing.T, wire *conversationWire, w *conversationTransport, ctx context.Context)
	}{
		{
			name:     "text file rides as a data block",
			text:     "see notes",
			files:    []runner.AgentAttachment{{Name: "notes.md", MIME: "text/markdown", Content: []byte("lease log")}},
			wantText: []string{"see notes", "<attachments>", "notes.md", "lease log"},
		},
		{
			name:     "image copy outlives the write until the provider answers",
			text:     "look",
			files:    []runner.AgentAttachment{{Name: "shot.png", MIME: "image/png", Content: png}},
			wantText: []string{"look"},
			images:   1,
			settle: func(t *testing.T, wire *conversationWire, w *conversationTransport, ctx context.Context) {
				wire.incoming <- Message{ID: requestID(1001), Result: json.RawMessage(`{}`)}
				wire.incoming <- Message{Method: "test/tick"}
				if _, err := w.Receive(ctx); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "image copy is removed when the transport closes first",
			text:     "look",
			files:    []runner.AgentAttachment{{Name: "shot.png", MIME: "image/png", Content: png}},
			wantText: []string{"look"},
			images:   1,
			settle: func(t *testing.T, _ *conversationWire, w *conversationTransport, ctx context.Context) {
				if err := w.Close(ctx); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			wire := newConversationWire(4)
			commands := make(chan runner.AgentControl, 1)
			w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, t.TempDir(), nil)
			defer closeConversationTransport(t, w)
			w.bind("thread", "turn")
			steer := runner.AgentControl{Kind: runner.AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: test.text, Attachments: test.files, Check: passingCheck, Reply: make(chan error, 1)}
			commands <- steer
			if _, err := w.Receive(ctx); err != nil {
				t.Fatal(err)
			}
			sent := wire.sentMessages()
			if len(sent) != 1 {
				t.Fatalf("provider writes = %d, want 1", len(sent))
			}
			assertRequest(t, sent[0], 1001, "turn/steer")
			var params struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.Unmarshal(sent[0].Params, &params); err != nil {
				t.Fatal(err)
			}
			text, _ := params.Input[0]["text"].(string)
			for _, want := range test.wantText {
				if !strings.Contains(text, want) {
					t.Fatalf("steer text = %q, want it to contain %q", text, want)
				}
			}
			if len(params.Input) != 1+test.images {
				t.Fatalf("steer input = %#v, want %d images", params.Input, test.images)
			}
			if test.images == 0 {
				return
			}
			path, _ := params.Input[1]["path"].(string)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("image copy removed before the provider answered: %v", err)
			}
			test.settle(t, wire, w, ctx)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("image copy still present after the steer settled: %v", err)
			}
		})
	}
}

func TestConversationControlAddressesUnboundControlsToTheLiveTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		thread     string
		turn       string
		wantStale  bool
		wantThread string
		wantTurn   string
	}{
		{name: "accepted before any turn", wantThread: "thread-2", wantTurn: "turn-2"},
		{name: "resume thread without a turn", thread: "thread-old", wantThread: "thread-2", wantTurn: "turn-2"},
		{name: "a named earlier turn stays stale", thread: "thread-2", turn: "turn-1", wantStale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			wire := newConversationWire(2)
			commands := make(chan runner.AgentControl, 1)
			w := newConversationTransport(ctx, wire, &runner.AgentConversationControl{Commands: commands}, "", nil)
			defer closeConversationTransport(t, w)
			w.bind("thread-2", "turn-2")
			interrupt := runner.AgentControl{Kind: runner.AgentControlInterrupt, ThreadID: test.thread, TurnID: test.turn, Check: passingCheck, Reply: make(chan error, 1)}
			commands <- interrupt
			if _, err := w.Receive(ctx); err != nil {
				t.Fatal(err)
			}
			sent := wire.sentMessages()
			if test.wantStale {
				if err := <-interrupt.Reply; !errors.Is(err, runner.ErrStaleConversationControl) || len(sent) != 0 {
					t.Fatalf("reply = %v, writes = %d; want stale and nothing written", err, len(sent))
				}
				return
			}
			if len(sent) != 1 {
				t.Fatalf("provider writes = %d, want 1", len(sent))
			}
			assertRequest(t, sent[0], 1001, "turn/interrupt")
			assertJSONContains(t, sent[0].Params, "threadId", test.wantThread)
			assertJSONContains(t, sent[0].Params, "turnId", test.wantTurn)
		})
	}
}

type conversationTurnRecordingFactory struct {
	transport Transport
	marked    *atomic.Bool
}

func (f conversationTurnRecordingFactory) NewTransport(ctx context.Context) (Transport, error) {
	f.marked.Store(ConversationTurn(ctx))
	return f.transport, nil
}

// INV-2: only a turn bound to a live conversation starts its app-server with
// questions enabled; an ordinary issue-worker turn never does.
func TestAppServerMarksOnlyConversationTurns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		control *runner.AgentConversationControl
		want    bool
	}{
		{name: "ordinary issue run", want: false},
		{name: "conversation turn", control: &runner.AgentConversationControl{Commands: make(chan runner.AgentControl)}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := newFakeAppServerTransport([]Message{
				responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
				responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
				responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
				responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
				notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
			})
			var marked atomic.Bool
			server, err := NewAppServer(conversationTurnRecordingFactory{transport: transport, marked: &marked}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := server.RunTurn(t.Context(), RunTurnRequest{Workspace: t.TempDir(), Prompt: "work", ConversationControl: test.control}, nil); err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if marked.Load() != test.want {
				t.Fatalf("app-server started with questions enabled = %t, want %t", marked.Load(), test.want)
			}
		})
	}
	if ConversationTurn(context.Background()) || !ConversationTurn(WithConversationTurn(context.Background())) {
		t.Fatal("ConversationTurn does not reflect WithConversationTurn")
	}
}
