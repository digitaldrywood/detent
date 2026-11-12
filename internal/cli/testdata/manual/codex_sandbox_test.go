package cli

import (
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestProbeBackendCodexEnforcesSandbox(t *testing.T) {
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: "codex app-server"}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.Sandbox}); err != nil {
		t.Fatalf("real Codex filesystem and network enforcement probe: %v", err)
	}
}
