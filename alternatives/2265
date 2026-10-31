package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestACPHelper(t *testing.T) {
	mode := os.Getenv("DETENT_TEST_ACP")
	if mode == "" {
		return
	}
	if mode == "hold-pipes" {
		conn, err := net.Dial("tcp", os.Getenv("DETENT_TEST_ACP_CONTROL"))
		if err != nil {
			os.Exit(4)
		}
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		os.Exit(0)
	}
	if mode == "detached" {
		child := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestACPHelper$")
		child.Env = append(os.Environ(), "DETENT_TEST_ACP=hold-pipes")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		procgroup.Configure(context.Background(), child)
		if err := child.Start(); err != nil {
			os.Exit(5)
		}
		if err := child.Process.Release(); err != nil {
			os.Exit(6)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	send := func(v any) {
		if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
			os.Exit(2)
		}
	}
	for scanner.Scan() {
		var r rpcRecord
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			os.Exit(3)
		}
		result := any(map[string]any{})
		switch r.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"mcpCapabilities": map[string]any{"http": true}}}
		case "session/new":
			result = map[string]any{"sessionId": "session-example"}
		case "session/set_config_option":
			var p struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
			}
			_ = json.Unmarshal(r.Params, &p)
			if mode == "mismatch" {
				p.Value = "other/model"
			}
			result = map[string]any{"configOptions": []map[string]string{{"id": p.ConfigID, "currentValue": p.Value}}}
		case "session/prompt":
			if mode == "diagnostics" {
				fmt.Fprintln(os.Stderr, "customer-secret-example")
				send(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32603, "message": "customer-secret-example"}})
				continue
			}
			if mode == "region" {
				send(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32603, "message": "customer-secret-example: This Go model requires Global regions. Select Global in your workspace's Privacy settings to use it."}})
				continue
			}
			if mode == "eof" {
				os.Exit(0)
			}
			if mode == "malformed" {
				fmt.Fprintln(os.Stdout, "invalid")
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-example", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "fixture complete"}}}})
			if mode == "cancel" {
				continue
			}
			result = map[string]any{"stopReason": "end_turn", "usage": map[string]int{"inputTokens": 20, "outputTokens": 5, "cachedReadTokens": 10, "cachedWriteTokens": 3, "thoughtTokens": 2, "totalTokens": 40}}
		}
		send(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result})
	}
	os.Exit(0)
}

