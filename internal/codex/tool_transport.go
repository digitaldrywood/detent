package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	dynamicToolTimeout    = 30 * time.Minute
	toolRetirementTimeout = 5 * time.Second
)

type ToolError struct{ Err error }

func (e *ToolError) Error() string { return "dynamic tool call failed" }
func (e *ToolError) Unwrap() error { return e.Err }
func (e *ToolError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	return tracker.NativeTerminalFailure{Provider: "codex", Operation: "item/tool/call", ErrorClass: "tool_error", Attribution: "instance"}
}

type toolTransport struct {
	Transport
	receive    func(context.Context) (Message, error)
	cancel     context.CancelCauseFunc
	handler    DynamicToolHandler
	logger     *slog.Logger
	done       chan struct{}
	wake       chan struct{}
	sendMu     sync.Mutex
	mu         sync.Mutex
	pending    []Message
	unresolved bool
	drain      bool
	lifetime   *sessiontimeout.Session
	turn       dynamicToolScope
	call       *runningDynamicTool
}

type dynamicToolScope struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type runningDynamicTool struct {
	ctx               context.Context
	scope             dynamicToolScope
	id                string
	finishObservation func()
	cancelObservation context.CancelFunc
}

func newToolTransport(ctx context.Context, transport Transport, handler DynamicToolHandler, logger *slog.Logger, lifetime *sessiontimeout.Session) *toolTransport {
	ctx, cancel := context.WithCancelCause(ctx)
	t := &toolTransport{Transport: transport, cancel: cancel, handler: handler, logger: logger, done: make(chan struct{}), wake: make(chan struct{}, 1), lifetime: lifetime}
	t.receive = func(readCtx context.Context) (Message, error) { return t.receiveMessage(ctx, readCtx) }
	go t.run(ctx)
	return t
}

func (t *toolTransport) Send(ctx context.Context, msg Message) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	return t.Transport.Send(ctx, msg)
}

func (t *toolTransport) Receive(ctx context.Context) (Message, error) { return t.receive(ctx) }

func (t *toolTransport) receiveMessage(toolCtx, ctx context.Context) (Message, error) {
	receiveCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(toolCtx, func() { cancel(context.Cause(toolCtx)) })
	defer stop()
	defer cancel(nil)
	for {
		if toolCtx.Err() != nil {
			return Message{}, context.Cause(toolCtx)
		}
		msg, err := t.Transport.Receive(receiveCtx)
		if err != nil {
			return Message{}, errors.Join(err, context.Cause(receiveCtx))
		}
		if msg.Method != "item/tool/call" || len(msg.ID) == 0 {
			return msg, nil
		}
		t.mu.Lock()
		t.pending = append(t.pending, msg)
		t.mu.Unlock()
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

func (t *toolTransport) observeTurn(threadID, turnID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	scope := dynamicToolScope{ThreadID: threadID, TurnID: turnID}
	if t.turn == scope {
		return
	}
	if t.call != nil && t.call.cancelObservation != nil {
		t.call.cancelObservation()
		t.call.finishObservation()
		t.call.cancelObservation, t.call.finishObservation = nil, nil
		t.unresolved = false
	}
	t.turn = scope
	t.observeCall()
}

func (t *toolTransport) observeCall() {
	call := t.call
	if call == nil || call.id == "" || call.scope != t.turn {
		return
	}
	ctx, cancel := context.WithCancel(call.ctx)
	finish, owned := t.lifetime.BeginOwnedCommand(ctx, call.id)
	if !owned {
		cancel()
		return
	}
	call.cancelObservation = cancel
	call.finishObservation = finish
	t.unresolved = true
}

func (t *toolTransport) run(ctx context.Context) {
	defer close(t.done)
	for {
		t.mu.Lock()
		if ctx.Err() != nil && (!t.drain || len(t.pending) == 0) {
			t.mu.Unlock()
			return
		}
		if len(t.pending) == 0 {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-t.wake:
			}
			continue
		}
		msg := t.pending[0]
		t.pending[0] = Message{}
		t.pending = t.pending[1:]
		t.mu.Unlock()
		err := t.handleCall(ctx, msg)
		t.mu.Lock()
		if err == nil && ctx.Err() == nil {
			t.unresolved = false
		}
		t.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			t.cancel(err)
		}
	}
}

func (t *toolTransport) handleCall(ctx context.Context, msg Message) error {
	callCtx, cancel := context.WithTimeout(ctx, dynamicToolTimeout)
	defer cancel()
	call := &runningDynamicTool{ctx: callCtx}
	if json.Unmarshal(msg.Params, &call.scope) == nil && call.scope.ThreadID != "" && call.scope.TurnID != "" {
		if requestID := dynamicToolRequestID(msg.ID); requestID != "" {
			call.id = fmt.Sprintf("dynamic/%q/%q/%s", call.scope.ThreadID, call.scope.TurnID, requestID)
		}
	}
	t.mu.Lock()
	t.call = call
	t.observeCall()
	t.mu.Unlock()
	handlerCtx := sessiontimeout.WithoutCommandObservation(callCtx)
	_, err := handleServerRequest(ctx, t, msg, func(_ context.Context, request DynamicToolCall) (DynamicToolResult, error) {
		return t.handler(handlerCtx, request)
	}, nil, t.logger)
	t.mu.Lock()
	if call.finishObservation != nil {
		call.finishObservation()
		call.cancelObservation()
	} else if err == nil && call.id != "" {
		t.lifetime.RememberOwnedCommandCompletion(call.id)
	}
	t.call = nil
	t.mu.Unlock()
	return err
}

func dynamicToolRequestID(raw json.RawMessage) string {
	if strings.HasPrefix(strings.TrimSpace(string(raw)), `"`) {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			return strconv.Quote(id)
		}
		return ""
	}
	var id *int64
	if json.Unmarshal(raw, &id) != nil || id == nil {
		return ""
	}
	return strconv.FormatInt(*id, 10)
}

func (t *toolTransport) finish(err error) error {
	t.mu.Lock()
	unresolved := t.unresolved
	t.drain = err == nil
	t.mu.Unlock()
	t.cancel(nil)
	timer := time.NewTimer(toolRetirementTimeout)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
		return errors.Join(err, runner.ErrProviderSessionUnretired)
	}
	if unresolved && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStreamStalled)) {
		var startup *StartupError
		var primary interface {
			NativeTerminalFailure() tracker.NativeTerminalFailure
		}
		if !errors.As(err, &startup) && !errors.As(err, &primary) {
			return &ToolError{Err: err}
		}
	}
	return err
}
