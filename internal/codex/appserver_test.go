package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
)

func TestAppServerRunTurnStartsLifecycleAndStreamsUpdates(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"},"model":"gpt-5-codex-resolved","modelProvider":"openai","reasoningEffort":"xhigh","serviceTier":"priority"}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "item/agentMessage/delta", `{
			"threadId":"thread-1",
			"turnId":"turn-1",
			"itemId":"item-1",
			"delta":"hello"
		}`),
		notificationMessage(t, "thread/tokenUsage/updated", `{
			"threadId":"thread-1",
			"turnId":"turn-1",
			"tokenUsage":{
				"total":{
					"inputTokens":20,
					"cachedInputTokens":5,
					"outputTokens":7,
					"reasoningOutputTokens":3,
					"totalTokens":27
				},
				"last":{
					"inputTokens":2,
					"cachedInputTokens":1,
					"outputTokens":3,
					"reasoningOutputTokens":1,
					"totalTokens":5
				},
				"modelContextWindow":200000
			}
		}`),
		notificationMessage(t, "account/rateLimits/updated", `{
			"rateLimits":{
				"limitId":"codex-primary",
				"limitName":"Codex primary",
				"primary":{
					"usedPercent":12.5,
					"windowDurationMins":300,
					"resetsAt":1780000000
				},
				"secondary":null,
				"credits":{
					"hasCredits":true,
					"unlimited":false,
					"balance":"7.25"
				},
				"planType":null,
				"rateLimitReachedType":null
			}
		}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	transport.processIdentity = "4242"
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithClientInfo(ClientInfo{
			Name:    "detent-test",
			Title:   "Detent Test",
			Version: "0.1.0",
		}),
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	result, err := server.RunTurn(context.Background(), RunTurnRequest{
		Workspace:         "/tmp/detent-workspace",
		Prompt:            "Ship issue #18",
		ApprovalPolicy:    json.RawMessage(`"never"`),
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: json.RawMessage(`{"type":"workspaceWrite","networkAccess":true}`),
		ModelProvider:     "local_ollama",
		ServiceTier:       "priority",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if result.ThreadID != "thread-1" || result.TurnID != "turn-1" || result.SessionID != "thread-1-turn-1" {
		t.Fatalf("RunTurn() result = %#v", result)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}

	assertRequest(t, sent[0], 1, "initialize")
	assertJSONContains(t, sent[0].Params, "clientInfo.name", "detent-test")
	assertJSONContains(t, sent[0].Params, "capabilities.experimentalApi", true)

	if sent[1].Method != "initialized" || len(sent[1].ID) != 0 {
		t.Fatalf("sent[1] = %#v, want initialized notification", sent[1])
	}

	assertRequest(t, sent[2], 2, "thread/start")
	assertJSONContains(t, sent[2].Params, "cwd", "/tmp/detent-workspace")
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "workspace-write")
	assertJSONContains(t, sent[2].Params, "modelProvider", "local_ollama")
	assertJSONContains(t, sent[2].Params, "serviceTier", "priority")

	assertRequest(t, sent[3], 3, "turn/start")
	assertJSONContains(t, sent[3].Params, "threadId", "thread-1")
	assertJSONContains(t, sent[3].Params, "input.0.type", "text")
	assertJSONContains(t, sent[3].Params, "input.0.text", "Ship issue #18")
	assertJSONContains(t, sent[3].Params, "cwd", "/tmp/detent-workspace")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.type", "workspaceWrite")
	assertJSONContains(t, sent[3].Params, "serviceTier", "priority")

	if len(updates) != 7 {
		t.Fatalf("updates = %d, want 7: %#v", len(updates), updates)
	}
	if updates[0].Type != UpdateProcessStarted || updates[0].ProcessIdentity != "4242" {
		t.Fatalf("updates[0] = %#v, want process identity", updates[0])
	}
	if updates[1].Type != UpdateRuntimeIdentity || updates[1].RuntimeIdentity.Provider.Value != "openai" || updates[1].RuntimeIdentity.ReasoningEffort.Value != "xhigh" || updates[1].RuntimeIdentity.ServiceTier.Value != "priority" {
		t.Fatalf("updates[1] = %#v, want runtime identity", updates[1])
	}
	if updates[2].Type != UpdateTurnStarted || updates[2].ThreadID != "thread-1" || updates[2].TurnID != "turn-1" || updates[2].Model != "gpt-5-codex-resolved" {
		t.Fatalf("updates[2] = %#v, want turn started", updates[2])
	}
	if updates[3].Type != UpdateAgentMessageDelta || updates[3].Delta != "hello" {
		t.Fatalf("updates[3] = %#v, want agent message delta", updates[3])
	}
	if updates[4].Type != UpdateTokenUsage || updates[4].Tokens.TotalTokens != 27 {
		t.Fatalf("updates[4] = %#v, want token usage total 27", updates[4])
	}
	if updates[4].Tokens.CachedInputTokens != 5 || updates[4].Tokens.ReasoningOutputTokens != 3 {
		t.Fatalf("updates[4].Tokens = %#v", updates[4].Tokens)
	}
	if updates[4].Tokens.ModelContextWindow == nil || *updates[4].Tokens.ModelContextWindow != 200000 {
		t.Fatalf("updates[4].Tokens.ModelContextWindow = %#v", updates[4].Tokens.ModelContextWindow)
	}
	if updates[5].Type != UpdateRateLimits || updates[5].RateLimits == nil {
		t.Fatalf("updates[5] = %#v, want rate limits", updates[5])
	}
	if updates[5].RateLimits.LimitID != "codex-primary" || updates[5].RateLimits.Primary == nil {
		t.Fatalf("updates[5].RateLimits = %#v", updates[5].RateLimits)
	}
	if updates[5].RateLimits.Primary.UsedPercent != 12.5 {
		t.Fatalf("Primary.UsedPercent = %f, want 12.5", updates[5].RateLimits.Primary.UsedPercent)
	}
	if updates[6].Type != UpdateTurnCompleted || updates[6].TurnID != "turn-1" {
		t.Fatalf("updates[6] = %#v, want turn completed", updates[6])
	}
}

