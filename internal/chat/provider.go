package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type AgentProvider struct {
	backend   runner.AgentToolBackend
	workspace string
	logger    *slog.Logger
	mu        sync.Mutex
}

func NewAgentProvider(backend runner.AgentToolBackend, workspacePath string, logger *slog.Logger) (*AgentProvider, error) {
	if backend == nil {
		return nil, errors.New("chat agent backend is required")
	}
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return nil, errors.New("chat workspace is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentProvider{backend: backend, workspace: workspacePath, logger: logger}, nil
}

func (p *AgentProvider) Reply(ctx context.Context, request TurnRequest) (TurnResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	scratch, err := workspace.PrepareWorkerScratch(ctx, p.workspace)
	if err != nil {
		return TurnResponse{}, fmt.Errorf("prepare chat scratch: %w", err)
	}
	defer func() {
		if err := workspace.CleanupWorkerScratch(p.workspace); err != nil {
			p.logger.Warn("chat scratch cleanup failed", "error", err)
		}
	}()

	tools := make([]runner.AgentTool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, runner.AgentTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	var response strings.Builder
	result, err := p.backend.RunTurnWithTools(ctx, runner.AgentTurnRequest{
		Workspace:       p.workspace,
		TempDir:         scratch,
		Prompt:          request.Prompt,
		ReasoningEffort: "low",
		Resume:          runner.AgentResume{ThreadID: request.ThreadID},
	}, tools, func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if request.Handle == nil {
			return runner.AgentToolResult{}, errors.New("chat tool handler is unavailable")
		}
		toolResult, err := request.Handle(ctx, ToolCall{Name: call.Name, Arguments: call.Arguments})
		return runner.AgentToolResult{Content: toolResult.Content, Success: err == nil}, err
	}, func(update runner.AgentUpdate) error {
		switch update.Type {
		case runner.AgentUpdateProcessStarted:
			p.logger.Info("chat provider process started", "process_identity", update.ProcessIdentity, "pid", update.WorkerProcess.PID, "pgid", update.WorkerProcess.GroupID, "started_at", update.WorkerProcess.StartedAt)
		case runner.AgentUpdateMessageDelta:
			response.WriteString(update.Delta)
		}
		return nil
	})
	if err != nil {
		return TurnResponse{}, err
	}
	return TurnResponse{ThreadID: result.ThreadID, Content: response.String()}, nil
}
