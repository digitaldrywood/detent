package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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
	workflow.Config = cfg
	if cfg.Tracker.Kind == TrackerHubNative {
		return resolveNativePolicy(workflow)
	}
	cfg = normalizePolicyConfig(cfg)
	if workflow.DefinitionSources != nil {
		authored, err := authoredProjectDefinitionVersion(*workflow.DefinitionSources, workflow.Authored.Version)
		if err != nil {
			return policy.Descriptor{}, err
		}
		workflow.Authored = authored
		return resolvePolicyDescriptor(workflow, authored.Digest, authored.Digest, authored.Digest)
	}
	raw, err := json.Marshal(struct {
		Config Config
		Prompt string
	}{cfg, workflow.Prompt})
	if err != nil {
		return policy.Descriptor{}, fmt.Errorf("digest project policy: %w", err)
	}
	return resolvePolicyDescriptor(workflow, workflow.Definition.Revision, workflow.SourceHash, policy.Digest(raw))
}

func resolvePolicyDescriptor(workflow Workflow, revision, sourceDigest, configDigest string) (policy.Descriptor, error) {
	cfg := normalizePolicyConfig(workflow.Config)
	g := gate.Effective(cfg.Gate)
	p := gate.EffectivePlan(cfg.Plan)
	descriptor := policy.Descriptor{
		SourceRevision: revision,
		SourceDigest:   sourceDigest,
		ConfigDigest:   configDigest,
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
	if cfg.Tracker.Kind == TrackerHubNative && cfg.Deliverable.GitHubPullRequest {
		descriptor.Gates.RequiredChecks = 0
		if len(g.RequiredStatusChecks) > 0 || g.CITriggerLabel != "" {
			contract, err := json.Marshal(struct {
				RequiredStatusChecks []string
				CITriggerLabel       string
			}{g.RequiredStatusChecks, g.CITriggerLabel})
			if err != nil {
				return policy.Descriptor{}, err
			}
			descriptor.Gates.GitHubCIDigest = policy.Digest(contract)
		}
	}
	if cfg.Tracker.Kind == TrackerHubNative && (workflow.Definition.Layout == ProjectDefinitionSplit || workflow.Definition.Layout == ProjectDefinitionLegacy) {
		if err := cfg.ValidateNativeWorkflow(); err != nil {
			return policy.Descriptor{}, err
		}
		source := workflow.Definition.WorkflowPath
		if workflow.Definition.Layout == ProjectDefinitionSplit {
			source = workflow.Definition.ConfigPath
		}
		source, committed := strings.CutPrefix(source, workflow.Definition.Revision+":")
		if !committed {
			source = filepath.Base(source)
		}
		descriptor.Workflow = &policy.Workflow{Source: source, States: cfg.NativeWorkflowStates()}
		if len(workflow.Definition.Revision) == 40 {
			descriptor.Workflow.Revision = workflow.Definition.Revision
		}
	}
	descriptor.Authored = workflow.Authored
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
	if cfg.Agent.AutoPromote.OptoutLabel == "" {
		cfg.Agent.AutoPromote.OptoutLabel = "requires-human-review"
	}
	return cfg
}