func TestAppServerRunTurnResumesThreadBeforeStartingTurn(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.142.5"}`),
		responseMessage(t, 4, `{"thread":{"id":"thread-existing","model":"gpt-5-codex-resumed"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-2"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-existing","turn":{"id":"turn-2","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	result, err := server.RunTurn(context.Background(), RunTurnRequest{
		Workspace:      "/tmp/detent-workspace",
		Prompt:         "Continue issue #18",
		ResumeThreadID: "thread-existing",
		Model:          "gpt-5-codex",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ThreadID != "thread-existing" || result.TurnID != "turn-2" || result.SessionID != "thread-existing-turn-2" {
		t.Fatalf("RunTurn() result = %#v, want resumed thread and new turn", result)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}
	assertRequest(t, sent[0], 1, "initialize")
	if sent[1].Method != "initialized" || len(sent[1].ID) != 0 {
		t.Fatalf("sent[1] = %#v, want initialized notification", sent[1])
	}
	assertRequest(t, sent[2], 4, "thread/resume")
	assertJSONContains(t, sent[2].Params, "threadId", "thread-existing")
	assertJSONContains(t, sent[2].Params, "cwd", "/tmp/detent-workspace")
	assertJSONContains(t, sent[2].Params, "model", "gpt-5-codex")
	assertRequest(t, sent[3], 3, "turn/start")
	assertJSONContains(t, sent[3].Params, "threadId", "thread-existing")
	assertJSONContains(t, sent[3].Params, "input.0.text", "Continue issue #18")

	if len(updates) != 3 {
		t.Fatalf("updates = %d, want identity, turn started, and completed: %#v", len(updates), updates)
	}
	if updates[0].Type != UpdateRuntimeIdentity || updates[0].Model != "gpt-5-codex-resumed" {
		t.Fatalf("updates[0] = %#v, want resumed runtime identity", updates[0])
	}
	if updates[1].Type != UpdateTurnStarted || updates[1].ThreadID != "thread-existing" || updates[1].TurnID != "turn-2" || updates[1].Model != "gpt-5-codex-resumed" {
		t.Fatalf("updates[1] = %#v, want resumed turn started", updates[1])
	}
}

func TestAppServerRunTurnUsesConfigReadModelWhenThreadResponseOmitsModel(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.143.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/detent-workspace",
		Prompt:    "Ship issue #1103",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 5 {
		t.Fatalf("sent messages = %d, want 5", len(sent))
	}
	assertRequest(t, sent[2], 2, "thread/start")
	assertRequest(t, sent[3], 5, "config/read")
	assertJSONContains(t, sent[3].Params, "cwd", "/tmp/detent-workspace")
	assertRequest(t, sent[4], 3, "turn/start")

	if len(updates) != 3 {
		t.Fatalf("updates = %d, want configured identity, turn started, and completed: %#v", len(updates), updates)
	}
	if updates[0].Type != UpdateRuntimeIdentity || updates[0].Model != "gpt-5.6" || updates[0].RuntimeIdentity.ResolvedModel.Provenance != "configured" {
		t.Fatalf("updates[0] = %#v, want configured fallback identity", updates[0])
	}
	if updates[1].Type != UpdateTurnStarted || updates[1].Model != "gpt-5.6" {
		t.Fatalf("updates[1] = %#v, want config-read fallback model", updates[1])
	}
}

func TestAppServerRunTurnContinuesWhenConfigReadFails(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.143.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		errorResponseMessage(t, 5, -32602, "invalid config/read params"),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/detent-workspace",
		Prompt:    "Ship issue #1103",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want config/read failure to be non-fatal", err)
	}

	if len(updates) != 2 {
		t.Fatalf("updates = %d, want turn started and completed: %#v", len(updates), updates)
	}
	if updates[0].Type != UpdateTurnStarted || updates[0].Model != "" {
		t.Fatalf("updates[0] = %#v, want unpriced fallback turn started", updates[0])
	}
}

func TestAppServerRunTurnPreservesTurnStartedRuntimeModel(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.143.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"gpt-5.5"}}`),
		notificationMessage(t, "turn/started", `{"threadId":"thread-1","turn":{"id":"turn-1","model":"gpt-5.6"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/detent-workspace",
		Prompt:    "Ship issue #1103",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var started []Update
	for _, update := range updates {
		if update.Type == UpdateTurnStarted {
			started = append(started, update)
		}
	}
	if len(started) != 1 {
		t.Fatalf("started updates = %d, want only app-server turn/started event: %#v", len(started), started)
	}
	if started[0].Model != "gpt-5.6" {
		t.Fatalf("started[0].Model = %q, want runtime model", started[0].Model)
	}
}

func TestUpdateFromMessageCapturesModelReroute(t *testing.T) {
	t.Parallel()

	update, ok, err := updateFromMessage(notificationMessage(t, "model/rerouted", `{
		"threadId":"thread-1",
		"turnId":"turn-1",
		"fromModel":"gpt-5.5",
		"toModel":"gpt-5.6",
		"reason":"highRiskCyberActivity"
	}`))
	if err != nil {
		t.Fatalf("updateFromMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("updateFromMessage() ok = false, want true")
	}
	if update.Type != UpdateModelUpdated || update.ThreadID != "thread-1" || update.TurnID != "turn-1" || update.Model != "gpt-5.6" {
		t.Fatalf("update = %#v, want model reroute update", update)
	}
	if got := update.RuntimeIdentity.ResolvedModel; got != (agentidentity.Value{Value: "gpt-5.6", Provenance: agentidentity.ProvenanceRuntime}) {
		t.Fatalf("runtime resolved model = %#v, want gpt-5.6 runtime", got)
	}
}

func TestUpdateFromMessageCapturesThreadSettingsIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		payload      string
		wantModel    agentidentity.Value
		wantProvider agentidentity.Value
		wantEffort   agentidentity.Value
		wantTier     agentidentity.Value
	}{
		{
			name:         "known runtime settings",
			payload:      `{"threadId":"thread-1","threadSettings":{"model":"gpt-5.6-sol","modelProvider":"local_ollama","effort":"xhigh","serviceTier":"priority"}}`,
			wantModel:    agentidentity.Value{Value: "gpt-5.6-sol", Provenance: agentidentity.ProvenanceRuntime},
			wantProvider: agentidentity.Value{Value: "local_ollama", Provenance: agentidentity.ProvenanceRuntime},
			wantEffort:   agentidentity.Value{Value: "xhigh", Provenance: agentidentity.ProvenanceRuntime},
			wantTier:     agentidentity.Value{Value: "priority", Provenance: agentidentity.ProvenanceRuntime},
		},
		{
			name:         "optional settings unavailable",
			payload:      `{"threadId":"thread-1","threadSettings":{"model":"gpt-5.6-sol","modelProvider":"openai"}}`,
			wantModel:    agentidentity.Value{Value: "gpt-5.6-sol", Provenance: agentidentity.ProvenanceRuntime},
			wantProvider: agentidentity.Value{Value: "openai", Provenance: agentidentity.ProvenanceRuntime},
			wantEffort:   agentidentity.UnknownValue(),
			wantTier:     agentidentity.UnknownValue(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			update, ok, err := updateFromMessage(notificationMessage(t, "thread/settings/updated", tt.payload))
			if err != nil {
				t.Fatalf("updateFromMessage() error = %v", err)
			}
			if !ok || update.Type != UpdateRuntimeIdentity || update.ThreadID != "thread-1" {
				t.Fatalf("update = %#v, want runtime identity update", update)
			}
			identity := update.RuntimeIdentity
			if identity.ResolvedModel != tt.wantModel || identity.Provider != tt.wantProvider || identity.ReasoningEffort != tt.wantEffort || identity.ServiceTier != tt.wantTier {
				t.Fatalf("runtime identity = %#v, want model=%#v provider=%#v effort=%#v tier=%#v", identity, tt.wantModel, tt.wantProvider, tt.wantEffort, tt.wantTier)
			}
		})
	}
}

func TestAppServerRunTurnRequestTurnTimeoutOverridesDefault(t *testing.T) {
	t.Parallel()

	transport := newBlockingAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-timeout"}}`),
		responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-timeout"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	startedAt := time.Now()
	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace:   "/tmp/detent-workspace",
		Prompt:      "timeout",
		TurnTimeout: 10 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("RunTurn() error = nil, want request timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunTurn() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("RunTurn() elapsed = %v, want request timeout instead of default", elapsed)
	}
}

