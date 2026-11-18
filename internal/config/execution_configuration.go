package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/policy"
)

type executionValues struct {
	Behavior       *nativeBehavior   `json:"behavior"`
	ModelSelection ModelSelection    `json:"model_selection"`
	Prompt         string            `json:"prompt"`
	SharedPrompt   string            `json:"shared_prompt"`
	AgentsPrompt   string            `json:"agents_prompt"`
	Definition     ProjectDefinition `json:"definition"`
	SourceHash     string            `json:"source_hash"`
	IssueContract  *IssueContract    `json:"issue_contract"`
}

func NewExecutionConfiguration(workflow Workflow, descriptor policy.Descriptor, selection ModelSelection) (*policy.ExecutionConfiguration, error) {
	contract, err := ResolveIssueContract(workflow.SharedPrompt)
	if err != nil {
		return nil, fmt.Errorf("resolve execution issue contract: %w", err)
	}
	behavior := nativeProjectBehavior(workflow.Config)
	values := executionValues{
		Behavior: &behavior, ModelSelection: selection,
		Prompt: workflow.Prompt, SharedPrompt: workflow.SharedPrompt, AgentsPrompt: workflow.AgentsPrompt,
		Definition: workflow.Definition, SourceHash: workflow.SourceHash,
		IssueContract: &contract,
	}
	if err := values.bindApprovedPrompt(descriptor, policy.ExecutionConfigurationVersion); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode execution configuration: %w", err)
	}
	return &policy.ExecutionConfiguration{Version: policy.ExecutionConfigurationVersion, PolicyID: descriptor.ID, Values: raw}, nil
}

func ApplyExecutionConfiguration(workflow Workflow, descriptor policy.Descriptor, execution *policy.ExecutionConfiguration) (Workflow, error) {
	if execution == nil || execution.Version != policy.ExecutionConfigurationVersion && !(execution.Version == 1 && (descriptor.Authored == nil || descriptor.Authored.SourceVersion() <= 3)) {
		return Workflow{}, errors.New("unsupported execution configuration version; upgrade the Hub and runner")
	}
	if execution.PolicyID == "" || execution.PolicyID != descriptor.ID {
		return Workflow{}, errors.New("policy_mismatch: execution configuration does not match approved policy")
	}
	if err := descriptor.Validate(); err != nil {
		return Workflow{}, err
	}
	if err := validateLocalPolicySnapshot(workflow, descriptor); err != nil {
		return Workflow{}, err
	}
	var values executionValues
	decoder := json.NewDecoder(bytes.NewReader(execution.Values))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return Workflow{}, fmt.Errorf("decode execution configuration: %w", err)
	}
	if err := values.bindApprovedPrompt(descriptor, execution.Version); err != nil {
		return Workflow{}, err
	}
	if values.Behavior == nil {
		return Workflow{}, errors.New("execution configuration has no resolved behavior")
	}
	if values.IssueContract == nil {
		return Workflow{}, errors.New("execution configuration has no resolved issue contract")
	}
	if values.ModelSelection.Sources == nil {
		return Workflow{}, errors.New("execution configuration has no resolved model selection")
	}
	values.ModelSelection.Sources["authority"] = "cloud"
	if problems := values.ModelSelection.Validate(); len(problems) > 0 {
		return Workflow{}, errors.New("execution configuration has invalid model selection")
	}
	for name, value := range values.Behavior.PriorityMap.Map {
		if number, ok := value.(json.Number); ok {
			rank, err := number.Int64()
			if err != nil || rank < 1 || rank > 4 {
				return Workflow{}, errors.New("execution configuration has invalid priority ranks")
			}
			values.Behavior.PriorityMap.Map[name] = int(rank)
		}
	}
	workflow.Config = values.Behavior.apply(workflow.Config)
	workflow.Config.Agents.ModelSelection = values.ModelSelection
	workflow.Config.executionIssueContract = values.IssueContract
	workflow.Prompt, workflow.SharedPrompt, workflow.AgentsPrompt = values.Prompt, values.SharedPrompt, values.AgentsPrompt
	workflow.Authored = cloneNativePolicy(descriptor.Authored)
	workflow.DefinitionSources = nil
	workflow.Definition, workflow.SourceHash = values.Definition, values.SourceHash
	pinned := cloneNativePolicy(descriptor)
	workflow.executionPolicy = &pinned
	workflow.Config.Policy = pinned
	return workflow, nil
}

func (values *executionValues) bindApprovedPrompt(descriptor policy.Descriptor, version int) error {
	approved, err := approvedPrompt(descriptor)
	if err != nil {
		return err
	}
	if version == 1 && descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		values.Prompt = string(legacyAuthoredPrompt([]byte(values.Prompt)))
		values.SharedPrompt = string(legacyAuthoredPrompt([]byte(values.SharedPrompt)))
		if values.AgentsPrompt == "" {
			values.AgentsPrompt = approved.AgentsPrompt
		} else if values.AgentsPrompt != approved.AgentsPrompt {
			agents, err := authoredMarkdown("AGENTS.md", []byte(values.AgentsPrompt), descriptor.Authored.Version)
			if err != nil {
				return err
			}
			values.AgentsPrompt = string(agents.prompt)
		}
	}
	if values.Prompt != approved.Prompt || values.SharedPrompt != approved.SharedPrompt || values.AgentsPrompt != approved.AgentsPrompt {
		return ErrNativeAuthoredMismatch
	}
	return nil
}

func approvedPrompt(descriptor policy.Descriptor) (Workflow, error) {
	if err := descriptor.Validate(); err != nil {
		return Workflow{}, err
	}
	if err := ValidateNativePromptInputs(descriptor); err != nil {
		return Workflow{}, err
	}
	if descriptor.Authored == nil || len(descriptor.Authored.Files) == 0 {
		c := descriptor.Configuration
		return Workflow{Prompt: c.Prompt, SharedPrompt: c.SharedPrompt, AgentsPrompt: c.AgentsPrompt}, nil
	}
	sources, err := verifiedAuthoredSources(descriptor.Authored)
	if err != nil {
		return Workflow{}, err
	}
	var parsed Workflow
	if sources.HasAgents {
		parsed, err = ParseHistoricalProjectDefinition(sources)
		if err != nil {
			return Workflow{}, err
		}
	}
	return withAuthoredPrompt(parsed, sources, descriptor.Authored.Version)
}

func withAuthoredPrompt(workflow Workflow, sources ProjectDefinitionSources, version int) (Workflow, error) {
	doc, err := authoredMarkdown("WORKFLOW.md", sources.Workflow, version)
	if err != nil {
		return Workflow{}, err
	}
	workflow.Prompt = string(doc.prompt)
	workflow.SharedPrompt = workflow.Prompt
	if workflow.AgentsPrompt != "" {
		agents, err := authoredMarkdown("AGENTS.md", sources.Agents, version)
		if err != nil {
			return Workflow{}, err
		}
		workflow.AgentsPrompt = string(agents.prompt)
	}
	return workflow, nil
}

func (w Workflow) WithApprovedPrompt() (Workflow, error) {
	approved, err := approvedPrompt(w.Config.Policy)
	if err != nil {
		return Workflow{}, err
	}
	w.Prompt, w.SharedPrompt, w.AgentsPrompt = approved.Prompt, approved.SharedPrompt, approved.AgentsPrompt
	return w, nil
}

func (c Config) PolicyIssueContract() (IssueContract, error) {
	if c.executionIssueContract != nil {
		return cloneNativePolicy(*c.executionIssueContract), nil
	}
	return ResolvePolicyIssueContract(c.Policy)
}
