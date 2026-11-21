package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestAgentBackendAppliesOptionsAndExtraWritableRoots(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"gpt-5-codex-resolved"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	factory := &workerTempCapturingTransportFactory{transport: transport}
	server, err := NewAppServer(factory,
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}
	backend, err := NewAgentBackend(server, Options{
		ApprovalPolicy: "never",
		ThreadSandbox:  "workspace-write",
		TurnSandboxPolicy: map[string]any{
			"type":          "workspaceWrite",
			"networkAccess": true,
			"writableRoots": []string{"/existing"},
		},
	})
	if err != nil {
		t.Fatalf("NewAgentBackend() error = %v", err)
	}

	var updates []runner.AgentUpdate
	_, err = backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace:          "/tmp/detent-workspace",
		TempDir:            "/tmp/detent-workspace/.detent/tmp",
		Prompt:             "Ship issue #820",
		Model:              "gpt-5-codex",
		ReasoningEffort:    "high",
		ExtraWritableRoots: []string{"/extra", "/existing", " "},
	}, func(update runner.AgentUpdate) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if factory.tempDir != "/tmp/detent-workspace/.detent/tmp" {
		t.Fatalf("worker temp directory = %q, want request temp directory", factory.tempDir)
	}
	if factory.workspace != "/tmp/detent-workspace" {
		t.Fatalf("worker workspace = %q, want request workspace", factory.workspace)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}

	assertRequest(t, sent[2], 2, "thread/start")
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "workspace-write")
	assertJSONContains(t, sent[2].Params, "model", "gpt-5-codex")
	assertJSONOmits(t, sent[2].Params, "effort")
	assertJSONContains(t, sent[2].Params, "developerInstructions", terminalWaitInstructions)

	assertRequest(t, sent[3], 3, "turn/start")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.type", "workspaceWrite")
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.networkAccess", true)
	paths, err := toolcache.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.writableRoots", []any{"/existing", "/extra", paths.Build, paths.Modules})
	assertJSONContains(t, sent[3].Params, "model", "gpt-5-codex")
	assertJSONContains(t, sent[3].Params, "effort", "high")

	if len(updates) < 2 || updates[0].Type != runner.AgentUpdateRuntimeIdentity || updates[1].Type != runner.AgentUpdateTurnStarted || updates[1].Model != "gpt-5-codex-resolved" {
		t.Fatalf("updates = %#v, want resolved model on turn started", updates)
	}
}