func TestAppServerRunTurnRespondsToServerRequests(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		responseMessage(t, 5, `{"config":{"model":"gpt-5.6"}}`),
		serverRequestMessage(t, 40, "item/tool/requestUserInput", `{"threadId":"thread-1","turnId":"turn-1"}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		serverRequestMessage(t, 41, "item/commandExecution/requestApproval", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 42, "item/fileChange/requestApproval", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 43, "item/permissions/requestApproval", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 44, "mcpServer/elicitation/request", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 45, "custom/request", `{"threadId":"thread-1","turnId":"turn-1"}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/detent-workspace",
		Prompt:    "Ship issue #18",
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 11 {
		t.Fatalf("sent messages = %d, want 11: %#v", len(sent), sent)
	}

	assertRequest(t, sent[3], 5, "config/read")
	assertResponseResultContains(t, sent[5], 40, "answers", map[string]any{})
	assertResponseResultContains(t, sent[6], 41, "decision", "decline")
	assertResponseResultContains(t, sent[7], 42, "decision", "decline")
	assertResponseResultContains(t, sent[8], 43, "permissions", map[string]any{})
	assertResponseResultContains(t, sent[9], 44, "action", "decline")
	assertResponseResultContains(t, sent[9], 44, "content", nil)
	assertErrorResponse(t, sent[10], 45, methodNotFoundCode, "unsupported server request: custom/request")
}

func TestAppServerRunTurnReportsResponseErrors(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		errorResponseMessage(t, 1, -32000, "initialize failed"),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport})
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/detent-workspace",
		Prompt:    "Ship issue #18",
	}, nil)
	if err == nil {
		t.Fatal("RunTurn() error = nil, want response error")
	}
	if !errors.Is(err, ErrResponseError) {
		t.Fatalf("RunTurn() error = %v, want ErrResponseError", err)
	}
}

