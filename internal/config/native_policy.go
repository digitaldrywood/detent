package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/retro"
)

type nativeBehavior struct {
	Review                     Review
	Runners                    Runners
	Workpad                    Workpad
	Deliverable                Deliverable
	Dependencies               Dependencies
	Recovery                   Recovery
	Agent                      Agent
	Gate                       gate.Config
	Plan                       gate.PlanConfig
	Budget                     Budget
	Release                    Release
	Retro                      retro.Config
	Operator                   Operator
	BacklogAdmission           BacklogAdmission
	Repository                 string
	ActiveStates               []string
	ObservedStates             []string
	TerminalStates             []string
	AllowedTransitions         map[string][]string `json:",omitempty"`
	StateMap                   StringOrMap
	PriorityMap                StringOrMap
	DependencyAutoUnblock      DependencyAutoUnblock
	BlockedRecovery            BlockedRecovery
	BlockerAutoPromote         BlockerAutoPromote
	ExtraNetworkDomains        []string
	AllowLocalBinding          *bool
	BudgetDayConfigured        bool
	BudgetIssueConfigured      bool
	AgentBudgetDayConfigured   bool
	AgentBudgetIssueConfigured bool
}

func nativeProjectBehavior(cfg Config) nativeBehavior {
	cfg = normalizePolicyConfig(cfg)
	behavior := nativeBehavior{
		Review: cfg.Review, Runners: cfg.Runners, Workpad: cfg.Workpad,
		Deliverable: cfg.Deliverable, Dependencies: cfg.Dependencies, Recovery: cfg.Recovery,
		Agent: cfg.Agent, Gate: cfg.Gate, Plan: cfg.Plan, Budget: cfg.Budget,
		Release: cfg.Release, Retro: cfg.Retro, Operator: cfg.Operator, BacklogAdmission: cfg.BacklogAdmission,
		Repository: cfg.Tracker.Repository, ActiveStates: cfg.Tracker.ActiveStates,
		ObservedStates: cfg.Tracker.ObservedStates, TerminalStates: cfg.Tracker.TerminalStates,
		AllowedTransitions: cfg.Server.Kanban.AllowedTransitions,
		StateMap:           cfg.Tracker.StateMap, PriorityMap: cfg.Tracker.PriorityMap,
		DependencyAutoUnblock: cfg.Tracker.DependencyAutoUnblock,
		BlockedRecovery:       cfg.Tracker.BlockedRecovery, BlockerAutoPromote: cfg.Tracker.BlockerAutoPromote,
		ExtraNetworkDomains: cfg.Worker.ExtraNetworkDomains, AllowLocalBinding: cfg.Worker.AllowLocalBinding,
		BudgetDayConfigured: cfg.Budget.perDayMaxUSDConfigured, BudgetIssueConfigured: cfg.Budget.perIssueMaxUSDConfigured,
		AgentBudgetDayConfigured: cfg.Agent.Budget.perDayMaxUSDConfigured, AgentBudgetIssueConfigured: cfg.Agent.Budget.perIssueMaxUSDConfigured,
	}
	behavior.Deliverable.OutputRoot = ""
	behavior.Deliverable.ReviewURL = ""
	behavior.Agent.MaxConcurrentAgents = 0
	behavior.Agent.MaxConcurrentAgentsByState = nil
	behavior.Agent.RateWindowPacing = RateWindowPacing{}
	behavior.Agent.Shutdown = Shutdown{}
	behavior.Agent.Lessons.Path = ""
	behavior.Agent.Knowledge.Sources = nil
	behavior.Agent.Knowledge.Configured = false
	behavior.Agent.Skills.Path = ""
	behavior.Agent.Budget.PricingPath = ""
	behavior.Budget.PricingPath = ""
	behavior.ExtraNetworkDomains = slices.Clone(behavior.ExtraNetworkDomains)
	slices.Sort(behavior.ExtraNetworkDomains)
	behavior.ExtraNetworkDomains = slices.Compact(behavior.ExtraNetworkDomains)
	if len(behavior.ExtraNetworkDomains) == 0 {
		behavior.ExtraNetworkDomains = nil
	}
	return behavior
}

