package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
)

type nativeBehavior struct {
	Review                     Review
	Runners                    Runners
	Workpad                    Workpad
	Deliverable                Deliverable
	Dependencies               Dependencies
	Agent                      Agent
	Gate                       gate.Config
	Plan                       gate.PlanConfig
	Budget                     Budget
	Release                    Release
	Retro                      json.RawMessage `json:",omitempty"`
	Operator                   Operator
	BacklogAdmission           BacklogAdmission
	Repository                 string
	Lanes                      []Lane `json:",omitempty"`
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
		Deliverable: cfg.Deliverable, Dependencies: cfg.Dependencies,
		Agent: cfg.Agent, Gate: cfg.Gate, Plan: cfg.Plan, Budget: cfg.Budget,
		Release: cfg.Release, Retro: cfg.historicalRetro, Operator: cfg.Operator, BacklogAdmission: cfg.BacklogAdmission,
		Lanes: cfg.Tracker.Lanes, Repository: cfg.Tracker.Repository, ActiveStates: cfg.Tracker.ActiveStates,
		ObservedStates: cfg.Tracker.ObservedStates, TerminalStates: cfg.Tracker.TerminalStates,
		AllowedTransitions: cfg.Server.Kanban.AllowedTransitions,
		StateMap:           cfg.Tracker.StateMap, PriorityMap: cfg.Tracker.PriorityMap,
		DependencyAutoUnblock: cfg.Tracker.DependencyAutoUnblock,
		BlockedRecovery:       cfg.Tracker.BlockedRecovery, BlockerAutoPromote: cfg.Tracker.BlockerAutoPromote,
		ExtraNetworkDomains: cfg.Worker.ExtraNetworkDomains, AllowLocalBinding: cfg.Worker.AllowLocalBinding,
		BudgetDayConfigured: cfg.Budget.perDayMaxUSDConfigured, BudgetIssueConfigured: cfg.Budget.perIssueMaxUSDConfigured,
		AgentBudgetDayConfigured: cfg.Agent.Budget.perDayMaxUSDConfigured, AgentBudgetIssueConfigured: cfg.Agent.Budget.perIssueMaxUSDConfigured,
	}
	legacyTracker := cfg.Tracker
	legacyTracker.Lanes = nil
	if slices.Equal(behavior.Lanes, legacyTracker.WorkflowLanes()) {
		behavior.Lanes = nil
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
	local.Dependencies = b.Dependencies
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
	local.historicalRetro = b.Retro
	local.Tracker.Kind, local.Tracker.Repository = TrackerHubNative, b.Repository
	local.Tracker.Lanes = b.Lanes
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
	if workflow.DefinitionSources != nil {
		sources, err := nativeAuthoredSources(*workflow.DefinitionSources, workflow.Authored.Version)
		if err != nil {
			return policy.Descriptor{}, err
		}
		authored, err := authoredProjectDefinitionVersion(sources, workflow.Authored.Version)
		if err != nil {
			return policy.Descriptor{}, err
		}
		authored.Files = authoredSourceFiles(sources)
		workflow.Authored = authored
		descriptor, err := resolvePolicyDescriptor(workflow, authored.Digest, authored.Digest, authored.Digest)
		if err != nil {
			return policy.Descriptor{}, err
		}
		descriptor.Configuration = &policy.Configuration{Behavior: json.RawMessage("null")}
		if workflow.Definition.Layout == ProjectDefinitionCloud {
			descriptor.Configuration.DefinitionDigest = workflow.SourceHash
		}
		return descriptor.WithID(), nil
	}
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
	if workflow.Authored != nil {
		digest = workflow.Authored.Digest
	}
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
	if err := validateLocalPolicySnapshot(workflow, descriptor); err != nil {
		return Workflow{}, err
	}
	return applyNativePolicy(workflow, descriptor)
}

func ReadNativePolicy(descriptor policy.Descriptor) (Workflow, error) {
	if err := descriptor.Validate(); err != nil {
		return Workflow{}, err
	}
	return applyNativePolicy(Workflow{Config: Default()}, descriptor)
}

