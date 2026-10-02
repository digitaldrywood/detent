package config

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
)

type Runners struct {
	Profile  string                         `yaml:"profile,omitempty"`
	Profiles map[string]policy.Requirements `yaml:"profiles,omitempty"`
}

func (r Runners) Validate() []string {
	var problems []string
	if len(r.Profiles) > 32 {
		problems = append(problems, "runners.profiles may contain at most 32 profiles")
	}
	for name, requirements := range r.Profiles {
		if !policy.ValidToken(name) {
			problems = append(problems, "runners.profiles names must be lowercase ASCII tokens of at most 64 characters")
		}
		if err := requirements.Normalized().Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("runners.profiles.%s: %v", name, err))
		}
	}
	if _, ok := r.Profiles[r.Profile]; r.Profile != "" && !ok {
		problems = append(problems, "runners.profile must name a declared runner profile")
	}
	sort.Strings(problems)
	return problems
}

func ResolvePolicy(workflow Workflow) (policy.Descriptor, error) {
	cfg := workflow.Config
	if err := cfg.Validate(); err != nil {
		return policy.Descriptor{}, err
	}
	cfg = normalizePolicyConfig(cfg)
	raw, err := json.Marshal(struct {
		Config Config
		Prompt string
	}{cfg, workflow.Prompt})
	if err != nil {
		return policy.Descriptor{}, fmt.Errorf("digest effective project policy: %w", err)
	}
	g := gate.Effective(cfg.Gate)
	p := gate.EffectivePlan(cfg.Plan)
	descriptor := policy.Descriptor{
		SourceRevision: workflow.Definition.Revision,
		SourceDigest:   workflow.SourceHash,
		ConfigDigest:   policy.Digest(raw),
		Profile:        cfg.Runners.Profile,
		Requirements:   cfg.Runners.Profiles[cfg.Runners.Profile].Normalized(),
		Gates: policy.Gates{
			Kind: g.Kind, PlanEnabled: p.Enabled, PlanReview: p.Review, PlanStopDigest: policy.Digest([]byte(p.Stop)),
			HumanReview: cfg.Review.Human,
			AutoPromote: cfg.Agent.AutoPromote.Enabled, AutomatedReview: g.AutomatedReview,
			RequiredChecks: len(g.RequiredStatusChecks), Validator: g.Validator.Enabled,
			SecurityAudit: g.SecurityAudit.Enabled, MergeMethod: cfg.Deliverable.EffectiveMergeMethod(),
			GitHubPullRequest: cfg.Deliverable.GitHubPullRequest,
		},
	}.WithID()
	return descriptor, descriptor.Validate()
}

// normalizePolicyConfig preserves the approved JSON representation across
// binary upgrades. Only equivalent absent/default execution settings collapse;
// explicit policy changes remain digest inputs. Normalize a copy so runtime
// configuration keeps its execution defaults.
func normalizePolicyConfig(cfg Config) Config {
	cfg.Policy = policy.Descriptor{}
	if !cfg.Worker.EffectiveAllowLocalBinding() {
		cfg.Worker.AllowLocalBinding = nil
	}
	if cfg.Worker.HostSelection == "least_loaded" {
		cfg.Worker.HostSelection = ""
	}
	// Before v0.117.6, gate normalization serialized absent checks as [].
	if cfg.Gate.RequiredStatusChecks == nil {
		cfg.Gate.RequiredStatusChecks = []string{}
	}
	if cfg.Agent.AutoPromote.OptoutLabel == "" {
		cfg.Agent.AutoPromote.OptoutLabel = "requires-human-review"
	}
	return cfg
}