func TestAgentBackendAppliesProcessContextToPreflightTransports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		messages  func(*testing.T) []Message
		operation func(context.Context, *AgentBackend, runner.AgentProcessRequest) error
	}{
		{
			name: "model catalog",
			messages: func(t *testing.T) []Message {
				return []Message{
					responseMessage(t, 1, `{"userAgent":"codex-cli/test"}`),
					responseMessage(t, 6, `{"data":[{"id":"example-model","model":"example-model"}]}`),
				}
			},
			operation: func(ctx context.Context, backend *AgentBackend, process runner.AgentProcessRequest) error {
				_, err := backend.ListModels(ctx, process)
				return err
			},
		},
		{
			name: "default model",
			messages: func(t *testing.T) []Message {
				return []Message{
					responseMessage(t, 1, `{"userAgent":"codex-cli/test"}`),
					responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
				}
			},
			operation: func(ctx context.Context, backend *AgentBackend, process runner.AgentProcessRequest) error {
				_, err := backend.DefaultModel(ctx, process)
				return err
			},
		},
		{
			name: "resume verification",
			messages: func(t *testing.T) []Message {
				return []Message{
					responseMessage(t, 1, `{"userAgent":"codex-cli/test"}`),
					responseMessage(t, 7, `{"thread":{"id":"thread-existing"}}`),
				}
			},
			operation: func(ctx context.Context, backend *AgentBackend, process runner.AgentProcessRequest) error {
				return backend.VerifyResume(ctx, process, runner.AgentResume{ThreadID: "thread-existing"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			factory := &workerTempCapturingTransportFactory{transport: newFakeAppServerTransport(tt.messages(t))}
			server, err := NewAppServer(factory, WithReadTimeout(time.Second))
			if err != nil {
				t.Fatalf("NewAppServer() error = %v", err)
			}
			backend, err := NewAgentBackend(server, Options{})
			if err != nil {
				t.Fatalf("NewAgentBackend() error = %v", err)
			}
			process := runner.AgentProcessRequest{
				Workspace: "/tmp/detent-workspace",
				TempDir:   "/tmp/detent-preflight",
				Environment: procgroup.Environment{Variables: map[string]string{
					"DETENT_WORKSPACE":        "/tmp/detent-workspace",
					"DETENT_ISSUE_ID":         "issue-2561",
					"DETENT_ISSUE_IDENTIFIER": "digitaldrywood/detent#2561",
				}},
			}
			if err := tt.operation(t.Context(), backend, process); err != nil {
				t.Fatalf("operation error = %v", err)
			}
			if factory.workspace != process.Workspace {
				t.Fatalf("worker workspace = %q, want %q", factory.workspace, process.Workspace)
			}
			if factory.tempDir != process.TempDir {
				t.Fatalf("worker temp dir = %q, want %q", factory.tempDir, process.TempDir)
			}
			if !reflect.DeepEqual(factory.variables, process.Environment.Variables) {
				t.Fatalf("worker variables = %#v, want %#v", factory.variables, process.Environment.Variables)
			}
		})
	}
}

func TestToolTurnInstructions(t *testing.T) {
	t.Parallel()

	tool := DynamicTool{Name: "board_state"}
	tests := []struct {
		name     string
		tools    []DynamicTool
		override string
		want     string
	}{
		{
			name: "normal turn",
			want: terminalWaitInstructions,
		},
		{
			name:     "normal turn with override",
			override: "Use a custom tool policy.",
			want:     "Use a custom tool policy.",
		},
		{
			name:  "dynamic tool turn",
			tools: []DynamicTool{tool},
			want:  dynamicToolTurnInstructions,
		},
		{
			name:     "dynamic tool turn with override",
			tools:    []DynamicTool{tool},
			override: "  Inspect only and submit proposals.  ",
			want:     "Inspect only and submit proposals.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := toolTurnInstructions(tt.tools, tt.override); got != tt.want {
				t.Fatalf("toolTurnInstructions() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMCPElicitationPolicyForTurn(t *testing.T) {
	t.Parallel()

	rules := []MCPElicitationRule{{
		Server: "codex_apps", Tool: "github.create_pull_request", Repository: "acme/widgets",
	}}
	tests := []struct {
		name       string
		restricted bool
		want       MCPElicitationPolicy
	}{
		{
			name: "deliverable turn",
			want: MCPElicitationPolicy{
				DeliverableKind: "pull_request",
				Repository:      "acme/widgets",
				IssueRepository: "acme/issues",
				Allowlist:       rules,
			},
		},
		{name: "restricted turn", restricted: true, want: MCPElicitationPolicy{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mcpElicitationPolicy(rules, runner.AgentTurnRequest{
				DeliverableKind:       " pull_request ",
				DeliverableRepository: " acme/widgets ",
				IssueRepository:       " acme/issues ",
			}, tt.restricted)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mcpElicitationPolicy() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAgentUpdateFromCodexCarriesCumulativeAndLastTokenUsage(t *testing.T) {
	t.Parallel()

	contextWindow := int64(200_000)
	update := agentUpdateFromCodex(Update{
		Type:          UpdateTokenUsage,
		ThreadID:      "thread-1716",
		TurnID:        "turn-2",
		AuxiliaryTurn: true,
		Tokens: TokenUsage{
			InputTokens:        14_300_000,
			CachedInputTokens:  13_900_000,
			OutputTokens:       119_042,
			TotalTokens:        14_419_042,
			ModelContextWindow: &contextWindow,
			Last: &TokenUsageBreakdown{
				InputTokens:       78_500,
				CachedInputTokens: 76_000,
				OutputTokens:      1_048,
				TotalTokens:       79_548,
			},
		},
	})

	if update.Tokens.ThreadTotal == nil || update.Tokens.ThreadTotal.TotalTokens != 14_419_042 {
		t.Fatalf("ThreadTotal = %#v, want cumulative provider total", update.Tokens.ThreadTotal)
	}
	if update.Tokens.Last == nil || update.Tokens.Last.TotalTokens != 79_548 {
		t.Fatalf("Last = %#v, want last-call usage", update.Tokens.Last)
	}
	if update.ProviderSessionID != "thread-1716-turn-2" {
		t.Fatalf("ProviderSessionID = %q, want thread-turn identity", update.ProviderSessionID)
	}
	if !update.AuxiliaryTurn {
		t.Fatal("AuxiliaryTurn = false, want child turn marker")
	}
}

func TestAgentUpdateFromCodexCarriesCommandCompletionEvidence(t *testing.T) {
	t.Parallel()

	exitCode := 19
	update := agentUpdateFromCodex(Update{
		Type:     UpdateToolCompleted,
		ItemID:   "push-item",
		Tool:     "commandExecution",
		Command:  "git push origin HEAD && exit 19",
		Status:   "failed",
		ExitCode: &exitCode,
	})

	if update.Command != "git push origin HEAD && exit 19" || update.ExitCode == nil || *update.ExitCode != exitCode {
		t.Fatalf("command completion evidence = command %q exit %#v", update.Command, update.ExitCode)
	}
}

func TestAgentBackendOperatorEnforcesConfiguredStallTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		transport := newBlockingAppServerTransport([]Message{
			responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
			responseMessage(t, 2, `{"thread":{"id":"thread-stall"}}`),
			responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
			responseMessage(t, 3, `{"turn":{"id":"turn-stall"}}`),
		})
		server, err := NewAppServer(staticTransportFactory{transport: transport},
			WithReadTimeout(time.Second),
			WithTurnTimeout(time.Hour),
		)
		if err != nil {
			t.Fatalf("NewAppServer() error = %v", err)
		}
		backend, err := NewAgentBackend(server, OptionsFromConfig(config.CodexOptions{
			StallTimeoutMS: 10,
		}))
		if err != nil {
			t.Fatalf("NewAgentBackend() error = %v", err)
		}

		_, err = backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{
			Workspace: "/tmp/detent-workspace",
			Prompt:    "stall",
		}, []runner.AgentTool{{Name: "board_state", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil, nil)
		if !errors.Is(err, ErrStreamStalled) {
			t.Fatalf("RunTurnWithTools() error = %v, want configured ErrStreamStalled", err)
		}
	})
}

func TestAgentBackendToolTurnUsesRestrictedSandbox(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"gpt-5-codex-resolved"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		serverRequestMessage(t, 99, "item/tool/call", `{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"board_state","arguments":{}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}
	backend, err := NewAgentBackend(server, Options{
		ApprovalPolicy: "on-request",
		ThreadSandbox:  "workspace-write",
		TurnSandboxPolicy: map[string]any{
			"type":          "workspaceWrite",
			"networkAccess": true,
			"writableRoots": []string{"/existing"},
		},
	})
	if err != nil {
		t.Fatalf("NewAgentBackend() error = %v", err)
	}

	_, err = backend.RunTurnWithTools(context.Background(), runner.AgentTurnRequest{
		Workspace:        "/tmp/detent-workspace",
		TempDir:          "/tmp/detent-workspace/.detent/tmp",
		Prompt:           "What is blocked?",
		ToolInstructions: "Inspect only and submit proposals.",
		ReasoningEffort:  "high",
	}, []runner.AgentTool{{
		Name:        "board_state",
		Description: "Return live board state.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}}, func(context.Context, runner.AgentToolCall) (runner.AgentToolResult, error) {
		return runner.AgentToolResult{Content: "published capture", Success: true, ImageURL: "data:image/png;base64,example"}, nil
	}, nil)
	if err != nil {
		t.Fatalf("RunTurnWithTools() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 5 {
		t.Fatalf("sent messages = %d, want 5", len(sent))
	}
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "read-only")
	assertJSONContains(t, sent[2].Params, "dynamicTools.0.name", "board_state")
	assertJSONContains(t, sent[2].Params, "developerInstructions", "Inspect only and submit proposals.")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "effort", "high")
	assertJSONOmits(t, sent[3].Params, "sandboxPolicy")
	assertResponseResultContains(t, sent[4], 99, "contentItems.0.text", "published capture")
	assertResponseResultContains(t, sent[4], 99, "contentItems.1.type", "inputImage")
	assertResponseResultContains(t, sent[4], 99, "contentItems.1.imageUrl", "data:image/png;base64,example")
}

func TestAgentBackendReadOnlyTurnUsesRestrictedSandbox(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"gpt-5-codex-resolved"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}
	backend, err := NewAgentBackend(server, Options{
		ApprovalPolicy: "bypass",
		ThreadSandbox:  "workspace-write",
		TurnSandboxPolicy: map[string]any{
			"type":          "workspaceWrite",
			"networkAccess": true,
			"writableRoots": []string{"/existing"},
		},
	})
	if err != nil {
		t.Fatalf("NewAgentBackend() error = %v", err)
	}

	_, err = backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace:          "/tmp/detent-workspace",
		Prompt:             "Inspect maintenance criteria.",
		ReadOnly:           true,
		ExtraWritableRoots: []string{"/extra"},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "read-only")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONOmits(t, sent[3].Params, "sandboxPolicy")
}

func TestAgentBackendTextOnlyTurn(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		config        string
		fail          bool
		tier          string
		investigation bool
	}{
		{name: "disables configured tools", config: `{"config":{"mcp_servers":{"files.example":{"command":"file-reader"},"remote":{"url":"https://example.test/mcp"}}}}`},
		{name: "native trusted isolation", tier: isolation.NativeTrusted, config: `{"config":{"mcp_servers":{"files.example":{"command":"file-reader"},"remote":{"url":"https://example.test/mcp"}}}}`},
		{name: "sandbox isolation", tier: isolation.Sandbox, config: `{"config":{"mcp_servers":{"files.example":{"command":"file-reader"},"remote":{"url":"https://example.test/mcp"}}}}`},
		{name: "missing configuration", config: `{}`, fail: true},
		{name: "invalid configuration", config: `{"config":{"mcp_servers":true}}`, fail: true},
		{name: "investigation disables external tools", investigation: true, config: `{"config":{"mcp_servers":{"files.example":{"command":"file-reader"},"remote":{"url":"https://example.test/mcp"}}}}`},
		{name: "trusted investigation stays read only", investigation: true, tier: isolation.NativeTrusted, config: `{"config":{"mcp_servers":{"files.example":{"command":"file-reader"},"remote":{"url":"https://example.test/mcp"}}}}`},
		{name: "sandbox investigation stays read only", investigation: true, tier: isolation.Sandbox, config: `{"config":{"mcp_servers":{"files.example":{"command":"file-reader"},"remote":{"url":"https://example.test/mcp"}}}}`},
		{name: "investigation missing configuration refuses turn", investigation: true, config: `{}`, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := newFakeAppServerTransport([]Message{
				responseMessage(t, 1, `{"userAgent":"codex-cli/0.157.0"}`),
				responseMessage(t, 5, test.config),
				responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"test-model"}}`),
				responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
				notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
			})
			if test.investigation && !test.fail {
				transport.received = append(transport.received[:len(transport.received)-1],
					serverRequestMessage(t, 90, "item/commandExecution/requestApproval", `{"command":"printf changed > source.txt"}`),
					serverRequestMessage(t, 91, "item/fileChange/requestApproval", `{"changes":{"source.txt":"changed"}}`),
					serverRequestMessage(t, 92, "item/permissions/requestApproval", `{"permissions":{"network":{"enabled":true},"fileSystem":{"write":["/scratch"]}}}`),
					notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
			}
			server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			options := Options{ApprovalPolicy: "bypass", ThreadSandbox: "danger-full-access"}
			if test.tier != "" {
				options.IsolationPolicy = func() (isolation.Policy, error) { return isolation.Policy{Tier: test.tier}, nil }
			}
			backend, err := NewAgentBackend(server, options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: "/scratch", ReadOnly: true, TextOnly: !test.investigation, IsolatedTools: test.investigation, Prompt: "Attempt a write in the source workspace and publish it through configured external tools.", ExtraWritableRoots: []string{"/private"}}, nil)
			sent := transport.sentMessages()
			if test.fail {
				if err == nil || len(sent) != 3 {
					t.Fatalf("unsafe configuration sent prompt: err=%v messages=%#v", err, sent)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRequest(t, sent[2], 5, "config/read")
			profile, rootAccess := "detent-text-only", "deny"
			if test.investigation {
				profile, rootAccess = "detent-investigation", "read"
			}
			assertJSONContains(t, sent[3].Params, "permissions", profile)
			assertJSONContains(t, sent[3].Params, "runtimeWorkspaceRoots", []any{"/scratch"})
			assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
			assertJSONContains(t, sent[3].Params, "config.permissions."+profile+".filesystem./", rootAccess)
			assertJSONContains(t, sent[3].Params, "config.permissions."+profile+".filesystem.:workspace_roots", "read")
			assertJSONContains(t, sent[3].Params, "config.permissions."+profile+".network.enabled", false)
			assertJSONContains(t, sent[3].Params, "config.project_doc_max_bytes", 0)
			var params struct {
				Config map[string]any `json:"config"`
			}
			if err := json.Unmarshal(sent[3].Params, &params); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"features.shell_tool", "features.unified_exec", "features.view_image", "features.apps", "features.plugins", "features.browser_use", "features.computer_use", "features.code_mode", "features.code_mode_host", "features.multi_agent", "features.hooks", "features.shell_snapshot", "features.image_generation", "features.multi_agent_v2", "features.goals", "features.tool_suggest", "features.skill_search", "features.memories", "features.sleep_tool", "features.network_proxy"} {
				want := test.investigation && (key == "features.shell_tool" || key == "features.unified_exec")
				if value, ok := params.Config[key]; !ok || value != want {
					t.Fatalf("tool remains available: %s=%v", key, value)
				}
			}
			servers, ok := params.Config["mcp_servers"].(map[string]any)
			if !ok || len(servers) != 2 {
				t.Fatalf("MCP configuration missing: %#v", params.Config["mcp_servers"])
			}
			for _, name := range []string{"files.example", "remote"} {
				server, ok := servers[name].(map[string]any)
				if !ok || server["enabled"] != false {
					t.Fatalf("MCP server remains enabled: %#v", servers[name])
				}
			}
			if params.Config["web_search"] != "disabled" {
				t.Fatal("web search remains available")
			}
			assertJSONContains(t, sent[4].Params, "permissions", profile)
			assertJSONContains(t, sent[4].Params, "approvalPolicy", "never")
			if test.investigation {
				for _, response := range sent[5:] {
					if string(response.ID) == "90" || string(response.ID) == "91" {
						assertJSONContains(t, response.Result, "decision", "decline")
					}
					if string(response.ID) == "92" {
						assertJSONContains(t, response.Result, "permissions", map[string]any{})
					}
				}
				if len(sent) != 8 {
					t.Fatalf("write/network approval attempts not answered: %d", len(sent))
				}
			}

		})
	}
}

func TestAgentBackendSupplementalToolsPreserveWorkerPolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, sandbox, approval, instructions string
		supplemental, readOnly                bool
	}{
		{name: "operator", sandbox: "read-only", approval: "never", instructions: dynamicToolTurnInstructions},
		{name: "worker", supplemental: true, sandbox: "workspace-write", approval: "on-request", instructions: terminalWaitInstructions},
		{name: "read-only worker", supplemental: true, readOnly: true, sandbox: "read-only", approval: "never", instructions: terminalWaitInstructions},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transport := newFakeAppServerTransport([]Message{
				responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
				responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"gpt-5-codex-resolved"}}`),
				responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
				notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
			})
			server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			backend, err := NewAgentBackend(server, Options{ApprovalPolicy: "on-request", ThreadSandbox: "workspace-write", TurnSandboxPolicy: map[string]any{"type": "workspaceWrite", "networkAccess": true}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Workspace: t.TempDir(), Prompt: "Continue assigned work", SupplementalTools: tt.supplemental, ReadOnly: tt.readOnly}, []runner.AgentTool{{Name: "ensure_human_prerequisite", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			sent := transport.sentMessages()
			if len(sent) != 4 {
				t.Fatalf("sent %d messages", len(sent))
			}
			assertJSONContains(t, sent[2].Params, "sandbox", tt.sandbox)
			assertJSONContains(t, sent[2].Params, "approvalPolicy", tt.approval)
			assertJSONContains(t, sent[2].Params, "developerInstructions", tt.instructions)
			assertJSONContains(t, sent[2].Params, "dynamicTools.0.name", "ensure_human_prerequisite")
			assertJSONContains(t, sent[3].Params, "approvalPolicy", tt.approval)
			if tt.sandbox == "read-only" {
				assertJSONOmits(t, sent[3].Params, "sandboxPolicy")
			} else {
				assertJSONContains(t, sent[3].Params, "sandboxPolicy.type", "workspaceWrite")
			}
		})
	}
}

type workerTempCapturingTransportFactory struct {
	transport Transport
	tempDir   string
	workspace string
	variables map[string]string
}

func (f *workerTempCapturingTransportFactory) NewTransport(ctx context.Context) (Transport, error) {
	f.tempDir = workerTempDir(ctx)
	f.workspace = workerWorkspace(ctx)
	f.variables = workerEnvironment(ctx).Variables
	return f.transport, nil
}

func TestAgentBackendNativeCommandWait(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                                      string
		resume, tools, supplemental, readOnly     bool
		registered                                bool
		traffic                                   string
		turnTimeout, cancelAfter, commandDuration time.Duration
		wantErr                                   error
		wantElapsed                               time.Duration
	}{
		{name: "quiet without a command", wantErr: ErrStreamStalled, wantElapsed: 5 * time.Minute},
		{name: "unrelated traffic", tools: true, traffic: "unrelated", wantErr: ErrStreamStalled, wantElapsed: 5 * time.Minute},
		{name: "replayed completion", tools: true, traffic: "replay", wantErr: ErrStreamStalled, wantElapsed: 5 * time.Minute},
		{name: "current activity", tools: true, traffic: "current", wantElapsed: 8 * time.Minute},
		{name: "repeated output", tools: true, traffic: "repeated output", wantElapsed: 8 * time.Minute},
		{name: "busy command exceeds deadline", tools: true, registered: true, traffic: "busy", turnTimeout: 6 * time.Minute, wantErr: context.DeadlineExceeded, wantElapsed: 6 * time.Minute},
		{name: "registered quiet command", tools: true, registered: true, wantElapsed: 45 * time.Minute},
		{name: "resumed worker", resume: true, registered: true, wantElapsed: 45 * time.Minute},
		{name: "supplemental worker", tools: true, supplemental: true, registered: true, wantElapsed: 45 * time.Minute},
		{name: "read-only worker", readOnly: true, registered: true, wantElapsed: 45 * time.Minute},
		{name: "operator keeps stall limit", tools: true, wantErr: ErrStreamStalled, wantElapsed: 5 * time.Minute},
		{name: "worker keeps stream limit", turnTimeout: 10 * time.Minute, registered: true, wantErr: context.DeadlineExceeded, wantElapsed: 10 * time.Minute},
		{name: "worker keeps default stream limit", commandDuration: 70 * time.Minute, registered: true, wantErr: context.DeadlineExceeded, wantElapsed: time.Hour},
		{name: "worker cancellation", cancelAfter: 7 * time.Minute, registered: true, wantErr: context.Canceled, wantElapsed: 7 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				threadRequest := threadStartRequestID
				resumeID := ""
				if tt.resume {
					threadRequest = threadResumeRequestID
					resumeID = "thread-1"
				}
				duration := tt.commandDuration
				if duration == 0 {
					duration = 45 * time.Minute
				}
				messages := []Message{
					responseMessage(t, 1, `{"userAgent":"codex-cli/0.155.1"}`),
					rolloutResponseMessage(t, threadRequest, `{"thread":{"id":"thread-1","model":"gpt-6-astra"}}`, nil),
					responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
				}
				if tt.registered {
					messages = append(messages, notificationMessage(t, "item/started", `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"command-1","type":"commandExecution","command":"make test","status":"inProgress"}}`))
				}
				delays := map[int]time.Duration{}
				switch tt.traffic {
				case "unrelated":
					for range 3 {
						delays[len(messages)] = 4 * time.Minute
						messages = append(messages, notificationMessage(t, "item/agentMessage/delta", `{"threadId":"thread-2","turnId":"turn-1","itemId":"text","delta":"unrelated"}`))
					}
				case "replay":
					message := notificationMessage(t, "item/completed", `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"read-1","type":"mcpToolCall","tool":"read_item","status":"completed"}}`)
					messages = append(messages, message)
					for range 3 {
						delays[len(messages)] = 4 * time.Minute
						messages = append(messages, message)
					}
				case "current":
					delays[len(messages)] = 4 * time.Minute
					messages = append(messages, notificationMessage(t, "item/agentMessage/delta", `{"threadId":"thread-1","turnId":"turn-1","itemId":"text","delta":"active"}`))
					delays[len(messages)] = 4 * time.Minute
				case "repeated output":
					message := notificationMessage(t, "item/agentMessage/delta", `{"threadId":"thread-1","turnId":"turn-1","itemId":"text","delta":"active"}`)
					messages = append(messages, message)
					delays[len(messages)] = 4 * time.Minute
					messages = append(messages, message)
					delays[len(messages)] = 4 * time.Minute
				case "busy":
					for i := range 4 {
						delays[len(messages)] = 2 * time.Minute
						messages = append(messages, notificationMessage(t, "item/commandExecution/outputDelta", fmt.Sprintf(`{"threadId":"thread-1","turnId":"turn-1","itemId":"command-1","delta":"%d"}`, i)))
					}
				default:
					delays[len(messages)] = duration
				}
				messages = append(messages, notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`))
				transport := &quietCommandTransport{fakeAppServerTransport: newFakeAppServerTransport(messages), delays: delays}
				server, err := NewAppServer(staticTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				backend, err := NewAgentBackend(server, Options{ThreadSandbox: "read-only", StallTimeout: 5 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.cancelAfter > 0 {
					timer := time.AfterFunc(tt.cancelAfter, cancel)
					defer timer.Stop()
				}
				req := runner.AgentTurnRequest{Workspace: t.TempDir(), Prompt: "Run the gate", Resume: runner.AgentResume{ThreadID: resumeID}, SupplementalTools: tt.supplemental, ReadOnly: tt.readOnly, TurnTimeout: tt.turnTimeout}
				var tools []runner.AgentTool
				if tt.tools {
					tools = []runner.AgentTool{{Name: "board_state", InputSchema: json.RawMessage(`{"type":"object"}`)}}
				}
				started := time.Now()
				var completedTool bool
				_, err = backend.RunTurnWithTools(ctx, req, tools, nil, func(update runner.AgentUpdate) error {
					if update.AuxiliaryTurn && !update.NonActivity {
						t.Error("unrelated turn update counted as activity")
					}
					if tt.traffic == "repeated output" && update.Type == runner.AgentUpdateMessageDelta && update.NonActivity {
						t.Error("repeated output was omitted from runtime activity")
					}
					if tt.traffic == "replay" && update.Type == runner.AgentUpdateToolCompleted {
						if update.NonActivity != completedTool {
							t.Error("tool completion activity did not distinguish replay")
						}
						completedTool = true
					}
					return nil
				})
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("RunTurnWithTools() = %v, want %v", err, tt.wantErr)
				}
				if elapsed := time.Since(started); elapsed != tt.wantElapsed {
					t.Fatalf("elapsed = %s, want %s", elapsed, tt.wantElapsed)
				}
				params := transport.sentMessages()[2].Params
				if tt.tools && !tt.supplemental {
					assertJSONOmits(t, params, "config")
				} else {
					assertJSONContains(t, params, "config.background_terminal_max_timeout", 3000000)
					var thread struct {
						Instructions string `json:"developerInstructions"`
					}
					if err := json.Unmarshal(params, &thread); err != nil {
						t.Fatal(err)
					}
					for _, required := range []string{"write_stdin", "3000000", "3060000"} {
						if !strings.Contains(thread.Instructions, required) {
							t.Errorf("missing native wait guidance %q", required)
						}
					}
					if strings.Contains(thread.Instructions, "55 seconds") {
						t.Error("retains paid polling cadence")
					}
				}
			})
		})
	}
}