func applyNativePolicy(workflow Workflow, descriptor policy.Descriptor) (Workflow, error) {
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		shared, _, err := resolveAuthoredNativePolicy(descriptor)
		if err != nil {
			return Workflow{}, err
		}
		return applyAuthoredNativePolicy(workflow, shared), nil
	}
	if descriptor.Configuration != nil {
		var err error
		workflow, err = applyNativePolicyConfiguration(workflow, descriptor)
		if err != nil {
			return Workflow{}, err
		}
	}
	cfg := &workflow.Config
	cfg.Review.Human = descriptor.Gates.HumanReview
	effectiveGate := gate.Effective(cfg.Gate)
	if effectiveGate.Kind != descriptor.Gates.Kind {
		cfg.Gate.Kind = descriptor.Gates.Kind
	}
	if effectiveGate.AutomatedReview != descriptor.Gates.AutomatedReview {
		cfg.Gate.AutomatedReview = descriptor.Gates.AutomatedReview
	}
	cfg.Gate.Validator.Enabled = descriptor.Gates.Validator
	cfg.Gate.SecurityAudit.Enabled = descriptor.Gates.SecurityAudit
	cfg.Plan.Enabled = descriptor.Gates.PlanEnabled
	if gate.EffectivePlan(cfg.Plan).Review != descriptor.Gates.PlanReview {
		cfg.Plan.Review = descriptor.Gates.PlanReview
	}
	cfg.Agent.AutoPromote.Enabled = descriptor.Gates.AutoPromote
	if cfg.Deliverable.EffectiveMergeMethod() != descriptor.Gates.MergeMethod {
		cfg.Deliverable.MergeMethod = descriptor.Gates.MergeMethod
	}
	cfg.Deliverable.GitHubPullRequest = descriptor.Gates.GitHubPullRequest
	cfg.Runners.Profile = descriptor.Profile
	if descriptor.Profile != "" && !reflect.DeepEqual(cfg.Runners.Profiles[descriptor.Profile].Normalized(), descriptor.Requirements) {
		if cfg.Runners.Profiles == nil {
			cfg.Runners.Profiles = make(map[string]policy.Requirements)
		}
		cfg.Runners.Profiles[descriptor.Profile] = descriptor.Requirements.Normalized()
	}
	return workflow, nil
}

var ErrNativePromptUnavailable = errors.New("policy_mismatch: approved workflow instructions are unavailable. Include the required instructions in WORKFLOW.md and approve its pinned content")

var ErrNativeAuthoredMismatch = errors.New("policy_mismatch: authored inputs do not match their digests")

func ValidateNativePromptInputs(descriptor policy.Descriptor) error {
	if descriptor.Authored != nil && (descriptor.Authored.SourceVersion() < 1 || descriptor.Authored.SourceVersion() > NativePolicyCanonicalizationVersion) {
		return fmt.Errorf("unsupported policy canonicalization version %d", descriptor.Authored.Version)
	}
	if descriptor.Authored == nil || len(descriptor.Authored.Files) == 0 {
		snapshot := descriptor.Configuration
		if snapshot == nil {
			return ErrNativePromptUnavailable
		}
		behavior := bytes.TrimSpace(snapshot.Behavior)
		if len(behavior) == 0 || bytes.Equal(behavior, []byte("null")) {
			return ErrNativePromptUnavailable
		}
		if descriptor.Authored != nil || strings.TrimSpace(snapshot.Prompt) == "" {
			raw, err := json.Marshal(snapshot)
			if err != nil || policy.Digest(raw) != descriptor.ConfigDigest {
				return ErrNativePromptUnavailable
			}
		}
		return nil
	}
	overlay, err := splitProjectWorkflow([]byte(descriptor.Authored.Files["WORKFLOW.local.md"]))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(overlay.prompt)) != 0 {
		return ErrNativePromptUnavailable
	}
	return nil
}

