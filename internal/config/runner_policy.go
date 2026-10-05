package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	}
	if cfg.Tracker.Kind == TrackerHubNative && (workflow.Definition.Layout == ProjectDefinitionSplit || workflow.Definition.Layout == ProjectDefinitionLegacy) {
		if err := cfg.ValidateNativeWorkflow(); err != nil {
			return policy.Descriptor{}, err
		}
		source := workflow.Definition.WorkflowPath
		if workflow.Definition.Layout == ProjectDefinitionSplit {
			source = workflow.Definition.ConfigPath
		}
		descriptor.Workflow = &policy.Workflow{Source: strings.TrimPrefix(source, workflow.Definition.Revision+":"), States: cfg.NativeWorkflowStates()}
	}
	descriptor = descriptor.WithID()
	return descriptor, descriptor.Validate()
}

func (c Config) NativeWorkflowStates() []policy.State {
	states := c.KanbanStateNames()
	result := make([]policy.State, 0, len(states))
	for _, name := range states {
		terminal := stateListContains(c.Tracker.TerminalStates, name)
		dispatchable := !terminal && stateListContains(c.Tracker.ActiveStates, name)
		if c.Plan.Enabled && sameKanbanPolicyState(c.Plan.Stop, name) {
			dispatchable = false
		}
		result = append(result, policy.State{Name: name, Terminal: terminal, Dispatchable: dispatchable, Transitions: c.KanbanAllowedTransitionTargets(name)})
	}
	return result
}

func (c Config) ValidateNativeWorkflow() error {
	states := c.KanbanStateNames()
	for _, name := range c.Tracker.ActiveStates {
		if stateListContains(c.Tracker.TerminalStates, name) {
			return fmt.Errorf("workflow state %q cannot be both active and terminal", name)
		}
	}
	for source, targets := range c.Server.Kanban.AllowedTransitions {
		if !stateListContains(states, source) {
			return fmt.Errorf("server.kanban.allowed_transitions source %q is not a configured workflow state", source)
		}
		for _, target := range targets {
			if !stateListContains(states, target) {
				return fmt.Errorf("server.kanban.allowed_transitions target %q is not a configured workflow state", target)
			}
		}
	}
	return policy.ValidateStates(c.NativeWorkflowStates())
}

func normalizePolicyConfig(cfg Config) Config {
	cfg.Policy = policy.Descriptor{}
	cfg.Agent.RateWindowPacing = DefaultRateWindowPacing()
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
