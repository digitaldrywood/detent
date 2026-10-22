package runnerauth

import "testing"

func TestLocalChecksValidation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		checks LocalChecks
		valid  bool
	}{
		{"runner commit policy", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", CommitName: "Enrolled runner", CommitEmail: "machine@runners.detent.invalid", CommitSigning: "unsigned"}, true},
		{"invalid signing mode", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", CommitSigning: "pinentry"}, false},
		{"multiline commit identity", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", CommitName: "Runner\nother"}, false},
		{"running configuration", LocalChecks{Checkout: "passed", Configuration: "running", Doctor: "pending", Provider: "pending"}, true},
		{"raw configuration", LocalChecks{Checkout: "passed", Configuration: "secret-token", Doctor: "pending", Provider: "pending"}, false},
		{"pending", LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "pending"}, true},
		{"setup failed", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", Setup: "failed"}, true},
		{"raw setup output", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", Setup: "secret-token"}, false},
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
			if tt.checks.Setup == "failed" && tt.checks.Passed() {
				t.Fatal("failed setup reported readiness")
			}
		})
	}
}

func TestLocalChecksWorkReady(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		checks LocalChecks
		ready  bool
	}{
		{"verified provider", LocalChecks{Checkout: "passed", Doctor: "warning", Provider: "passed", ProviderKinds: []string{"codex"}}, true},
		{"Pi has no authentication status", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "pending", ProviderKinds: []string{"pi_agent"}}, true},
		{"Pi with verified Codex", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "pending", ProviderKinds: []string{"codex", "pi_agent"}}, true},
		{"no observed provider", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "pending"}, false},
		{"signed-out provider beside Pi", LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "failed", ProviderKinds: []string{"codex", "pi_agent"}}, false},
		{"checkout not verified", LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "passed", ProviderKinds: []string{"codex"}}, false},
		{"missing configuration", LocalChecks{Configuration: "missing", Checkout: "passed", Doctor: "passed", Provider: "passed"}, false},
		{"setup failed", LocalChecks{Setup: "failed", Checkout: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.checks.WorkReady(); got != tt.ready {
				t.Fatalf("WorkReady()=%t, want %t", got, tt.ready)
			}
		})
	}
}