func TestACPTurn(t *testing.T) {
	for _, mode := range []string{"success", "detached", "mismatch", "eof", "malformed", "callback", "cancel", "region", "diagnostics"} {
		t.Run(mode, func(t *testing.T) {
			var control string
			if mode == "detached" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				control = listener.Addr().String()
				connection := make(chan net.Conn, 1)
				go func() {
					conn, _ := listener.Accept()
					connection <- conn
				}()
				defer func() {
					_ = listener.Close()
					if conn := <-connection; conn != nil {
						_ = conn.Close()
					}
				}()
			}
			b, err := NewAgentBackend(Options{CommandFactory: func(ctx context.Context, args []string) *exec.Cmd {
				if len(args) != 1 || args[0] != "acp" {
					t.Fatalf("args=%v", args)
				}
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestACPHelper$")
				cmd.Env = append(os.Environ(), "DETENT_TEST_ACP="+mode, "DETENT_TEST_ACP_CONTROL="+control)
				return cmd
			}})
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			var tokens int64
			var usage runner.AgentTokenUsage
			var completed string
			var diagnostic string
			callbackErr := errors.New("callback rejected")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, err := b.RunTurn(ctx, runner.AgentTurnRequest{Workspace: t.TempDir(), TempDir: t.TempDir(), Model: "example/model", Prompt: "fixture"}, func(u runner.AgentUpdate) error {
				if u.Type == runner.AgentUpdateMessageDelta {
					text.WriteString(u.Delta)
					if mode == "cancel" {
						cancel()
					}
					if mode == "callback" {
						return callbackErr
					}
				}
				if u.Type == runner.AgentUpdateTokenUsage {
					tokens = u.Tokens.TotalTokens
					usage = u.Tokens
				}
				if u.Type == runner.AgentUpdateTurnCompleted {
					completed = u.Status
					diagnostic = u.BackendErrorMessage
				}
				return nil
			})
			if mode == "success" || mode == "detached" {
				if err != nil || result.SessionID != "session-example" || text.String() != "fixture complete" || tokens != 40 || usage.InputTokens != 33 || usage.CachedInputTokens != 10 || usage.OutputTokens != 7 || usage.ReasoningOutputTokens != 2 || completed != runner.FinalStateCompleted {
					t.Fatalf("result=%+v err=%v text=%q tokens=%d completed=%q", result, err, text.String(), tokens, completed)
				}
			} else if err == nil {
				t.Fatal("failure accepted")
			}
			if strings.Contains(diagnostic, "customer-secret-example") || err != nil && strings.Contains(err.Error(), "customer-secret-example") {
				t.Fatal("backend diagnostics exposed provider content")
			}
			if mode == "region" {
				details, ok := b.ClassifyCapacityError(err, nil, time.Time{})
				if !ok || details.Kind != "startup_handshake_failure" || !strings.Contains(details.Reason, "region policy") || !strings.Contains(err.Error(), "Settings > Privacy > Regions > Global") || text.Len() != 0 || tokens != 0 || completed != runner.FinalStateFailed {
					t.Fatalf("region failure misclassified: details=%+v err=%v", details, err)
				}
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			if mode == "callback" && !errors.Is(err, callbackErr) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

type deadlineContext struct{ done <-chan struct{} }

func (c deadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c deadlineContext) Done() <-chan struct{}       { return c.done }
func (c deadlineContext) Value(any) any               { return nil }

func (c deadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type rpcWriter func([]byte) (int, error)

func (f rpcWriter) Write(p []byte) (int, error) { return f(p) }

func TestACPDeadlineAttribution(t *testing.T) {
	for _, stage := range []string{"initialize", "session/new", "session/set_config_option", "session/prompt"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			records := make(chan scanResult, 1)
			writer := rpcWriter(func(data []byte) (int, error) {
				var request rpcRecord
				if err := json.Unmarshal(data, &request); err != nil {
					return 0, err
				}
				if request.Method == stage {
					cancel()
					return len(data), nil
				}
				results := map[string]string{
					"initialize":                `{"protocolVersion":1}`,
					"session/new":               `{"sessionId":"session"}`,
					"session/set_config_option": `{"configOptions":[{"id":"model","currentValue":"example/model"}]}`,
				}
				records <- scanResult{record: rpcRecord{ID: request.ID, Result: json.RawMessage(results[request.Method])}}
				return len(data), nil
			})
			backend, _ := NewAgentBackend(Options{})
			state := &turnState{req: runner.AgentTurnRequest{Model: "example/model"}, emit: func(runner.AgentUpdate) error { return nil }}
			err := backend.exchange(deadlineContext{ctx.Done()}, writer, records, state)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v", err)
			}
			details, classified := backend.ClassifyCapacityError(err, nil, time.Time{})
			if stage == "session/prompt" {
				if classified {
					t.Fatal("execution timeout classified as startup failure")
				}
			} else if !classified || details.Kind != "startup_handshake_timeout" {
				t.Fatalf("startup timeout attribution = %+v, %v", details, classified)
			}
		})
	}
}

func TestUnsupportedRequestsDoNotLaunch(t *testing.T) {
	for _, name := range []string{"resume", "readonly", "textonly", "subscription", "tools", "model", "sandbox"} {
		t.Run(name, func(t *testing.T) {
			b, _ := NewAgentBackend(Options{CommandFactory: func(context.Context, []string) *exec.Cmd { t.Fatal("unexpected launch"); return nil }})
			req := runner.AgentTurnRequest{Model: "example/model"}
			switch name {
			case "resume":
				req.Resume.SessionID = "session"
			case "readonly":
				req.ReadOnly = true
			case "textonly":
				req.TextOnly = true
			case "subscription":
				req.RequireSubscriptionAuth = true
			case "tools":
				req.SupplementalTools = true
			case "model":
				req.Model = "model"
			case "sandbox":
				b.options.IsolationPolicy = func() (isolation.Policy, error) { return isolation.Policy{Tier: isolation.Sandbox}, nil }
			}
			if _, err := b.RunTurn(t.Context(), req, nil); err == nil {
				t.Fatal("unsupported request accepted")
			}
		})
	}
}

func TestOpenCodeLive(t *testing.T) {
	model := os.Getenv("DETENT_OPENCODE_SMOKE_MODEL")
	if model == "" {
		t.Skip("explicit smoke model required")
	}
	b, _ := NewAgentBackend(Options{TurnTimeout: 2 * time.Minute})
	var output strings.Builder
	var calls int
	tools := []runner.AgentTool{{Name: "pilot_value", Description: "Read the required value for this test", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}
	result, err := b.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Workspace: t.TempDir(), TempDir: t.TempDir(), Model: model, SupplementalTools: true, Prompt: "Call the pilot_value MCP tool. Reply with its returned value exactly. Do not read files, run commands, or create child sessions."}, tools, func(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if call.Name != "pilot_value" {
			return runner.AgentToolResult{}, errors.New("unexpected tool")
		}
		calls++
		return runner.AgentToolResult{Content: `{"value":"detent-opencode-connected"}`, Success: true}, nil
	}, func(u runner.AgentUpdate) error {
		if u.Type == runner.AgentUpdateMessageDelta {
			output.WriteString(u.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(output.String(), "detent-opencode-connected") || result.SessionID == "" {
		t.Fatalf("calls=%d output=%q session=%q", calls, output.String(), result.SessionID)
	}
	t.Logf("model=%s scoped_tool_calls=%d session=%s", model, calls, result.SessionID)
}

func TestACPClientPermissionScope(t *testing.T) {
	for _, test := range []struct{ name, session, method, kind, want string }{
		{"trusted once", "session", "session/request_permission", "allow_once", "selected"},
		{"other session", "other", "session/request_permission", "allow_once", "cancelled"},
		{"never persistent", "session", "session/request_permission", "allow_always", "cancelled"},
		{"unknown method", "session", "fs/write_text_file", "allow_once", "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := turnState{sessionID: "session"}
			params, err := json.Marshal(map[string]any{"sessionId": test.session, "options": []map[string]string{{"optionId": "option", "kind": test.kind}}})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err = state.respond(&out, rpcRecord{ID: json.RawMessage(`7`), Method: test.method, Params: params}); err != nil {
				t.Fatal(err)
			}
			var response struct {
				ID     int `json:"id"`
				Result struct {
					Outcome struct {
						Outcome string `json:"outcome"`
					} `json:"outcome"`
				} `json:"result"`
				Error json.RawMessage `json:"error"`
			}
			if err = json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			got := response.Result.Outcome.Outcome
			if len(response.Error) > 0 {
				got = "error"
			}
			if response.ID != 7 || got != test.want {
				t.Fatalf("response=%s", out.String())
			}
		})
	}
}
