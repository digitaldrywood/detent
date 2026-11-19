package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/conversation"

	"github.com/digitaldrywood/detent/internal/runner"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

// Handles reports whether the hub-side coordinator owns the
// conversation's turns. Status is deliberately not consulted: a settled
// conversation with a pending message is woken by the message itself, and
// leaving its turn unanswered would strand the user.
func Handles(record conversations.ConversationRecord) bool {
	return record.WorkItemID == ""
}

var ErrRevision = errors.New("coordinator conversation changed")

// ErrLinked refuses a coordinator write to a conversation that
// was linked to an issue.
var ErrLinked = errors.New("coordinator: conversation is linked")

// callBackend runs the turn with coordination tools when the backend
// supports them and with a plain turn otherwise.
func CallBackend(ctx context.Context, backend runner.AgentBackend, request runner.AgentTurnRequest, stop <-chan struct{}, handle func(context.Context, runner.AgentUpdate), tools func() ([]runner.AgentTool, runner.AgentToolHandler)) (runner.AgentTurnResult, error) {
	var updateMu sync.Mutex
	finished := false
	onUpdate := func(update runner.AgentUpdate) error {
		updateMu.Lock()
		defer updateMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if finished {
			return context.Canceled
		}
		handle(ctx, update)
		return nil
	}
	defer func() {
		updateMu.Lock()
		finished = true
		updateMu.Unlock()
	}()
	type completion struct {
		result runner.AgentTurnResult
		err    error
	}
	done := make(chan completion, 1)
	go func() {
		var result runner.AgentTurnResult
		var err error
		if toolBackend, ok := backend.(runner.AgentToolBackend); ok {
			toolList, handleTool := tools()
			result, err = toolBackend.RunTurnWithTools(ctx, request, toolList, handleTool, onUpdate)
		} else {
			result, err = backend.RunTurn(ctx, request, onUpdate)
		}
		done <- completion{result: result, err: err}
	}()
	select {
	case result := <-done:
		return result.result, result.err
	case <-stop:
		return runner.AgentTurnResult{}, ctx.Err()
	}
}

func Outcome(runErr error, cancelled bool) conversation.Delivery {
	switch {
	case cancelled:
		return conversation.DeliveryInterrupted
	case runErr != nil:
		return conversation.DeliveryFailed
	default:
		return conversation.DeliveryCompleted
	}
}

func ToolSummary(update runner.AgentUpdate) string {
	name := strings.TrimSpace(update.Tool)
	if name == "" {
		name = "tool"
	}
	detail := strings.TrimSpace(update.Command)
	if detail == "" {
		detail = strings.TrimSpace(update.Status)
	}
	detail = strings.Join(strings.Fields(detail), " ")
	var summary string
	switch {
	case update.Type == runner.AgentUpdateToolStarted && detail != "":
		summary = "Running " + name + ": " + detail
	case update.Type == runner.AgentUpdateToolStarted:
		summary = "Running " + name
	case detail != "":
		summary = "Finished " + name + ": " + detail
	default:
		summary = "Finished " + name
	}
	return conversations.BoundRunes(summary, ToolSummaryRunes)
}

const ToolSummaryRunes = 500