func validateLocalPolicySnapshot(workflow Workflow, descriptor policy.Descriptor) error {
	if err := ValidateNativePromptInputs(descriptor); err != nil {
		return err
	}
	if workflow.DefinitionSources == nil || descriptor.Authored == nil || len(descriptor.Authored.Files) == 0 {
		return nil
	}
	sources := *workflow.DefinitionSources
	localWorkflow, err := splitProjectWorkflow(sources.LocalWorkflow)
	if err != nil {
		return err
	}
	localWorkflow.prompt = nil
	sources.LocalWorkflow = legacyWorkflowBytes(localWorkflow)
	local, err := nativeAuthoredSources(sources, NativePolicyCanonicalizationVersion)
	if err != nil {
		return err
	}
	approved, err := nativeAuthoredSources(sourcesFromAuthoredFiles(descriptor.Authored.Files), NativePolicyCanonicalizationVersion)
	if err != nil {
		return err
	}
	if local.HasLocalWorkflow && !bytes.Equal(local.LocalWorkflow, approved.LocalWorkflow) || local.HasLocalConfig && !bytes.Equal(local.LocalConfig, approved.LocalConfig) {
		return errors.New("policy_mismatch: machine-local policy differs from approved inputs")
	}
	return nil
}

func applyAuthoredNativePolicy(workflow, shared Workflow) Workflow {
	workflow.Config = nativeProjectBehavior(shared.Config).apply(workflow.Config)
	workflow.Prompt, workflow.SharedPrompt, workflow.AgentsPrompt = shared.Prompt, shared.SharedPrompt, shared.AgentsPrompt
	workflow.Authored, workflow.DefinitionSources, workflow.Definition = shared.Authored, shared.DefinitionSources, shared.Definition
	return workflow
}

func applyNativePolicyConfiguration(workflow Workflow, descriptor policy.Descriptor) (Workflow, error) {
	defaults, err := json.Marshal(nativeProjectBehavior(Default()))
	if err != nil {
		return Workflow{}, fmt.Errorf("encode shared project defaults: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(defaults, &fields); err != nil {
		return Workflow{}, fmt.Errorf("decode shared project defaults: %w", err)
	}
	if raw := bytes.TrimSpace(descriptor.Configuration.Behavior); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return Workflow{}, fmt.Errorf("decode shared project behavior: %w", err)
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return Workflow{}, fmt.Errorf("encode shared project behavior: %w", err)
	}
	var behavior nativeBehavior
	decoder := json.NewDecoder(bytes.NewReader(raw))
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
	workflow.Authored = descriptor.Authored
	workflow.DefinitionSources = nil
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
	return workflow, nil
}

func verifiedAuthoredSources(authored *policy.Authored) (ProjectDefinitionSources, error) {
	sources := sourcesFromAuthoredFiles(authored.Files)
	identity, err := authoredProjectDefinitionVersion(sources, authored.Version)
	if err != nil {
		return ProjectDefinitionSources{}, err
	}
	if identity.Digest != authored.Digest {
		return ProjectDefinitionSources{}, ErrNativeAuthoredMismatch
	}
	return sources, nil
}

func resolveAuthoredNativePolicy(descriptor policy.Descriptor) (Workflow, policy.Descriptor, error) {
	if err := descriptor.Validate(); err != nil {
		return Workflow{}, policy.Descriptor{}, err
	}
	sources, err := verifiedAuthoredSources(descriptor.Authored)
	if err != nil {
		return Workflow{}, policy.Descriptor{}, err
	}
	authored := *descriptor.Authored
	authored.Files = authoredSourceFiles(sources)
	parseSources := sources
	if sources.HasLocalWorkflow {
		document, err := splitProjectWorkflow(sources.LocalWorkflow)
		if err != nil {
			return Workflow{}, policy.Descriptor{}, err
		}
		if prompt := strings.TrimSpace(string(document.prompt)); prompt != "" {
			if err := validateLocalWorkflowDigest(prompt); err != nil {
				return Workflow{}, policy.Descriptor{}, err
			}
		}
		document.prompt = nil
		parseSources.LocalWorkflow = legacyWorkflowBytes(document)
	}
	shared, err := ParseHistoricalProjectDefinition(parseSources)
	if err != nil {
		return Workflow{}, policy.Descriptor{}, err
	}
	shared, err = withAuthoredPrompt(shared, sources, authored.Version)
	if err != nil {
		return Workflow{}, policy.Descriptor{}, err
	}
	shared.DefinitionSources = &sources
	shared.Config = shared.Config.ForNativeTracker()
	shared.Authored = &authored
	shared.Definition = ProjectDefinition{}
	if descriptor.Workflow != nil {
		shared.Definition.Layout = ProjectDefinitionSplit
		if !sources.HasConfig {
			shared.Definition.Layout = ProjectDefinitionLegacy
		}
		shared.Definition.ConfigPath, shared.Definition.WorkflowPath, shared.Definition.Revision = descriptor.Workflow.Source, descriptor.Workflow.Source, descriptor.Workflow.Revision
	} else if descriptor.Configuration != nil && descriptor.Configuration.DefinitionDigest != "" {
		shared.Definition.Layout = ProjectDefinitionCloud
		shared.SourceHash = descriptor.Configuration.DefinitionDigest
	}
	if err := shared.Config.Validate(); err != nil {
		return Workflow{}, policy.Descriptor{}, err
	}
	if err := ValidateWorkflowAdmission(shared); err != nil {
		return Workflow{}, policy.Descriptor{}, err
	}
	resolved, err := resolveNativePolicy(shared)
	return shared, resolved, err
}

