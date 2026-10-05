//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/serviceapi"
)

// This catches a configured Pi route that constructs the wrong backend, drops
// session/provider identity, or fails to carry final usage into Detent's store.
func TestBuildRunnerSupportsPiAgentRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "pi-fixture")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
read -r state
echo '{"type":"response","id":"state","command":"get_state","success":true,"data":{"sessionId":"pi-session","model":{"id":"fixture-model","provider":"fixture"},"thinkingLevel":"high"}}'
read -r prompt
cat <<'RECORDS'
{"type":"response","id":"prompt","command":"prompt","success":true,"data":{"disposition":"started"}}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{"role":"assistant"}}
{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"pi streamed"}}
{"type":"message_end","message":{"role":"assistant","model":"fixture-model","provider":"fixture","stopReason":"stop","usage":{"input":10,"cacheRead":4,"cacheWrite":3,"output":5,"totalTokens":22}}}
{"type":"agent_end"}
{"type":"agent_settled"}
RECORDS
while read -r record; do :; done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	workflow, err := workflowconfig.ParseWorkflow([]byte(`---
tracker:
  kind: memory
workspace:
  root: ` + strconv.Quote(filepath.Join(dir, "workspaces")) + `
agents:
  backends:
    - id: pi-worker
      kind: pi_agent
      provider: fixture
      command: ` + strconv.Quote(runnerShellQuote(script)) + `
      options:
        thinking_level: high
  routes:
    - name: pi-code
      backend: pi-worker
      model: fixture-model
      default: true
---
Prompt {{ issue.identifier }}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	sessionStore := &runnerSessionStore{sessionID: 2469}
	deps, err := buildRunnerDependencies(workflow, "detent", initRunnerSourceRepo(t), globalconfig.Memory{}, sessionStore, nil, serviceapi.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	deps.ReapWorkspaceProcesses = func(context.Context, string, time.Duration) (int, error) { return 0, nil }
	run, err := runnerpkg.NewRunner(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Run(t.Context(), runnerpkg.RunRequest{Issue: connector.Issue{ID: "issue-2469", Identifier: "digitaldrywood/detent#2469", BranchName: "detent/issue-2469"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalState != runnerpkg.FinalStateCompleted || result.Output != "pi streamed" || result.Tokens.InputTokens != 17 || result.Tokens.TotalTokens != 22 {
		t.Fatalf("result = %#v", result)
	}
	if result.RuntimeIdentity.BackendKind != workflowconfig.AgentBackendPiAgent || result.RuntimeIdentity.Provider.Value != "fixture" || result.RuntimeIdentity.ResolvedModel.Value != "fixture-model" || result.RuntimeIdentity.ReasoningEffort.Value != "high" {
		t.Fatalf("identity = %#v", result.RuntimeIdentity)
	}
	if sessionStore.phase.EndpointFamily != "pi_agent" || sessionStore.phase.TotalTokens != 22 || sessionStore.finished.ProviderSessionID != "pi-session" {
		t.Fatalf("store phase = %#v, finish = %#v", sessionStore.phase, sessionStore.finished)
	}
}

// Native-trusted capability may be advertised after a version probe; Pi must
// never report sandbox support merely because its CLI exists.
func TestProbePiIsolation(t *testing.T) {
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendPiAgent, Command: "printf fixture-version", Protocol: "rpc"}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.NativeTrusted}); err != nil {
		t.Fatal(err)
	}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.Sandbox}); !errors.Is(err, isolation.ErrSandboxUnavailable) {
		t.Fatalf("sandbox = %v", err)
	}
}
