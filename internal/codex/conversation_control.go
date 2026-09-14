package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

// conversationControlProgressMethod is the synthetic notification the wrapper
// hands back to the app-server read loop after consuming a control command so
// the loop's stall accounting observes progress. It carries no ID and is not a
// known update, so the app server ignores it.
const conversationControlProgressMethod = "conversation/controlProgress"

// conversationControlRequestIDBase starts the wrapper's own JSON-RPC request
// ID space well above the app server's fixed request IDs.
const conversationControlRequestIDBase = 1000

var errConversationTransportClosed = errors.New("conversation transport closed")

// optionalTransportCapabilities lists the optional transport interfaces the
// app server probes with type assertions. Both the local transport and the
// conversation wrapper must satisfy it so wrapping never hides a capability.
type optionalTransportCapabilities interface {
	ProcessIdentity() string
	WorkerProcess() procgroup.Identity
	MarkStartupReady(time.Time)
	StartupProcessEvidence() backendcapacity.StartupProcessEvidence
}

var (
	_ optionalTransportCapabilities = (*localTransport)(nil)
	_ optionalTransportCapabilities = (*conversationTransport)(nil)
	_ Transport                     = (*conversationTransport)(nil)
)

type conversationRead struct {
	message Message
	err     error
}

type pendingQuestion struct {
	message  Message
	deadline time.Time
}

// conversationTransport multiplexes live conversation controls into an active
// provider turn. The pump goroutine is the only reader of the wrapped
// transport; Receive remains the sole owner of command handling and protocol
// state, so the app server's single-goroutine read loop is preserved. Close
// cancels and joins the pump before returning and fails outstanding
// acknowledgements.
type conversationTransport struct {
	Transport

	runContext context.Context //nolint:containedctx // The pump reads under the run context and it replaces the caller deadline while a question is pending.
	cancel     context.CancelCauseFunc
	control    *runner.AgentConversationControl
	// tempDir is where a steer's image copies are written (section 17.1).
	tempDir         string
	logger          *slog.Logger
	questionTimeout time.Duration
	now             func() time.Time
	done            chan struct{}
	reads           chan conversationRead

	mu       sync.Mutex
	commands <-chan runner.AgentControl
	thread   string
	turn     string
	armed    bool
	ended    bool
	next     int
	pending  map[int]chan error
	requests map[string]pendingQuestion

	closeOnce sync.Once
	closeErr  error
}

func newConversationTransport(
	ctx context.Context,
	transport Transport,
	control *runner.AgentConversationControl,
	tempDir string,
	logger *slog.Logger,
) *conversationTransport {
	ctx, cancel := context.WithCancelCause(contextOrBackground(ctx))
	w := &conversationTransport{
		Transport:       transport,
		runContext:      ctx,
		cancel:          cancel,
		control:         control,
		tempDir:         tempDir,
		logger:          logger,
		questionTimeout: control.EffectiveQuestionTimeout(),
		now:             time.Now,
		done:            make(chan struct{}),
		reads:           make(chan conversationRead),
		commands:        control.Commands,
		next:            conversationControlRequestIDBase,
		pending:         make(map[int]chan error),
		requests:        make(map[string]pendingQuestion),
	}
	go w.pump(transport)
	return w
}

func (w *conversationTransport) pump(transport Transport) {
	defer close(w.done)
	for {
		message, err := transport.Receive(w.runContext)
		select {
		case w.reads <- conversationRead{message: message, err: err}:
		case <-w.runContext.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (w *conversationTransport) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.cancel(errConversationTransportClosed)
		w.closeErr = w.Transport.Close(ctx)
		<-w.done

		w.mu.Lock()
		defer w.mu.Unlock()
		for id, reply := range w.pending {
			reply <- fmt.Errorf("%w before acknowledgement", errConversationTransportClosed)
			delete(w.pending, id)
		}
	})
	return w.closeErr
}

// Send forwards to the wrapped transport and arms the binding on turn/start so
// the following turn/started (or turn/start response) for that thread binds the
// live turn.
func (w *conversationTransport) Send(ctx context.Context, m Message) error {
	if m.Method == "turn/start" {
		var params struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return fmt.Errorf("decode turn/start params: %w", err)
		}
		w.mu.Lock()
		w.thread = params.ThreadID
		w.turn = ""
		w.armed = true
		w.ended = false
		w.mu.Unlock()
	}
	return w.Transport.Send(ctx, m)
}

// Receive delivers the next provider message, interleaving control commands
// from the conversation. While a provider question is pending the caller's
// read deadline is suspended and replaced by the question timeout.
func (w *conversationTransport) Receive(ctx context.Context) (Message, error) {
	for {
		message, again, err := w.receiveOnce(ctx)
		if err != nil {
			return Message{}, err
		}
		if again {
			continue
		}
		return message, nil
	}
}