func SameSharedPolicyBehavior(approved, selected policy.Descriptor) (bool, error) {
	if approved.Authored == nil || selected.Authored == nil || len(approved.Authored.Files) == 0 || len(selected.Authored.Files) == 0 {
		return false, nil
	}
	if approved.Gates != selected.Gates || approved.Profile != selected.Profile || !reflect.DeepEqual(approved.Requirements, selected.Requirements) || (approved.Workflow == nil) != (selected.Workflow == nil) {
		return false, nil
	}
	if approved.Workflow != nil && (approved.Workflow.Source != selected.Workflow.Source || !reflect.DeepEqual(approved.Workflow.States, selected.Workflow.States)) {
		return false, nil
	}
	before, _, err := resolveAuthoredNativePolicy(approved)
	if err != nil {
		return false, err
	}
	after, _, err := resolveAuthoredNativePolicy(selected)
	if err != nil {
		return false, err
	}
	left, right := nativeProjectBehavior(before.Config), nativeProjectBehavior(after.Config)
	left.Agent.Effort, right.Agent.Effort = AgentRoleEffort{}, AgentRoleEffort{}
	left.Agent.InstructionsByState, right.Agent.InstructionsByState = nil, nil
	left.Agent.InstructionsByTransition, right.Agent.InstructionsByTransition = nil, nil
	return reflect.DeepEqual(left, right), nil
}

func ResolveSharedPolicy(descriptor policy.Descriptor) (policy.Descriptor, error) {
	if err := descriptor.Validate(); err != nil {
		return policy.Descriptor{}, err
	}
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		_, resolved, err := resolveAuthoredNativePolicy(descriptor)
		return resolved, err
	}
	if descriptor.Configuration == nil {
		return descriptor, nil
	}
	workflow, err := applyNativePolicyConfiguration(Workflow{Config: Default()}, descriptor)
	if err != nil {
		return policy.Descriptor{}, err
	}
	if err := ValidateWorkflowAdmission(workflow); err != nil {
		return policy.Descriptor{}, err
	}
	resolved, err := ResolvePolicy(workflow)
	if err != nil {
		return policy.Descriptor{}, err
	}
	if !reflect.DeepEqual(resolved.Gates, descriptor.Gates) || !reflect.DeepEqual(resolved.Requirements, descriptor.Requirements) || resolved.Profile != descriptor.Profile || !reflect.DeepEqual(resolved.Workflow, descriptor.Workflow) {
		return policy.Descriptor{}, errors.New("policy_mismatch: runtime metadata differs from shared configuration")
	}
	if err := resolved.Match(descriptor); err != nil {
		return policy.Descriptor{}, err
	}
	return descriptor, nil
}

func ValidateSharedPolicy(descriptor policy.Descriptor) error {
	_, err := ResolveSharedPolicy(descriptor)
	return err
}

type ResolvedNativePolicy struct {
	descriptor policy.Descriptor
	workflow   Workflow
	once       sync.Once
	err        error
}

