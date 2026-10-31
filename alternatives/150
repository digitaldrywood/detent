package antigravity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
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
		return result, errors.New("antigravity worker tool handler is unavailable")
	}
	if req.TempDir == "" {
		return result, errors.New("antigravity worker tools require a worker scratch directory")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var definitions []operatortool.Definition
	for _, tool := range tools {
		definitions = append(definitions, operatortool.Definition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
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
	directory, err := os.MkdirTemp(req.TempDir, "antigravity-tools-")
	if err != nil {
		return result, &infrastructureError{err: fmt.Errorf("create Antigravity tool directory: %w", err), startup: true}
	}
	defer func() {
		turnErr = errors.Join(turnErr, os.RemoveAll(directory))
	}()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return result, &infrastructureError{err: fmt.Errorf("listen for Antigravity worker tools: %w", err), startup: true}
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
	if err := writeToolConfig(directory, "http://"+listener.Addr().String()+"/mcp", token); err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	bounded := *b
	bounded.toolDirectory = directory
	return bounded.RunTurn(ctx, req, update)
}

func writeToolConfig(directory string, url string, token string) error {
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"detent": map[string]any{"serverUrl": url, "headers": map[string]string{"Authorization": token}}}})
	if err != nil {
		return fmt.Errorf("encode Antigravity tool config: %w", err)
	}
	agents := filepath.Join(directory, ".agents")
	if err := os.Mkdir(agents, 0o700); err != nil {
		return fmt.Errorf("create Antigravity tool config directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agents, "mcp_config.json"), config, 0o600); err != nil {
		return fmt.Errorf("write Antigravity tool config: %w", err)
	}
	return nil
}
