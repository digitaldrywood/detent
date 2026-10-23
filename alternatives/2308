package onboarding

import (
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Progress struct {
	Revision   tracker.Revision `json:"revision,string"`
	Repository string           `json:"repository"`
	Doctor     bool             `json:"doctor"`
	Provider   bool             `json:"provider"`
	Artifacts  string           `json:"artifacts"`
	UpdatedAt  string           `json:"updated_at,omitempty"`
}

func (p Progress) Validate() error {
	if !slices.Contains([]string{"", "existing", "generate"}, p.Repository) || !slices.Contains([]string{"", "local", "customer"}, p.Artifacts) {
		return errors.New("select an existing or new repository configuration and local or customer artifact storage")
	}
	return nil
}

type Step struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type Project struct {
	Repository       string                   `json:"repository,omitempty"`
	LatestRun        string                   `json:"latest_run,omitempty"`
	Progress         Progress                 `json:"progress"`
	Policy           *policy.Approval         `json:"policy,omitempty"`
	ObservedPolicies []policy.ObservedPolicy  `json:"observed_policies,omitempty"`
	Runners          []runnerauth.Eligibility `json:"runners"`
	Artifacts        []artifact.Binding       `json:"artifact_services"`
	Steps            []Step                   `json:"steps"`
	Ready            bool                     `json:"ready"`
}

func (p *Project) Evaluate() {
	p.Steps = nil
	add := func(name string, ready bool, detail string) {
		state := "action_required"
		if ready {
			state = "ready"
		}
		p.Steps = append(p.Steps, Step{Name: name, State: state, Detail: detail})
	}
	enrolled := func(r runnerauth.Eligibility) bool {
		return r.Runner.State == "active" && r.Runner.Health != "revoked" && r.Runner.Health != "expired" && !slices.ContainsFunc(r.Exclusions, func(e runnerauth.Exclusion) bool {
			return e.Code == "project_access_denied"
		})
	}
	add("Runner", slices.ContainsFunc(p.Runners, enrolled), "Use a runner you already have. It can work on this project alongside its other projects.")
	add("Repository", p.Repository != "", "Choose the repository your runner should work on using its own Git sign-in.")
	validated := slices.ContainsFunc(p.Runners, func(r runnerauth.Eligibility) bool {
		return enrolled(r) && (r.Runner.ConnectionHealth == "online" || r.Runner.ConnectionHealth == "" && r.Runner.Health == "online") && r.LocalChecks != nil && r.LocalChecks.Passed()
	})
	checkoutDetail := "Verify the checkout, repository configuration, Detent checks and provider sign-in on your runner."
	if !validated && slices.ContainsFunc(p.Runners, func(r runnerauth.Eligibility) bool {
		return enrolled(r) && r.LocalChecks != nil && r.LocalChecks.Configuration == "missing"
	}) {
		checkoutDetail = "Configuration missing: detent.yaml and WORKFLOW.md not found. Create starter files or add them to the repository; checks continue automatically."
	}
	add("Checkout", validated && (p.Policy != nil || len(p.ObservedPolicies) > 0), checkoutDetail)
	matching := slices.ContainsFunc(p.Runners, func(r runnerauth.Eligibility) bool {
		return !slices.ContainsFunc(r.Exclusions, func(e runnerauth.Exclusion) bool {
			return e.Code != "host_capacity" && e.Code != "runner_capacity"
		})
	})
	artifactDetail := "Choose where to keep files from agent runs, such as logs and screenshots. Keep them on your runner, or connect storage that you manage."
	add("Run files", p.Progress.Artifacts == "local" || p.Progress.Artifacts == "customer" && len(p.Artifacts) > 0, artifactDetail)
	p.Ready = matching && !slices.ContainsFunc(p.Steps, func(s Step) bool { return s.State != "ready" })
}
