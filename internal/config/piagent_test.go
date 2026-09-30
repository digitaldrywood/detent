package config

import (
	"fmt"
	"strings"
	"testing"
)

// Prevent accepting unsupported protocols/tool names and losing Pi defaults
// while a workflow is normalized and routed through the existing agent schema.
func TestPiAgentWorkflow(t *testing.T) {
	for _, test := range []struct{ name, extra, want string }{
		{name: "defaults"},
		{name: "options", extra: "options:\n        thinking_level: HIGH\n        tools: [read, bash]\n        session_dir: /sessions\n        turn_timeout_ms: 1000"},
		{name: "protocol", extra: "protocol: headless", want: "protocol must be rpc"},
		{name: "thinking", extra: "options:\n        thinking_level: invalid", want: "thinking_level must be one of"},
		{name: "tools", extra: "options:\n        tools: [custom]", want: "tools must contain only Pi built-in tools"},
		{name: "timeout", extra: "options:\n        turn_timeout_ms: -1", want: "turn_timeout_ms must be greater"},
		{name: "stall", extra: "options:\n        stall_timeout_ms: -1", want: "stall_timeout_ms must be greater"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte(fmt.Sprintf(`---
tracker:
  kind: memory
agents:
  backends:
    - id: implementation
      kind: PI_AGENT
      provider: fixture
      %s
  routes:
    - name: pi-code
      role: code
      backend: implementation
      model: fixture-model
      default: true
---
Prompt
`, test.extra)))
			if err == nil {
				err = workflow.Config.Validate()
			}
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("error = %v, want %q", err, test.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			backends := workflow.Config.AgentBackendConfigs()
			if len(backends) != 1 || backends[0].Kind != AgentBackendPiAgent || backends[0].Protocol != "rpc" || backends[0].Command != "pi" || backends[0].Provider != "fixture" {
				t.Fatalf("backends = %#v", backends)
			}
			if test.name == "options" {
				o := backends[0].PiAgentOptions()
				if o.ThinkingLevel != "high" || len(o.Tools) != 2 || o.SessionDir != "/sessions" || o.TurnTimeoutMS != 1000 {
					t.Fatalf("options = %#v", o)
				}
			}
		})
	}
}