func ResolveNativePolicy(descriptor policy.Descriptor) (*ResolvedNativePolicy, error) {
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	descriptor = cloneNativePolicy(descriptor)
	result := &ResolvedNativePolicy{descriptor: descriptor}
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		workflow, resolved, err := resolveAuthoredNativePolicy(descriptor)
		if err != nil {
			return nil, err
		}
		result.descriptor = resolved
		result.once.Do(func() {
			result.workflow = applyAuthoredNativePolicy(Workflow{Config: Default()}, workflow)
		})
	}
	return result, nil
}

func (p *ResolvedNativePolicy) Descriptor() policy.Descriptor {
	return cloneNativePolicy(p.descriptor)
}

func (p *ResolvedNativePolicy) Workflow() (Workflow, error) {
	if err := ValidateNativePromptInputs(p.descriptor); err != nil {
		return Workflow{}, err
	}
	return p.ReadWorkflow()
}

func (p *ResolvedNativePolicy) ReadWorkflow() (Workflow, error) {
	p.once.Do(func() {
		p.workflow, p.err = ReadNativePolicy(p.descriptor)
	})
	if p.err != nil {
		return Workflow{}, p.err
	}
	return cloneNativePolicy(p.workflow), nil
}

type nativePolicyCloneKey struct {
	typeOf  reflect.Type
	pointer uintptr
}

func cloneNativePolicy[T any](value T) T {
	return cloneNativePolicyWithPointers(value, make(map[nativePolicyCloneKey]reflect.Value))
}

func cloneNativePolicyWithPointers[T any](value T, pointers map[nativePolicyCloneKey]reflect.Value) T {
	var result T
	reflect.ValueOf(&result).Elem().Set(cloneNativePolicyValue(reflect.ValueOf(value), pointers))
	return result
}

func cloneNativePolicyValue(value reflect.Value, pointers map[nativePolicyCloneKey]reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
		if value.IsNil() {
			return value
		}
	}
	switch value.Kind() {
	case reflect.Pointer:
		key := nativePolicyCloneKey{typeOf: value.Type(), pointer: value.Pointer()}
		if result, ok := pointers[key]; ok {
			return result
		}
		result := reflect.New(value.Type().Elem())
		pointers[key] = result
		result.Elem().Set(cloneNativePolicyValue(value.Elem(), pointers))
		return result
	case reflect.Interface:
		result := reflect.New(value.Type()).Elem()
		result.Set(cloneNativePolicyValue(value.Elem(), pointers))
		return result
	case reflect.Slice:
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			result.Index(i).Set(cloneNativePolicyValue(value.Index(i), pointers))
		}
		return result
	case reflect.Map:
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		entries := value.MapRange()
		for entries.Next() {
			result.SetMapIndex(entries.Key(), cloneNativePolicyValue(entries.Value(), pointers))
		}
		return result
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		for i := range value.NumField() {
			if result.Field(i).CanSet() && value.Type().Field(i).IsExported() {
				result.Field(i).Set(cloneNativePolicyValue(value.Field(i), pointers))
			}
		}
		switch copied := result.Interface().(type) {
		case Config:
			copied.configuredFields = maps.Clone(copied.configuredFields)
			copied.executionIssueContract = cloneNativePolicyWithPointers(copied.executionIssueContract, pointers)
			return reflect.ValueOf(copied)
		case Worker:
			copied.localDefaults = cloneNativePolicyWithPointers(copied.localDefaults, pointers)
			return reflect.ValueOf(copied)
		case Agents:
			copied.local = cloneNativePolicyWithPointers(copied.local, pointers)
			return reflect.ValueOf(copied)
		case AgentBackend:
			copied.codexOptions = cloneNativePolicyWithPointers(copied.codexOptions, pointers)
			copied.claudeCodeOptions = cloneNativePolicyWithPointers(copied.claudeCodeOptions, pointers)
			return reflect.ValueOf(copied)
		case BackendOptions:
			copied.node = cloneNativePolicyWithPointers(copied.node, pointers)
			return reflect.ValueOf(copied)
		}
		return result
	default:
		return value
	}
}
