package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestAntigravityWorkflow(t *testing.T) {
	for _, test := range []struct{ name, extra, want string }{
		{name: "defaults"},
		{name: "protocol", extra: "protocol: headless", want: "protocol must be stream-json"},
		{name: "timeout", extra: "options:\n        turn_timeout_ms: -1", want: "turn_timeout_ms must be greater"},
		{name: "stall", extra: "options:\n        stall_timeout_ms: -1", want: "stall_timeout_ms must be greater"},
		{name: "effort", extra: "options:\n        effort: extreme", want: "effort must be one of"},
		{name: "valid effort", extra: "options:\n        effort: XHigh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte(fmt.Sprintf(`---
tracker:
  kind: hub_native
agents:
  backends:
    - id: implementation
      kind: ANTIGRAVITY
      provider: fixture
      %s
  routes:
    - name: antigravity-code
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
			if len(backends) != 1 || backends[0].Kind != AgentBackendAntigravity || backends[0].Protocol != "stream-json" || backends[0].Command != "agy" || backends[0].Provider != "fixture" {
				t.Fatalf("backends = %#v", backends)
			}
		})
	}
}
