package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var ErrInvestigationBudget = errors.New("investigation tool budget exhausted")

const InvestigationBudgetMessage = "Investigation tool budget exhausted. Further tool calls are refused. Return your final decision JSON now using the evidence already gathered, acknowledge missing or truncated evidence, and stay within the remaining deadline."

type AgentInvestigationBackend interface {
	SupportsReadOnlyInvestigation() bool
}

type investigationItem struct {
	started, completed, handled bool
}

type AgentToolBudget struct {
	mu                 sync.Mutex
	calls, bytes       int
	maxCalls, maxBytes int
	err                error
	tools              map[string]bool
	items              map[string]investigationItem
}

func NewAgentToolBudget(maxCalls, maxBytes int) *AgentToolBudget {
	return &AgentToolBudget{maxCalls: maxCalls, maxBytes: maxBytes, tools: make(map[string]bool), items: make(map[string]investigationItem)}
}

func (b *AgentToolBudget) consume(calls, bytes int) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consumeLocked(calls, bytes)
}

func (b *AgentToolBudget) consumeLocked(calls, bytes int) error {
	if b.err == nil && (calls > b.maxCalls-b.calls || bytes > b.maxBytes-b.bytes || (calls > 0 && b.bytes >= b.maxBytes)) {
		b.err = ErrInvestigationBudget
	}
	if b.err != nil {
		return b.err
	}
	b.calls += calls
	b.bytes += bytes
	return nil
}

func (b *AgentToolBudget) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err != nil || b.calls >= b.maxCalls || b.bytes >= b.maxBytes
}

func (b *AgentToolBudget) Observe(update AgentUpdate) error {
	if b == nil {
		return nil
	}
	if update.Type != AgentUpdateToolStarted && update.Type != AgentUpdateToolCompleted {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	key := update.ThreadID + "\x00" + update.TurnID + "\x00" + update.ItemID
	item := b.items[key]
	item.handled = item.handled || b.tools[update.Tool] || b.tools[strings.TrimPrefix(update.Tool, "mcp__detent__")]
	var calls, bytes int
	if !item.handled {
		if update.Type == AgentUpdateToolStarted && !item.started {
			calls = 1
		}
		if update.Type == AgentUpdateToolCompleted && !item.completed {
			bytes = len(update.Delta)
		}
	}
	item.started = item.started || update.Type == AgentUpdateToolStarted
	item.completed = item.completed || update.Type == AgentUpdateToolCompleted
	b.items[key] = item
	return b.consumeLocked(calls, bytes)
}

func (b *AgentToolBudget) Handler(tools []AgentTool, handle AgentToolHandler) AgentToolHandler {
	if handle == nil || b == nil {
		return handle
	}
	b.mu.Lock()
	for _, tool := range tools {
		b.tools[tool.Name] = true
	}
	b.mu.Unlock()
	bounded := b.handler(handle)
	return func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
		result, err := bounded(ctx, call)
		if errors.Is(err, ErrInvestigationBudget) {
			return AgentToolResult{Content: InvestigationBudgetMessage}, nil
		}
		return result, err
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
