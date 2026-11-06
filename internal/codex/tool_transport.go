package codex

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ToolError struct{ Err error }

func (e *ToolError) Error() string { return "dynamic tool call failed" }
func (e *ToolError) Unwrap() error { return e.Err }
func (e *ToolError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	return tracker.NativeTerminalFailure{Provider: "codex", Operation: "item/tool/call", ErrorClass: "tool_error", Attribution: "instance"}
}

type toolTransport struct {
	Transport
	receive func(context.Context) (Message, error)
	cancel  context.CancelCauseFunc
	handler DynamicToolHandler
	logger  *slog.Logger
	done    chan struct{}
	wake    chan struct{}
	sendMu  sync.Mutex
	mu      sync.Mutex
	pending []Message
	failure error
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

func (t *toolTransport) run(ctx context.Context) {
	defer close(t.done)
	for {
		t.mu.Lock()
		if len(t.pending) == 0 {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				t.mu.Lock()
				empty := len(t.pending) == 0
				t.mu.Unlock()
				if empty {
					return
				}
			case <-t.wake:
			}
			continue
		}
		msg := t.pending[0]
		t.pending[0] = Message{}
		t.pending = t.pending[1:]
		t.mu.Unlock()
		handler := func(ctx context.Context, call DynamicToolCall) (DynamicToolResult, error) {
			result, err := t.handler(ctx, call)
			if err != nil && ctx.Err() == nil {
				t.mu.Lock()
				t.failure = &ToolError{Err: err}
				t.mu.Unlock()
			}
			return result, err
		}
		_, err := handleServerRequest(ctx, t, msg, handler, nil, t.logger)
		if err != nil && ctx.Err() == nil {
			t.cancel(err)
		}
	}
}

func (t *toolTransport) finish(err error) error {
	t.cancel(nil)
	<-t.done
	if err == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failure != nil {
		var provider *TurnFailedError
		if errors.As(err, &provider) && provider.ProviderCode != "" {
			return errors.Join(err, t.failure)
		}
		return errors.Join(t.failure, err)
	}
	return err
}
