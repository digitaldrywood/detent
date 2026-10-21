package claudecode

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (b *AgentBackend) RunTurnWithTools(ctx context.Context, req runner.AgentTurnRequest, tools []runner.AgentTool, handle runner.AgentToolHandler, update runner.AgentUpdateHandler) (result runner.AgentTurnResult, turnErr error) {
	if len(tools) == 0 {
		return b.RunTurn(ctx, req, update)
	}
	if handle == nil {
		return result, errors.New("claude worker tool handler is unavailable")
	}
	ctx, cancel := context.WithCancel(contextOrBackground(ctx))
	defer cancel()
	bounded := *b
	var definitions []operatortool.Definition
	for _, tool := range tools {
		definitions = append(definitions, operatortool.Definition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
		bounded.toolNames = append(bounded.toolNames, "mcp__detent__"+tool.Name)
	}
	executor, err := mcp.NewConnectionExecutor(definitions, func(callCtx context.Context, call operatortool.Call) (operatortool.Result, error) {
		response, callErr := handle(callCtx, runner.AgentToolCall{Name: call.Name, Arguments: call.Arguments})
		if callErr != nil || !response.Success {
			message := response.Content
			if message == "" {
				message = "Worker tool refused the request"
			}
			return operatortool.Result{}, &operatortool.RequestError{Code: "worker_tool_refused", Message: message}
		}
		content := json.RawMessage(response.Content)
		var object map[string]json.RawMessage
		if json.Unmarshal(content, &object) != nil || object == nil {
			content, callErr = json.Marshal(map[string]string{"content": response.Content})
		}
		return operatortool.Result{Content: content}, callErr
	})
	if err != nil {
		return result, err
	}
	directory, err := os.MkdirTemp(req.TempDir, "claude-tools-")
	if err != nil {
		return result, err
	}
	defer func() { turnErr = errors.Join(turnErr, os.RemoveAll(directory)) }()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return result, err
	}
	token := "Bearer " + rand.Text()
	protocol := mcp.NewHTTPHandler(executor, "worker", mcp.HTTPConfig{MaxSessions: 1})
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(token)) != 1 {
			http.Error(w, "Worker tool connection is unauthorized", http.StatusUnauthorized)
			return
		}
		protocol.ServeHTTP(w, r)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		cancel()
		closeErr := server.Close()
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		shutdownErr := protocol.Shutdown(shutdownCtx)
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		turnErr = errors.Join(turnErr, closeErr, shutdownErr, serveErr)
	}()
	bounded.toolAddress = listener.Addr().String()
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"detent": map[string]any{"type": "http", "url": "http://" + bounded.toolAddress, "headers": map[string]string{"Authorization": token}}}})
	if err != nil {
		return result, err
	}
	bounded.toolConfig = filepath.Join(directory, "mcp.json")
	if err := os.WriteFile(bounded.toolConfig, config, 0o600); err != nil {
		return result, err
	}
	return bounded.RunTurn(ctx, req, update)
}
