package codex

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const toolRetirementTimeout = 5 * time.Second

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
	threadID   string
	turnID     string
	lifetime   *sessiontimeout.Session
}

func newToolTransport(ctx context.Context, transport Transport, handler DynamicToolHandler, logger *slog.Logger) *toolTransport {
	ctx, cancel := context.WithCancelCause(ctx)
	t := &toolTransport{Transport: transport, cancel: cancel, handler: handler, logger: logger, done: make(chan struct{}), wake: make(chan struct{}, 1)}
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

func (t *toolTransport) observeTurn(threadID, turnID string, lifetime *sessiontimeout.Session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.threadID, t.turnID, t.lifetime = threadID, turnID, lifetime
}

func (t *toolTransport) observeCall(msg Message) (*sessiontimeout.Session, string) {
	if t.lifetime == nil {
		return nil, ""
	}
	var scope struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if json.Unmarshal(msg.Params, &scope) != nil || scope.ThreadID != t.threadID || scope.TurnID != t.turnID {
		return nil, ""
	}
	id := scope.ThreadID + "/" + scope.TurnID + "/" + string(msg.ID)
	if !t.lifetime.Event("tool/call/" + id) {
		return nil, ""
	}
	return t.lifetime, id
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
		t.unresolved = true
		lifetime, callID := t.observeCall(msg)
		t.mu.Unlock()
		_, err := handleServerRequest(ctx, t, msg, t.handler, nil, t.logger)
		t.mu.Lock()
		if err == nil && ctx.Err() == nil {
			t.unresolved = false
			if lifetime != nil {
				lifetime.Event("tool/result/" + callID)
			}
		}
		t.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			t.cancel(err)
		}
	}
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