func TestAppServerRunTurnReportsTurnErrorBody(t *testing.T) {
	t.Parallel()

	backendError := `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5-codex' model is not supported when using Codex with a ChatGPT account."}}`
	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.142.5"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1","model":"gpt-5-codex"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", backendError),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/detent-workspace",
		Prompt:    "Ship issue #927",
		Model:     "gpt-5-codex",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("RunTurn() error = %v, want ErrTurnFailed", err)
	}
	var turnErr *TurnFailedError
	if !errors.As(err, &turnErr) {
		t.Fatalf("RunTurn() error = %T, want TurnFailedError", err)
	}
	if turnErr.BackendErrorBody() != backendError {
		t.Fatalf("BackendErrorBody = %q, want %q", turnErr.BackendErrorBody(), backendError)
	}
	if !strings.Contains(err.Error(), backendError) {
		t.Fatalf("RunTurn() error = %v, want backend body", err)
	}
	if len(updates) != 3 {
		t.Fatalf("updates = %#v, want identity, turn started, and failed turn completed", updates)
	}
	if updates[2].Status != "failed" || updates[2].BackendErrorBody != backendError {
		t.Fatalf("failed update = %#v, want status failed with backend error", updates[2])
	}
}

type staticTransportFactory struct {
	transport Transport
}

func (f staticTransportFactory) NewTransport(context.Context) (Transport, error) {
	return f.transport, nil
}

type fakeAppServerTransport struct {
	received        []Message
	sent            []Message
	processIdentity string
}

func newFakeAppServerTransport(received []Message) *fakeAppServerTransport {
	return &fakeAppServerTransport{received: append([]Message(nil), received...)}
}

func (t *fakeAppServerTransport) Send(_ context.Context, msg Message) error {
	t.sent = append(t.sent, msg)
	return nil
}

