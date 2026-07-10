package codex

import (
	"context"
	"testing"
	"time"

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
	server, err := NewAppServer(staticTransportFactory{transport: transport},
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
		Prompt:             "Ship issue #820",
		Model:              "gpt-5-codex",
		ExtraWritableRoots: []string{"/extra", "/existing", " "},
	}, func(update runner.AgentUpdate) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}

	assertRequest(t, sent[2], 2, "thread/start")
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "workspace-write")
	assertJSONContains(t, sent[2].Params, "model", "gpt-5-codex")

	assertRequest(t, sent[3], 3, "turn/start")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.type", "workspaceWrite")
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.networkAccess", true)
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.writableRoots", []any{"/existing", "/extra"})
	assertJSONContains(t, sent[3].Params, "model", "gpt-5-codex")

	if len(updates) < 2 || updates[0].Type != runner.AgentUpdateRuntimeIdentity || updates[1].Type != runner.AgentUpdateTurnStarted || updates[1].Model != "gpt-5-codex-resolved" {
		t.Fatalf("updates = %#v, want resolved model on turn started", updates)
	}
}