type quietCommandTransport struct {
	*fakeAppServerTransport
	delays    map[int]time.Duration
	nextIndex int
}

func (t *quietCommandTransport) Receive(ctx context.Context) (Message, error) {
	if delay := t.delays[t.nextIndex]; delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-timer.C:
		}
	}
	t.nextIndex++
	return t.fakeAppServerTransport.Receive(ctx)
}

func TestAgentBackendCompletesOnTaskComplete(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		params string
	}{
		{"direct", "task_complete", `{"threadId":"thread-1","turn_id":"turn-1"}`},
		{"legacy envelope", "codex/event/task_complete", `{"id":"turn-1","conversationId":"thread-1","msg":{"type":"task_complete","turn_id":"turn-1"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transport := &blockingAppServerTransport{newFakeAppServerTransport([]Message{
					responseMessage(t, 1, `{"userAgent":"codex-cli/0.155.1"}`),
					rolloutResponseMessage(t, threadStartRequestID, `{"thread":{"id":"thread-1","model":"example-model"}}`, nil),
					responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
					notificationMessage(t, "codex/event/task_complete", `{"id":"old-turn","conversationId":"thread-1","msg":{"type":"task_complete"}}`),
					notificationMessage(t, "task_complete", `{"thread_id":"another-thread","turn_id":"turn-1"}`),
					notificationMessage(t, "task_complete", `{"type":"task_complete"}`),
					notificationMessage(t, "item/agentMessage/delta", `{"threadId":"thread-1","turnId":"turn-1","itemId":"final","delta":"ready"}`),
					notificationMessage(t, test.method, test.params),
				})}
				server, err := NewAppServer(staticTransportFactory{transport}, WithTurnTimeout(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				backend, err := NewAgentBackend(server, Options{})
				if err != nil {
					t.Fatal(err)
				}
				var answer string
				var completions int
				started := time.Now()
				result, err := backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Workspace: t.TempDir(), Prompt: "Complete the turn", DetectToolLoops: true}, []runner.AgentTool{{Name: "work_item", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil, func(update runner.AgentUpdate) error {
					switch update.Type {
					case runner.AgentUpdateMessageDelta:
						answer += update.Delta
					case runner.AgentUpdateTurnCompleted:
						if !update.AuxiliaryTurn {
							completions++
						}
					}
					return nil
				})
				if err != nil || result.TurnID != "turn-1" || answer != "ready" || completions != 1 || time.Since(started) != 0 {
					t.Fatalf("result=%+v err=%v answer=%q completions=%d elapsed=%s", result, err, answer, completions, time.Since(started))
				}
			})
		})
	}
}

func TestInvestigationExhaustsToolBudgetWithinOneTurn(t *testing.T) {
	for _, test := range []struct {
		name                   string
		calls, bytes           int
		fallback, evidenceOnly bool
	}{
		{name: "calls", calls: 1, bytes: 1000}, {name: "result bytes", calls: 10, bytes: 7}, {name: "same thread final turn", calls: 1, bytes: 1000, fallback: true}, {name: "evidence only", calls: 1, bytes: 1000, evidenceOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			decisionJSON := `{"outcome":"reroute","step":"implement","reason":"Repair the assertion","diagnosis":"failed: observed assertion failure","instruction":"Repair the implementation exercised by the failing assertion","evidence":["failed"]}`
			encoded, err := json.Marshal(decisionJSON)
			if err != nil {
				t.Fatal(err)
			}
			transport := newFakeAppServerTransport([]Message{
				responseMessage(t, 1, `{"userAgent":"codex-cli/0.157.0"}`),
				responseMessage(t, 5, `{"config":{}}`),
				responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
				responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
				serverRequestMessage(t, 90, "item/tool/call", `{"tool":"work_item","arguments":{}}`),
				serverRequestMessage(t, 91, "item/tool/call", `{"tool":"work_item","arguments":{}}`),
				serverRequestMessage(t, 92, "item/tool/call", `{"tool":"work_item","arguments":{}}`),
				notificationMessage(t, "item/agentMessage/delta", `{"threadId":"thread-1","turnId":"turn-1","itemId":"final","delta":`+string(encoded)+`}`),
				notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
			})
			if test.fallback {
				transport.received = append(transport.received[:len(transport.received)-2],
					notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
					responseMessage(t, finalTurnStartRequestID, `{"turn":{"id":"turn-2"}}`),
					serverRequestMessage(t, 93, "item/tool/call", `{"tool":"work_item","arguments":{}}`),
					notificationMessage(t, "item/agentMessage/delta", `{"threadId":"thread-1","turnId":"turn-2","itemId":"final","delta":`+string(encoded)+`}`),
					notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-2","status":"completed"}}`),
				)
			}
			server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			server.timeoutContext = func(ctx context.Context, limit time.Duration, cause error) (context.Context, context.CancelFunc) {
				if limit > time.Minute {
					t.Fatalf("provider timeout %s exceeds the end-to-end deadline", limit)
				}
				return context.WithTimeoutCause(ctx, limit, cause)
			}
			backend, err := NewAgentBackend(server, Options{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			budget := runner.NewAgentToolBudget(test.calls, test.bytes)
			reads := 0
			var output strings.Builder
			workspace := "/scratch"
			if test.evidenceOnly {
				workspace = ""
			}
			_, err = backend.RunTurnWithTools(ctx, runner.AgentTurnRequest{Workspace: workspace, TempDir: "/scratch", Model: "example-model", ReadOnly: true, IsolatedTools: true, ToolBudget: budget}, []runner.AgentTool{{Name: "work_item", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(context.Context, runner.AgentToolCall) (runner.AgentToolResult, error) {
				reads++
				return runner.AgentToolResult{Success: true, Content: "four"}, nil
			}, func(update runner.AgentUpdate) error {
				if update.Type == runner.AgentUpdateMessageDelta {
					output.WriteString(update.Delta)
				}
				return nil
			})
			if err != nil || context.Cause(ctx) != nil || output.String() != decisionJSON {
				t.Fatalf("no final decision: output=%q err=%v cause=%v", output.String(), err, context.Cause(ctx))
			}
			wantReads := 1
			if test.bytes == 7 {
				wantReads = 2
			}
			if reads != wantReads {
				t.Fatalf("reads=%d want=%d", reads, wantReads)
			}
			turns := 0
			refused := false
			responses := make(map[string][]Message)
			for _, message := range transport.sent {
				if message.Method == "turn/interrupt" {
					t.Fatal("budget exhaustion canceled the turn")
				}
				if message.Method == "" && len(message.ID) > 0 {
					responses[string(message.ID)] = append(responses[string(message.ID)], message)
				}
				if message.Method == "turn/start" {
					turns++
					assertJSONContains(t, message.Params, "threadId", "thread-1")
					if turns == 2 && !strings.Contains(string(message.Params), "Remaining time:") {
						t.Fatal("finalization prompt omits remaining deadline")
					}
				}
				if message.Method == "thread/resume" {
					t.Fatal("final turn opened a new connection")
				}
				if message.Method == "thread/start" {
					assertJSONContains(t, message.Params, "config.permissions.detent-investigation.filesystem./", "deny")
					assertJSONContains(t, message.Params, "config.project_doc_max_bytes", 0)
					var params struct {
						Config map[string]any `json:"config"`
					}
					if err := json.Unmarshal(message.Params, &params); err != nil {
						t.Fatal(err)
					}
					if params.Config["features.shell_tool"] != false || params.Config["features.unified_exec"] != false {
						t.Fatal("native tools enabled")
					}
					if test.evidenceOnly {
						assertJSONContains(t, message.Params, "cwd", "/scratch")
					}
				}
				if strings.Contains(string(message.Result), "Return your final decision") {
					refused = true
				}
			}
			wantTurns := 1
			if test.fallback {
				wantTurns = 2
			}
			if turns != wantTurns || !refused {
				t.Fatalf("turns=%d refused=%t", turns, refused)
			}
			lastID := 92
			if test.fallback {
				lastID = 93
			}
			for id := 90; id <= lastID; id++ {
				messages := responses[string(requestID(id))]
				if len(messages) != 1 {
					t.Fatalf("request %d received %d responses, want exactly one", id, len(messages))
				}
				var result struct {
					Success bool `json:"success"`
				}
				if err := json.Unmarshal(messages[0].Result, &result); err != nil {
					t.Fatal(err)
				}
				wantSuccess := id == 90
				if result.Success != wantSuccess || (!wantSuccess && !strings.Contains(string(messages[0].Result), runner.InvestigationBudgetMessage)) {
					t.Fatalf("request %d response = %s", id, messages[0].Result)
				}
			}
		})
	}
}

func TestIndependentModelProbeOwnership(t *testing.T) {
	for _, name := range []string{"catalog", "default"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "started")
			ctx := workspace.WithAttemptWorkspace(t.Context(), workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"})
			factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
				return exec.CommandContext(ctx, "git", "config", "--file", marker, "test.executed", "true")
			})
			if err != nil {
				t.Fatal(err)
			}
			server, err := NewAppServer(factory, WithReadTimeout(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			backend, err := NewAgentBackend(server, Options{})
			if err != nil {
				t.Fatal(err)
			}
			proc := runner.AgentProcessRequest{Workspace: root, TempDir: root}
			if name == "catalog" {
				_, err = backend.ListModels(ctx, proc)
			} else {
				_, err = backend.DefaultModel(ctx, proc)
			}
			if err == nil {
				t.Fatal("unowned provider returned success")
			}
			if _, probe := os.Stat(marker); !errors.Is(probe, os.ErrNotExist) {
				t.Errorf("provider command executed without session acknowledgement, error=%v marker=%v", err, probe)
			}
		})
	}
}