func (w *conversationTransport) receiveOnce(ctx context.Context) (Message, bool, error) {
	if err := w.closedErr(); err != nil {
		return Message{}, false, err
	}
	receiveContext := ctx
	var expiry <-chan time.Time
	w.mu.Lock()
	commands := w.commands
	if w.turn == "" && !w.ended {
		// The provider has not reported the turn yet: initialize and thread
		// start run under this wrapper. A control consumed now could only be
		// rejected as stale, so leave it queued until the turn is live. Once
		// the turn has ended the control is consumed and refused instead, so
		// the user learns straight away that it arrived too late.
		commands = nil
	}
	if deadline, ok := w.earliestQuestionDeadlineLocked(); ok {
		receiveContext = w.runContext
		timer := time.NewTimer(deadline.Sub(w.now()))
		defer timer.Stop()
		expiry = timer.C
	}
	w.mu.Unlock()

	select {
	case <-receiveContext.Done():
		return Message{}, false, receiveContext.Err()
	case <-w.done:
		return Message{}, false, w.closedErr()
	case <-expiry:
		return Message{}, false, runner.ErrConversationQuestionExpired
	case command, ok := <-commands:
		if !ok {
			w.mu.Lock()
			w.commands = nil
			w.mu.Unlock()
			return Message{}, true, nil
		}
		// A close that raced the select wins: the command is failed rather
		// than written to a transport that is already gone.
		if err := w.closedErr(); err != nil {
			replyControl(command, err)
			return Message{}, false, err
		}
		replyControl(command, w.sendControl(receiveContext, command))
		return Message{Method: conversationControlProgressMethod}, false, nil
	case read := <-w.reads:
		if read.err != nil {
			return Message{}, false, read.err
		}
		return w.observe(read.message)
	}
}

// closedErr reports the close cause once the pump has exited, or nil while
// the transport is still open.
func (w *conversationTransport) closedErr() error {
	select {
	case <-w.done:
		if cause := context.Cause(w.runContext); cause != nil {
			return cause
		}
		return errConversationTransportClosed
	default:
		return nil
	}
}

// replyControl delivers a rejection to a command's reply channel when the
// command was rejected before registering a pending acknowledgement. Commands
// without a usable reply channel are dropped silently: Validate already
// rejected them and there is nowhere to report it.
func replyControl(c runner.AgentControl, err error) {
	if err == nil || c.Reply == nil || cap(c.Reply) < 1 {
		return
	}
	c.Reply <- err
}

func (w *conversationTransport) earliestQuestionDeadlineLocked() (time.Time, bool) {
	var earliest time.Time
	for _, question := range w.requests {
		if earliest.IsZero() || question.deadline.Before(earliest) {
			earliest = question.deadline
		}
	}
	return earliest, !earliest.IsZero()
}

// observe applies protocol state from an inbound message and reports whether
// the message was consumed (again=true) or should be handed to the caller.
func (w *conversationTransport) observe(m Message) (Message, bool, error) {
	if m.Method == "" {
		return w.observeResponse(m)
	}
	switch m.Method {
	case "turn/started":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return Message{}, false, fmt.Errorf("decode turn/started params: %w", err)
		}
		w.bindArmed(params.ThreadID, params.Turn.ID)
	case "item/tool/requestUserInput":
		return w.observeUserInputRequest(m)
	case "turn/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return Message{}, false, fmt.Errorf("decode turn/completed params: %w", err)
		}
		w.mu.Lock()
		if params.ThreadID == w.thread && params.Turn.ID == w.turn {
			w.turn = ""
			w.armed = false
			w.ended = true
			clear(w.requests)
		}
		w.mu.Unlock()
	}
	return m, false, nil
}

func (w *conversationTransport) observeResponse(m Message) (Message, bool, error) {
	if len(m.Result) == 0 && m.Error == nil {
		return m, false, nil
	}
	var responseID int
	if err := json.Unmarshal(m.ID, &responseID); err != nil {
		return m, false, nil
	}
	w.mu.Lock()
	reply, ok := w.pending[responseID]
	if ok {
		delete(w.pending, responseID)
	}
	w.mu.Unlock()
	if ok {
		var err error
		if m.Error != nil {
			err = fmt.Errorf("provider rejected control: %s", m.Error.Message)
		}
		reply <- err
		return Message{}, true, nil
	}
	if responseID == turnStartRequestID && len(m.Result) > 0 {
		var result struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(m.Result, &result); err == nil && result.Turn.ID != "" {
			w.mu.Lock()
			thread := w.thread
			w.mu.Unlock()
			w.bindArmed(thread, result.Turn.ID)
		}
	}
	return m, false, nil
}