func (b nativeBehavior) apply(local Config) Config {
	local.Review, local.Runners, local.Workpad = b.Review, b.Runners, b.Workpad
	outputRoot, reviewURL := local.Deliverable.OutputRoot, local.Deliverable.ReviewURL
	local.Deliverable = b.Deliverable
	local.Deliverable.OutputRoot, local.Deliverable.ReviewURL = outputRoot, reviewURL
	local.Dependencies, local.Recovery = b.Dependencies, b.Recovery
	agent := local.Agent
	local.Agent = b.Agent
	local.Agent.MaxConcurrentAgents = agent.MaxConcurrentAgents
	local.Agent.MaxConcurrentAgentsByState = agent.MaxConcurrentAgentsByState
	local.Agent.RateWindowPacing, local.Agent.Shutdown = agent.RateWindowPacing, agent.Shutdown
	local.Agent.Lessons.Path = agent.Lessons.Path
	local.Agent.Knowledge.Sources, local.Agent.Knowledge.Configured = agent.Knowledge.Sources, agent.Knowledge.Configured
	local.Agent.Skills.Path = agent.Skills.Path
	local.Agent.Budget.PricingPath = agent.Budget.PricingPath
	pricingPath := local.Budget.PricingPath
	local.Gate, local.Plan, local.Budget = b.Gate, b.Plan, b.Budget
	local.Budget.PricingPath = pricingPath
	local.Budget.perDayMaxUSDConfigured, local.Budget.perIssueMaxUSDConfigured = b.BudgetDayConfigured, b.BudgetIssueConfigured
	local.Agent.Budget.perDayMaxUSDConfigured, local.Agent.Budget.perIssueMaxUSDConfigured = b.AgentBudgetDayConfigured, b.AgentBudgetIssueConfigured
	local.Release, local.Operator, local.BacklogAdmission = b.Release, b.Operator, b.BacklogAdmission
	local.Retro = b.Retro
	local.Tracker.Kind, local.Tracker.Repository = TrackerHubNative, b.Repository
	local.Tracker.ActiveStates, local.Tracker.ObservedStates, local.Tracker.TerminalStates = b.ActiveStates, b.ObservedStates, b.TerminalStates
	local.Server.Kanban.AllowedTransitions = b.AllowedTransitions
	local.Tracker.StateMap, local.Tracker.PriorityMap = b.StateMap, b.PriorityMap
	local.Tracker.DependencyAutoUnblock = b.DependencyAutoUnblock
	local.Tracker.BlockedRecovery, local.Tracker.BlockerAutoPromote = b.BlockedRecovery, b.BlockerAutoPromote
	local.Worker.ExtraNetworkDomains, local.Worker.AllowLocalBinding = b.ExtraNetworkDomains, b.AllowLocalBinding
	if local.Worker.AllowLocalBinding == nil {
		local.Worker.AllowLocalBinding = new(false)
	}
	local.Worker.localDefaults = &WorkerDefaults{AllowLocalBinding: local.Worker.AllowLocalBinding}
	return local
}

func resolveNativePolicy(workflow Workflow) (policy.Descriptor, error) {
	behavior, err := json.Marshal(nativeProjectBehavior(workflow.Config))
	if err != nil {
		return policy.Descriptor{}, fmt.Errorf("encode shared project behavior: %w", err)
	}
	configuration := &policy.Configuration{Behavior: behavior, Prompt: workflow.Prompt, SharedPrompt: workflow.SharedPrompt, AgentsPrompt: workflow.AgentsPrompt}
	if workflow.Definition.Layout == ProjectDefinitionCloud {
		configuration.DefinitionDigest = workflow.SourceHash
	}
	raw, err := json.Marshal(configuration)
	if err != nil {
		return policy.Descriptor{}, err
	}
	digest := policy.Digest(raw)
	descriptor, err := resolvePolicyDescriptor(workflow, digest, digest, digest)
	if err != nil {
		return policy.Descriptor{}, err
	}
	descriptor.Configuration = configuration
	descriptor = descriptor.WithID()
	return descriptor, descriptor.Validate()
}

func ApplyNativePolicy(workflow Workflow, descriptor policy.Descriptor) (Workflow, error) {
	if err := descriptor.Validate(); err != nil {
		return Workflow{}, err
	}
	if descriptor.Configuration == nil {
		return workflow, nil
	}
	var behavior nativeBehavior
	decoder := json.NewDecoder(bytes.NewReader(descriptor.Configuration.Behavior))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&behavior); err != nil {
		return Workflow{}, fmt.Errorf("decode shared project behavior: %w", err)
	}
	for name, value := range behavior.PriorityMap.Map {
		if number, ok := value.(json.Number); ok {
			rank, err := number.Int64()
			if err != nil || rank < 1 || rank > 4 {
				return Workflow{}, errors.New("shared project priority ranks must be integers 1 through 4")
			}
			behavior.PriorityMap.Map[name] = int(rank)
		}
	}
	workflow.Config = behavior.apply(workflow.Config)
	workflow.Prompt, workflow.SharedPrompt = descriptor.Configuration.Prompt, descriptor.Configuration.SharedPrompt
	workflow.AgentsPrompt = descriptor.Configuration.AgentsPrompt
	if descriptor.Workflow != nil {
		workflow.Definition.Layout = ProjectDefinitionSplit
		workflow.Definition.ConfigPath = descriptor.Workflow.Source
		workflow.Definition.Revision = descriptor.Workflow.Revision
	} else if descriptor.Configuration.DefinitionDigest != "" {
		workflow.Definition.Layout = ProjectDefinitionCloud
		workflow.SourceHash = descriptor.Configuration.DefinitionDigest
	}
	resolved, err := ResolvePolicy(workflow)
	if err != nil {
		return Workflow{}, err
	}
	if err := resolved.Match(descriptor); err != nil {
		return Workflow{}, err
	}
	return workflow, nil
}

func ValidateSharedPolicy(descriptor policy.Descriptor) error {
	if err := descriptor.Validate(); err != nil {
		return err
	}
	if descriptor.Configuration == nil {
		return nil
	}
	workflow, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
	if err != nil {
		return err
	}
	return ValidateWorkflowAdmission(workflow)
}
