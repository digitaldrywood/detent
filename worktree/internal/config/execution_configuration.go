package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	values, err := json.Marshal(executionValues{
		Behavior: &behavior, ModelSelection: selection,
		Prompt: workflow.Prompt, SharedPrompt: workflow.SharedPrompt, AgentsPrompt: workflow.AgentsPrompt,
		Definition: workflow.Definition, SourceHash: workflow.SourceHash,
		IssueContract: &contract,
	})
	if err != nil {
		return nil, fmt.Errorf("encode execution configuration: %w", err)
	}
	return &policy.ExecutionConfiguration{Version: policy.ExecutionConfigurationVersion, PolicyID: descriptor.ID, Values: values}, nil
}

func ApplyExecutionConfiguration(workflow Workflow, descriptor policy.Descriptor, execution *policy.ExecutionConfiguration) (Workflow, error) {
	if execution == nil || execution.Version != policy.ExecutionConfigurationVersion {
		return Workflow{}, errors.New("unsupported execution configuration version; upgrade the Hub and runner")
	}
	if execution.PolicyID == "" || execution.PolicyID != descriptor.ID {
		return Workflow{}, errors.New("policy_mismatch: execution configuration does not match approved policy")
	}
	if err := descriptor.Validate(); err != nil {
		return Workflow{}, err
	}
	var values executionValues
	decoder := json.NewDecoder(bytes.NewReader(execution.Values))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return Workflow{}, fmt.Errorf("decode execution configuration: %w", err)
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
	localPrompt, err := nativeMachinePrompt(workflow)
	if err != nil {
		return Workflow{}, err
	}
	workflow.Config = values.Behavior.apply(workflow.Config)
	workflow.Config.Agents.ModelSelection = values.ModelSelection
	workflow.Config.executionIssueContract = values.IssueContract
	workflow.Prompt, workflow.SharedPrompt, workflow.AgentsPrompt = values.Prompt, values.SharedPrompt, values.AgentsPrompt
	workflow.machinePrompt = localPrompt
	if localPrompt != "" && !strings.HasSuffix(workflow.Prompt, localPrompt) {
		workflow.Prompt = mergeWorkflowPrompts([]byte(workflow.Prompt), []byte(localPrompt))
	}
	workflow.Authored = cloneNativePolicy(descriptor.Authored)
	workflow.DefinitionSources = nil
	workflow.Definition, workflow.SourceHash = values.Definition, values.SourceHash
	pinned := cloneNativePolicy(descriptor)
	workflow.executionPolicy = &pinned
	workflow.Config.Policy = pinned
	return workflow, nil
}

func (c Config) PolicyIssueContract() (IssueContract, error) {
	if c.executionIssueContract != nil {
		return cloneNativePolicy(*c.executionIssueContract), nil
	}
	return ResolvePolicyIssueContract(c.Policy)
}
