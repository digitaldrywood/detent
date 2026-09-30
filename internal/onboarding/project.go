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
	LatestRun string           `json:"latest_run,omitempty"`
	Progress  Progress         `json:"progress"`
	Policy    *policy.Approval `json:"policy,omitempty"`
	// ObservedPolicies are the distinct descriptors runners resolved and could
	// not run, newest first; the approved policy is never among them.
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
	enrolled := slices.ContainsFunc(p.Runners, func(r runnerauth.Eligibility) bool { return r.Runner.Health == "online" && r.Runner.State == "active" })
	add("Execution runner", enrolled, "Enroll a host, then prepare its project checkout, detent.yaml and WORKFLOW.md. Enrollment does not require policy approval.")
	validated := slices.ContainsFunc(p.Runners, func(r runnerauth.Eligibility) bool {
		return r.Runner.Health == "online" && r.Runner.State == "active" && r.LocalChecks != nil && r.LocalChecks.Passed()
	})
	add("Local validation", validated, "The runner reports checkout, detent doctor and selected provider sign-in checks. Fix failures on that host, then restart the runner to report fresh results. Credentials stay local.")
	add("Repository configuration", p.Policy != nil, "Review the policy reported by the runner and explicitly approve it before dispatch.")
	matching := slices.ContainsFunc(p.Runners, func(r runnerauth.Eligibility) bool { return len(r.Exclusions) == 0 })
	artifactDetail := "Choose local history or configure the customer S3-compatible service and independent gateway. A binding does not verify storage or promise offline access."
	add("Artifact history", p.Progress.Artifacts == "local" || p.Progress.Artifacts == "customer" && len(p.Artifacts) > 0, artifactDetail)
	p.Ready = matching && !slices.ContainsFunc(p.Steps, func(s Step) bool { return s.State != "ready" })
}
