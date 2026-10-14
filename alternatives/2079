package onboarding

import (
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
		want                                      bool
	}{
		{name: "interrupted"},
		{name: "missing policy", doctor: true, provider: true, runner: true, artifacts: "local"},
		{name: "missing provider", policy: true, doctor: true, runner: true, artifacts: "local"},
		{name: "missing runner", policy: true, doctor: true, provider: true, artifacts: "local"},
		{name: "missing gateway", policy: true, doctor: true, provider: true, runner: true, artifacts: "customer"},
		{name: "local ready", policy: true, doctor: true, provider: true, runner: true, artifacts: "local", want: true},
		{name: "customer configured", policy: true, doctor: true, provider: true, runner: true, binding: true, artifacts: "customer", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := Project{Progress: Progress{Repository: "existing", Doctor: tt.doctor, Provider: tt.provider, Artifacts: tt.artifacts}}
			if tt.policy {
				p.Policy = &policy.Approval{}
			}
			if tt.runner {
				p.Runners = []runnerauth.Eligibility{{Runner: runnerauth.Runner{Routing: runnerauth.Routing{State: "active"}, Health: "online"}, LocalChecks: &runnerauth.LocalChecks{Checkout: "passed", Doctor: "failed", Provider: "failed"}}}
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
	}{
		{"missing checkout", "online", "failed", "pending", "pending", true, false},
		{"failed doctor", "online", "passed", "failed", "passed", true, false},
		{"missing sign in", "online", "passed", "passed", "failed", true, false},
		{"successful checks", "online", "passed", "passed", "passed", true, true},
		{"doctor warnings", "online", "passed", "warning", "passed", true, true},
		{"offline evidence", "offline", "passed", "passed", "passed", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := Project{Progress: Progress{Doctor: true, Provider: true, Artifacts: "local"}, Runners: []runnerauth.Eligibility{{Runner: runnerauth.Runner{Routing: runnerauth.Routing{State: "active"}, Health: tt.health}, LocalChecks: &runnerauth.LocalChecks{Checkout: tt.checkout, Doctor: tt.doctor, Provider: tt.provider}}}}
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
