package codex

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/runner"
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
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.writableRoots", []any{"/existing", "/extra"})
	assertJSONContains(t, sent[3].Params, "model", "gpt-5-codex")
	assertJSONContains(t, sent[3].Params, "effort", "high")

	if len(updates) < 2 || updates[0].Type != runner.AgentUpdateRuntimeIdentity || updates[1].Type != runner.AgentUpdateTurnStarted || updates[1].Model != "gpt-5-codex-resolved" {
		t.Fatalf("updates = %#v, want resolved model on turn started", updates)
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

func TestAgentBackendEnforcesConfiguredStallTimeout(t *testing.T) {
	t.Parallel()

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

	factory := newControlledTimeoutFactory()
	server.timeoutContext = factory.context
	err = runWithTimeoutExpiration(t, factory, 10*time.Millisecond, ErrStreamStalled, func() error {
		_, runErr := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
			Workspace: "/tmp/detent-workspace",
			Prompt:    "stall",
		}, nil)
		return runErr
	})
	if !errors.Is(err, ErrStreamStalled) {
		t.Fatalf("RunTurn() error = %v, want configured ErrStreamStalled", err)
	}
}

func TestAgentBackendToolTurnUsesRestrictedSandbox(t *testing.T) {
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
	}}, nil, nil)
	if err != nil {
		t.Fatalf("RunTurnWithTools() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "read-only")
	assertJSONContains(t, sent[2].Params, "dynamicTools.0.name", "board_state")
	assertJSONContains(t, sent[2].Params, "developerInstructions", "Inspect only and submit proposals.")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "effort", "high")
	assertJSONOmits(t, sent[3].Params, "sandboxPolicy")
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

type workerTempCapturingTransportFactory struct {
	transport Transport
	tempDir   string
}

func (f *workerTempCapturingTransportFactory) NewTransport(ctx context.Context) (Transport, error) {
	f.tempDir = workerTempDir(ctx)
	return f.transport, nil
}
