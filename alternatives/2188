package onboarding

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestProjectReadiness(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                                      string
		policy, doctor, provider, runner, binding bool
		artifacts                                 string
		exclusions                                []runnerauth.Exclusion
		want                                      bool
	}{
		{name: "interrupted"},
		{name: "missing policy", doctor: true, provider: true, runner: true, artifacts: "local"},
		{name: "missing provider", policy: true, doctor: true, runner: true, artifacts: "local"},
		{name: "missing runner", policy: true, doctor: true, provider: true, artifacts: "local"},
		{name: "missing gateway", policy: true, doctor: true, provider: true, runner: true, artifacts: "customer"},
		{name: "local ready", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", want: true},
		{name: "host full", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", exclusions: []runnerauth.Exclusion{{Code: "host_capacity", Message: "Shared host capacity is full or paused"}}, want: true},
		{name: "runner full", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", exclusions: []runnerauth.Exclusion{{Code: "runner_capacity", Message: "Runner capacity is full or paused"}}, want: true},
		{name: "host and runner full", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", exclusions: []runnerauth.Exclusion{{Code: "host_capacity"}, {Code: "runner_capacity"}}, want: true},
		{name: "requirements unmatched", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", exclusions: []runnerauth.Exclusion{{Code: "selector_no_match", Message: "Runner does not match required tags"}}},
		{name: "requirements unmatched and capacity full", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", exclusions: []runnerauth.Exclusion{{Code: "host_capacity"}, {Code: "runner_capacity"}, {Code: "selector_no_match"}}},
		{name: "customer configured", policy: true, doctor: true, provider: true, runner: true, binding: true, artifacts: "customer", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := Project{Progress: Progress{Repository: "existing", Doctor: tt.doctor, Provider: tt.provider, Artifacts: tt.artifacts}}
			if tt.policy {
				p.Policy = &policy.Approval{}
			}
			if tt.runner {
				p.Runners = []runnerauth.Eligibility{{Runner: runnerauth.Runner{Routing: runnerauth.Routing{State: "active"}, Health: "online"}, LocalChecks: &runnerauth.LocalChecks{Checkout: "passed", Doctor: "failed", Provider: "failed"}}}
				p.Runners[0].Exclusions = slices.Clone(tt.exclusions)
				if tt.doctor {
					p.Runners[0].LocalChecks.Doctor = "passed"
				}
				if tt.provider {
					p.Runners[0].LocalChecks.Provider = "passed"
				}
			}
			if tt.binding {
				p.Artifacts = []artifact.Binding{{Mode: "customer"}}
			}
			p.Evaluate()
			p.Evaluate()
			if p.Ready != tt.want || len(p.Steps) != 4 {
				t.Fatalf("readiness = %+v", p)
			}
			if tt.runner && !slices.Equal(p.Runners[0].Exclusions, tt.exclusions) {
				t.Fatalf("exclusions = %+v, want %+v", p.Runners[0].Exclusions, tt.exclusions)
			}
		})
	}
}

func TestProgressValidation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		repository, artifacts string
		valid                 bool
	}{
		{"", "", true}, {"existing", "local", true}, {"generate", "customer", true}, {"workflow source", "local", false}, {"existing", "secret", false},
	} {
		t.Run(tt.repository+tt.artifacts, func(t *testing.T) {
			err := (Progress{Repository: tt.repository, Artifacts: tt.artifacts}).Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestRunnerFirstObservedReadiness(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, health, checkout, doctor, provider string
		enrolled, validated                      bool
		exclusion                                string
	}{
		{"missing checkout", "online", "failed", "pending", "pending", true, false, ""},
		{"failed doctor", "online", "passed", "failed", "passed", true, false, ""},
		{"missing sign in", "online", "passed", "passed", "failed", true, false, ""},
		{"successful checks", "online", "passed", "passed", "passed", true, true, ""},
		{"doctor warnings", "online", "passed", "warning", "passed", true, true, ""},
		{"offline evidence", "offline", "passed", "passed", "passed", false, false, ""},
		{"project access denied", "online", "passed", "passed", "passed", false, false, "project_access_denied"},
		{"runner full", "online", "passed", "passed", "passed", true, true, "runner_capacity"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := Project{Progress: Progress{Doctor: true, Provider: true, Artifacts: "local"}, Runners: []runnerauth.Eligibility{{Runner: runnerauth.Runner{Routing: runnerauth.Routing{State: "active"}, Health: tt.health}, LocalChecks: &runnerauth.LocalChecks{Checkout: tt.checkout, Doctor: tt.doctor, Provider: tt.provider}}}}
			if tt.exclusion != "" {
				p.Runners[0].Exclusions = []runnerauth.Exclusion{{Code: tt.exclusion}}
			}
			for range 2 {
				p.Evaluate()
				if p.Steps[0].Name != "Execution runner" || (p.Steps[0].State == "ready") != tt.enrolled || (p.Steps[1].State == "ready") != tt.validated || p.Steps[2].Name != "Repository configuration" || p.Ready {
					t.Fatalf("setup = %+v", p)
				}
			}
		})
	}
	p := Project{Progress: Progress{Doctor: true, Provider: true}, Runners: []runnerauth.Eligibility{{Runner: runnerauth.Runner{Health: "online", Routing: runnerauth.Routing{State: "active"}}}}}
	p.Evaluate()
	if p.Steps[1].State == "ready" {
		t.Fatal("browser attestations became observed evidence")
	}
}
