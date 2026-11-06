package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestOpenCodeWorkflow(t *testing.T) {
	for _, test := range []struct{ name, extra, want string }{
		{name: "defaults"},
		{name: "protocol", extra: "protocol: headless", want: "protocol must be acp"},
		{name: "timeout", extra: "options:\n        turn_timeout_ms: -1", want: "turn_timeout_ms must be greater"},
		{name: "stall", extra: "options:\n        stall_timeout_ms: -1", want: "stall_timeout_ms must be greater"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte(fmt.Sprintf(`---
tracker:
  kind: hub_native
agents:
  backends:
    - id: implementation
      kind: OPENCODE
      provider: fixture
      %s
  routes:
    - name: opencode-code
      role: code
      backend: implementation
      model: fixture/model
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
			if len(backends) != 1 || backends[0].Kind != AgentBackendOpenCode || backends[0].Protocol != "acp" || backends[0].Command != "opencode" || backends[0].Provider != "fixture" {
				t.Fatalf("backends = %#v", backends)
			}
		})
	}
}
