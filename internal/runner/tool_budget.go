package runner

import (
	"context"
	"errors"
	"sync"
)

var ErrInvestigationBudget = errors.New("investigation tool budget exhausted")

type AgentInvestigationBackend interface {
	SupportsReadOnlyInvestigation() bool
}

type AgentToolBudget struct {
	mu                 sync.Mutex
	calls, bytes       int
	maxCalls, maxBytes int
	cancel             context.CancelCauseFunc
	err                error
}

func NewAgentToolBudget(maxCalls, maxBytes int, cancel context.CancelCauseFunc) *AgentToolBudget {
	return &AgentToolBudget{maxCalls: maxCalls, maxBytes: maxBytes, cancel: cancel}
}

func (b *AgentToolBudget) consume(calls, bytes int) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil && (calls > b.maxCalls-b.calls || bytes > b.maxBytes-b.bytes) {
		b.err = ErrInvestigationBudget
		if b.cancel != nil {
			b.cancel(b.err)
		}
	}
	if b.err != nil {
		return b.err
	}
	b.calls += calls
	b.bytes += bytes
	return nil
}

func (b *AgentToolBudget) Observe(update AgentUpdate) error {
	if b == nil {
		return nil
	}
	switch update.Type {
	case AgentUpdateToolStarted:
		return b.consume(1, 0)
	case AgentUpdateToolOutput, AgentUpdateToolCompleted:
		return b.consume(0, len(update.Delta))
	default:
		return b.consume(0, 0)
	}
}

func (b *AgentToolBudget) handler(handle AgentToolHandler) AgentToolHandler {
	if handle == nil || b == nil {
		return handle
	}
	return func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
		if err := b.consume(1, 0); err != nil {
			return AgentToolResult{}, err
		}
		result, err := handle(ctx, call)
		if budgetErr := b.consume(0, len(result.Content)+len(result.ImageURL)); budgetErr != nil {
			return AgentToolResult{}, budgetErr
		}
		return result, err
	}
}