func (w *conversationTransport) observeUserInputRequest(m Message) (Message, bool, error) {
	var params struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return Message{}, false, fmt.Errorf("decode requestUserInput params: %w", err)
	}
	id := string(m.ID)

	w.mu.Lock()
	thread, turn := w.thread, w.turn
	matches := params.ThreadID == thread && params.TurnID == turn && w.control.InputRequested != nil
	if matches {
		w.requests[id] = pendingQuestion{message: m, deadline: w.now().Add(w.questionTimeout)}
	}
	w.mu.Unlock()

	if !matches {
		// Fall through to the app server's empty-answer handling; only a
		// question for the bound turn is held for a human.
		reason := "no input hook"
		if w.control.InputRequested != nil {
			reason = "thread or turn does not match the live binding"
		}
		w.warn("declining conversation question outside the live turn",
			"reason", reason,
			"requestId", id,
			"threadId", params.ThreadID,
			"turnId", params.TurnID,
			"boundThreadId", thread,
			"boundTurnId", turn,
		)
		return m, false, nil
	}

	err := w.control.InputRequested(runner.AgentInputRequest{
		ID:        id,
		ThreadID:  params.ThreadID,
		TurnID:    params.TurnID,
		Questions: params.Questions,
	})
	if err != nil {
		w.mu.Lock()
		delete(w.requests, id)
		w.mu.Unlock()
		return Message{}, false, fmt.Errorf("publish conversation question: %w", err)
	}
	return Message{}, true, nil
}

func (w *conversationTransport) bindArmed(thread string, turn string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.armed && thread == w.thread && w.turn == "" && turn != "" {
		w.turn = turn
		w.armed = false
	}
}

// bind sets the live thread and turn directly. It exists for tests that do
// not drive the turn/start handshake.
func (w *conversationTransport) bind(thread string, turn string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.thread, w.turn, w.armed, w.ended = thread, turn, false, false
}

// sendControl validates, re-checks ownership, and writes one control to the
// provider. The lock is held across the check and the write so the binding
// cannot change between them.
func (w *conversationTransport) sendControl(ctx context.Context, c runner.AgentControl) error {
	if err := c.Validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.turn == "" || c.ThreadID != w.thread || c.TurnID != w.turn {
		return runner.ErrStaleConversationControl
	}
	if err := c.Check(ctx); err != nil {
		return err
	}
	switch c.Kind {
	case runner.AgentControlAnswer:
		return w.sendAnswerLocked(ctx, c)
	case runner.AgentControlMessage:
		// A steer carries the same input shape as a turn start, so a file
		// dropped mid-turn reaches the provider the same way (section 17.1).
		input, cleanupInput := turnInputItems(c.Text, c.Attachments, w.tempDir)
		defer cleanupInput()
		params := map[string]any{
			"threadId":            w.thread,
			"expectedTurnId":      w.turn,
			"clientUserMessageId": c.MessageID,
			"input":               input,
		}
		return w.sendRequestLocked(ctx, "turn/steer", params, c.Reply)
	case runner.AgentControlInterrupt:
		params := map[string]any{"threadId": w.thread, "turnId": w.turn}
		return w.sendRequestLocked(ctx, "turn/interrupt", params, c.Reply)
	default:
		return fmt.Errorf("%w: kind %q", runner.ErrUnsupportedConversationControl, c.Kind)
	}
}

func (w *conversationTransport) sendRequestLocked(ctx context.Context, method string, params map[string]any, reply chan error) error {
	w.next++
	if err := sendRequest(ctx, w.Transport, w.next, method, params); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}
	w.pending[w.next] = reply
	return nil
}

func (w *conversationTransport) sendAnswerLocked(ctx context.Context, c runner.AgentControl) error {
	request, ok := w.requests[c.RequestID]
	if !ok {
		return runner.ErrStaleConversationControl
	}
	answers := make(map[string]any, len(c.Answers))
	for key, values := range c.Answers {
		answers[key] = map[string]any{"answers": values}
	}
	result, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		return fmt.Errorf("marshal conversation answers: %w", err)
	}
	if err := w.Transport.Send(ctx, Message{ID: request.message.ID, Result: result}); err != nil {
		return fmt.Errorf("send conversation answer: %w", err)
	}
	delete(w.requests, c.RequestID)
	c.Reply <- nil // Write receipt only; the provider gives no answer acknowledgement.
	return nil
}

func (w *conversationTransport) warn(msg string, args ...any) {
	if w.logger == nil {
		return
	}
	w.logger.Warn(msg, args...)
}

func (w *conversationTransport) ProcessIdentity() string {
	return transportProcessIdentity(w.Transport)
}

func (w *conversationTransport) WorkerProcess() procgroup.Identity {
	return transportWorkerProcess(w.Transport)
}

func (w *conversationTransport) MarkStartupReady(readyAt time.Time) {
	markTransportReady(w.Transport, readyAt)
}

func (w *conversationTransport) StartupProcessEvidence() backendcapacity.StartupProcessEvidence {
	provider, ok := w.Transport.(interface {
		StartupProcessEvidence() backendcapacity.StartupProcessEvidence
	})
	if !ok {
		return backendcapacity.StartupProcessEvidence{}
	}
	return provider.StartupProcessEvidence()
}
