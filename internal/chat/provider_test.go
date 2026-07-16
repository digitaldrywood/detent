package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestAgentProviderRunsRestrictedToolTurnAndCollectsReply(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	backend := &agentToolBackendStub{}
	provider, err := NewAgentProvider(backend, workspacePath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewAgentProvider() error = %v", err)
	}
	response, err := provider.Reply(context.Background(), TurnRequest{
		ThreadID: "thread-existing",
		Prompt:   "What is blocked?",
		Tools:    Tools()[:1],
		Handle: func(_ context.Context, call ToolCall) (ToolResult, error) {
			if call.Name != "board_state" || string(call.Arguments) != `{"state":"Blocked"}` {
				t.Fatalf("tool call = %#v", call)
			}
			return ToolResult{Content: `{"blocked":2}`}, nil
		},
	})
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	if response.ThreadID != "thread-next" || response.Content != "Two items are blocked." {
		t.Fatalf("Reply() = %#v", response)
	}
	if backend.request.Workspace != workspacePath || backend.request.Prompt != "What is blocked?" || backend.request.ReasoningEffort != "low" || backend.request.Resume.ThreadID != "thread-existing" {
		t.Fatalf("turn request = %#v", backend.request)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if backend.request.TempDir != filepath.Join(canonicalWorkspace, ".detent", "tmp") {
		t.Fatalf("TempDir = %q", backend.request.TempDir)
	}
	if len(backend.tools) != 1 || backend.tools[0].Name != "board_state" {
		t.Fatalf("tools = %#v", backend.tools)
	}
	if backend.toolResult.Content != `{"blocked":2}` || !backend.toolResult.Success {
		t.Fatalf("tool result = %#v", backend.toolResult)
	}
	if _, err := os.Stat(backend.request.TempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("chat scratch stat error = %v, want not exist", err)
	}
}

func TestNewAgentProviderValidatesDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewAgentProvider(nil, t.TempDir(), nil); err == nil {
		t.Fatal("NewAgentProvider(nil) error = nil")
	}
	if _, err := NewAgentProvider(&agentToolBackendStub{}, " ", nil); err == nil {
		t.Fatal("NewAgentProvider(empty workspace) error = nil")
	}
}

type agentToolBackendStub struct {
	request    runner.AgentTurnRequest
	tools      []runner.AgentTool
	toolResult runner.AgentToolResult
}

func (s *agentToolBackendStub) RunTurnWithTools(
	ctx context.Context,
	request runner.AgentTurnRequest,
	tools []runner.AgentTool,
	handle runner.AgentToolHandler,
	onUpdate runner.AgentUpdateHandler,
) (runner.AgentTurnResult, error) {
	s.request = request
	s.tools = tools
	var err error
	s.toolResult, err = handle(ctx, runner.AgentToolCall{Name: "board_state", Arguments: json.RawMessage(`{"state":"Blocked"}`)})
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	for _, update := range []runner.AgentUpdate{
		{Type: runner.AgentUpdateMessageDelta, Delta: "Two items are "},
		{Type: runner.AgentUpdateMessageDelta, Delta: "blocked."},
	} {
		if err := onUpdate(update); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	return runner.AgentTurnResult{ThreadID: "thread-next", TurnID: "turn-1", SessionID: "thread-next-turn-1"}, nil
}
