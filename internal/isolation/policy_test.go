package isolation

import "testing"

func TestReportSupports(t *testing.T) {
	for _, test := range []struct {
		name   string
		report Report
		tier   string
		want   bool
	}{
		{"absent", nil, Sandbox, false},
		{"sandbox", Report{"codex": {Sandbox, NativeTrusted}}, Sandbox, true},
		{"mixed", Report{"codex": {Sandbox, NativeTrusted}, "claude": {NativeTrusted}}, Sandbox, false},
		{"trusted", Report{"codex": {NativeTrusted}, "claude": {NativeTrusted}}, NativeTrusted, true},
		{"failed backend", Report{"codex": {Sandbox}, "claude": {}}, Sandbox, false},
		{"invalid", Report{"codex": {"container"}}, "container", false},
		{"duplicate", Report{"codex": {Sandbox, Sandbox}}, Sandbox, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.report.Supports(test.tier); got != test.want {
				t.Fatalf("Supports = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPolicyValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy Policy
		want   bool
	}{
		{"sandbox", Policy{Sandbox, []string{"/worktree"}, []string{"unix:/var/run/example.sock"}}, true},
		{"no worktree", Policy{Tier: Sandbox}, false},
		{"root", Policy{Sandbox, []string{"/"}, nil}, false},
		{"relative", Policy{Sandbox, []string{"worktree"}, nil}, false},
		{"tcp", Policy{Sandbox, []string{"/worktree"}, []string{"tcp:127.0.0.1:8080"}}, false},
		{"socket parent", Policy{Sandbox, []string{"/worktree"}, []string{"unix:/var/run/../example.sock"}}, false},
		{"trusted", Policy{Tier: NativeTrusted}, true},
		{"unknown", Policy{Tier: "container"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.policy.Validate(); (err == nil) != test.want {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}
