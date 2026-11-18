package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestAntigravityWorkflow(t *testing.T) {
	for _, test := range []struct{ name, protocol, options, want string }{
		{name: "defaults", options: "home: /srv/agy-home"},
		{name: "protocol", protocol: "protocol: headless", options: "home: /srv/agy-home", want: "protocol must be stream-json"},
		{name: "missing home", options: "effort: high", want: "home must be an absolute path"},
		{name: "relative home", options: "home: agy-home", want: "home must be an absolute path"},
		{name: "timeout", options: "home: /srv/agy-home\n        turn_timeout_ms: -1", want: "turn_timeout_ms must be greater"},
		{name: "stall", options: "home: /srv/agy-home\n        stall_timeout_ms: -1", want: "stall_timeout_ms must be greater"},
		{name: "effort", options: "home: /srv/agy-home\n        effort: extreme", want: "effort must be one of"},
		{name: "valid effort", options: "home: /srv/agy-home\n        effort: XHigh"},
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
      options:
        %s
  routes:
    - name: antigravity-code
      role: code
      backend: implementation
      model: fixture-model
      default: true
---
Prompt
`, test.protocol, test.options)))
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
			if len(backends) != 1 || backends[0].Kind != AgentBackendAntigravity || backends[0].Protocol != "stream-json" || backends[0].Command != "agy" || backends[0].Provider != "fixture" || backends[0].AntigravityOptions().Home != "/srv/agy-home" {
				t.Fatalf("backends = %#v", backends)
			}
		})
	}
}