func (t *fakeAppServerTransport) Receive(context.Context) (Message, error) {
	if len(t.received) == 0 {
		return Message{}, io.EOF
	}
	msg := t.received[0]
	t.received = t.received[1:]
	return msg, nil
}

func (t *fakeAppServerTransport) Close(context.Context) error {
	return nil
}

func (t *fakeAppServerTransport) ProcessIdentity() string {
	return t.processIdentity
}

func (t *fakeAppServerTransport) sentMessages() []Message {
	return append([]Message(nil), t.sent...)
}

type blockingAppServerTransport struct {
	*fakeAppServerTransport
}

func newBlockingAppServerTransport(received []Message) *blockingAppServerTransport {
	return &blockingAppServerTransport{fakeAppServerTransport: newFakeAppServerTransport(received)}
}

func (t *blockingAppServerTransport) Receive(ctx context.Context) (Message, error) {
	if len(t.received) > 0 {
		msg := t.received[0]
		t.received = t.received[1:]
		return msg, nil
	}
	<-ctx.Done()
	return Message{}, ctx.Err()
}

func responseMessage(t *testing.T, id int, result string) Message {
	t.Helper()

	return Message{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(mustMarshalJSON(t, id)),
		Result:  json.RawMessage(result),
	}
}

func errorResponseMessage(t *testing.T, id int, code int, message string) Message {
	t.Helper()

	return Message{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(mustMarshalJSON(t, id)),
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
}

func notificationMessage(t *testing.T, method string, params string) Message {
	t.Helper()

	return Message{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  json.RawMessage(params),
	}
}

func serverRequestMessage(t *testing.T, id int, method string, params string) Message {
	t.Helper()

	msg := notificationMessage(t, method, params)
	msg.ID = json.RawMessage(mustMarshalJSON(t, id))
	return msg
}

func assertRequest(t *testing.T, msg Message, id int, method string) {
	t.Helper()

	if msg.Method != method {
		t.Fatalf("Method = %q, want %q", msg.Method, method)
	}
	if string(msg.ID) != mustMarshalJSON(t, id) {
		t.Fatalf("ID = %s, want %d", msg.ID, id)
	}
	if len(msg.Params) == 0 {
		t.Fatalf("Params empty for %s", method)
	}
}

func assertResponseResultContains(t *testing.T, msg Message, id int, path string, want any) {
	t.Helper()

	assertResponseID(t, msg, id)
	if msg.Error != nil {
		t.Fatalf("Error = %#v, want result", msg.Error)
	}
	if len(msg.Result) == 0 {
		t.Fatalf("Result empty for response %d", id)
	}
	assertJSONContains(t, msg.Result, path, want)
}

func assertErrorResponse(t *testing.T, msg Message, id int, code int, message string) {
	t.Helper()

	assertResponseID(t, msg, id)
	if msg.Error == nil {
		t.Fatalf("Error = nil, want code %d", code)
	}
	if msg.Error.Code != code || msg.Error.Message != message {
		t.Fatalf("Error = %#v, want code %d message %q", msg.Error, code, message)
	}
}

func assertResponseID(t *testing.T, msg Message, id int) {
	t.Helper()

	if msg.Method != "" {
		t.Fatalf("Method = %q, want response", msg.Method)
	}
	if string(msg.ID) != mustMarshalJSON(t, id) {
		t.Fatalf("ID = %s, want %d", msg.ID, id)
	}
}

func assertJSONContains(t *testing.T, data json.RawMessage, path string, want any) {
	t.Helper()

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}

	got := lookupJSONPath(t, decoded, path)
	if !jsonValuesEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v in %s", path, got, want, string(data))
	}
}

func lookupJSONPath(t *testing.T, value any, path string) any {
	t.Helper()

	parts := strings.Split(path, ".")
	current := value
	for _, part := range parts {
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				t.Fatalf("path %q missing key %q in %#v", path, part, value)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				t.Fatalf("path %q invalid index %q in %#v", path, part, value)
			}
			current = node[index]
		default:
			t.Fatalf("path %q hit non-container %#v", path, current)
		}
	}

	return current
}

func jsonValuesEqual(got any, want any) bool {
	gotData, err := json.Marshal(got)
	if err != nil {
		return false
	}
	wantData, err := json.Marshal(want)
	if err != nil {
		return false
	}
	var gotCanonical any
	if err := json.Unmarshal(gotData, &gotCanonical); err != nil {
		return false
	}
	var wantCanonical any
	if err := json.Unmarshal(wantData, &wantCanonical); err != nil {
		return false
	}
	return reflect.DeepEqual(gotCanonical, wantCanonical)
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}
