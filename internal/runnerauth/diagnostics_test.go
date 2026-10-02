package runnerauth

import "testing"

func TestLocalChecksValidation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		checks LocalChecks
		valid  bool
	}{
		{"pending", LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "pending"}, true},
		{"passed", LocalChecks{Checkout: "passed", Doctor: "warning", Provider: "passed", ProviderKinds: []string{"codex", "claude-code"}}, true},
		{"runner backend kinds", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "failed", ProviderKinds: []string{"claude_code", "codex", "pi_agent"}}, true},
		{"duplicate provider", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"claude_code", "claude_code"}}, false},
		{"raw doctor output", LocalChecks{Checkout: "passed", Doctor: "secret-token", Provider: "passed"}, false},
		{"raw provider output", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"account-secret"}}, false},
		{"unbounded provider list", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex", "codex", "codex"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.checks.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate()=%v", err)
			}
		})
	}
}
