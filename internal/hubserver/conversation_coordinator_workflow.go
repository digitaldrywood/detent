package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func coordinatorWorkflowMutation(name string) bool {
	return name == "approve_project_policy" || name == "apply_local_project_policy"
}

func coordinatorWorkflowTool(name string) bool {
	return coordinatorWorkflowMutation(name) || name == operatortool.LocalProjectConfiguration
}

type coordinatorPolicyApprovalArguments struct {
	ProjectID  string                           `json:"project_id"`
	RequestID  string                           `json:"request_id"`
	RunnerID   string                           `json:"runner_id"`
	SelectedID string                           `json:"selected_policy_id"`
	Input      operatortool.PolicyApprovalInput `json:"input"`
}

func coordinatorWorkflowTools() []runner.AgentTool {
	var result []runner.AgentTool
	for _, name := range []string{operatortool.LocalProjectConfiguration, "approve_project_policy", "apply_local_project_policy"} {
		if name == "approve_project_policy" {
			result = append(result, coordinatorTool(name, "Preview approval of the complete selected_policy observed by the selected runner, unchanged. Supply the current Hub expected_policy_id and observed selected_policy_id; discover runner IDs with get_runners. Repository-owned changes must first be committed and reconciled through their existing authoring owner. The client controls confirmation and submission. Apply the approved policy through apply_local_project_policy; running attempts keep their pinned policy.", `{"type":"object","required":["expected_policy_id","selected_policy_id"],"properties":{"runner_id":{"type":"string"},"expected_policy_id":{"type":"string"},"selected_policy_id":{"type":"string"},"repository_policy":{"type":"boolean"}},"additionalProperties":false}`))
			continue
		}
		definition, _ := operatortool.Lookup(name)
		var schema struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required,omitempty"`
			AdditionalProperties bool                       `json:"additionalProperties"`
		}
		if json.Unmarshal(definition.InputSchema, &schema) != nil {
			panic("invalid workflow tool schema")
		}
		delete(schema.Properties, "project_id")
		delete(schema.Properties, "request_id")
		schema.Required = slices.DeleteFunc(schema.Required, func(key string) bool { return key == "project_id" || key == "request_id" })
		raw, err := json.Marshal(schema)
		if err != nil {
			panic(err)
		}
		description := definition.Description + " Scoped to this conversation's project."
		if coordinatorWorkflowMutation(name) {
			description = "Preview this change through the existing configuration owner. The client controls confirmation and submission. " + strings.ReplaceAll(description, "Authorized approval executes directly.", "")
		}
		if name == operatortool.LocalProjectConfiguration {
			description += " Discover applicable runner IDs with get_runners; pass runner_id when multiple runners apply."
		}
		result = append(result, runner.AgentTool{Name: name, Description: description, InputSchema: raw})
	}
	return result
}

func (t *coordinatorToolset) workflowTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: projectToolScope(call.Name, !coordinatorWorkflowMutation(call.Name)), ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if decodeCoordinatorArguments(call.Arguments, &fields) != nil || fields == nil || fields["project_id"] != nil || fields["request_id"] != nil {
		return nil, operatortool.ErrInvalidArguments
	}
	fields["project_id"], err = json.Marshal(record.ProjectID)
	if err != nil {
		return nil, err
	}
	executor := hubProjectExecutor{t.coordinator.service.server}
	var approval coordinatorPolicyApprovalArguments
	if call.Name == "approve_project_policy" {
		var args struct {
			RunnerID         string `json:"runner_id"`
			ExpectedID       string `json:"expected_policy_id"`
			SelectedID       string `json:"selected_policy_id"`
			RepositoryPolicy bool   `json:"repository_policy"`
		}
		if decodeCoordinatorArguments(call.Arguments, &args) != nil || fields["expected_policy_id"] == nil || args.SelectedID == "" {
			return nil, operatortool.ErrInvalidArguments
		}
		result, err := executor.localProjectConfiguration(ctx, operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: string(record.ProjectID), RunnerID: args.RunnerID})
		if err != nil {
			return nil, err
		}
		var configuration runnerauth.ProjectConfiguration
		if err := json.Unmarshal(result.Content, &configuration); err != nil {
			return nil, err
		}
		if configuration.Source == "configured_local_workflow_read_only" {
			return nil, nativeWorkflowInvalid("Repository workflow is read-only here; propose a reviewed definition change and reconcile its committed revision before policy approval")
		}
		if configuration.SelectedPolicy == nil {
			return nil, &operatortool.RequestError{Code: "unavailable", Message: "The runner has no selected policy observation; inspect local_project_configuration before approval"}
		}
		if configuration.SelectedPolicy.ID != args.SelectedID {
			return nil, policyMismatch("Selected policy changed; read local_project_configuration and preview the change again")
		}
		input, err := json.Marshal(operatortool.PolicyApprovalInput{ExpectedID: args.ExpectedID, Policy: *configuration.SelectedPolicy, RepositoryPolicy: args.RepositoryPolicy})
		if err != nil {
			return nil, err
		}
		fields = map[string]json.RawMessage{"project_id": fields["project_id"], "input": input}
		approval.RunnerID, approval.SelectedID = configuration.RunnerID, configuration.SelectedPolicy.ID
	}
	requestID := coordinatorActionRequestID(t.state.users[len(t.state.users)-1].ID, call)
	if coordinatorWorkflowMutation(call.Name) {
		fields["request_id"], err = json.Marshal(requestID)
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	ownerCall := operatortool.Call{Name: call.Name, Arguments: raw}
	if !coordinatorWorkflowMutation(call.Name) {
		result, err := executor.Execute(ctx, ownerCall)
		if err != nil {
			return nil, err
		}
		return coordinatorPolicyEvidence(result.Content)
	}
	action := chat.Action{Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), RequestID: requestID, Arguments: raw, Material: true}
	if call.Name == "approve_project_policy" {
		if _, err := executor.command(ctx, ownerCall, projectCommandPreview); err != nil {
			return nil, err
		}
		request, err := projectCommandInput[operatortool.PolicyApprovalInput](raw)
		if err != nil {
			return nil, err
		}
		readRaw, err := json.Marshal(operatortool.ProjectReadRequest{ProjectID: string(record.ProjectID), RepositoryPolicy: request.Input.RepositoryPolicy})
		if err != nil {
			return nil, err
		}
		result, err := executor.read(ctx, operatortool.Call{Name: "get_project_policy", Arguments: readRaw})
		var current struct {
			Data policy.Approval `json:"data"`
		}
		if err != nil {
			var missing *operatortool.ConflictError
			if request.Input.ExpectedID != "" || !errors.As(err, &missing) || missing.Code != "policy_mismatch" {
				return nil, err
			}
		} else if err := json.Unmarshal(result.Content, &current); err != nil {
			return nil, err
		}
		if current.Data.Policy.ID != request.Input.ExpectedID {
			return nil, policyMismatch("Approved policy changed; read get_project_policy and preview the change again")
		}
		action.Title = "Approve project workflow policy"
		action.Description, err = coordinatorSettingsDiff(current.Data.Policy, request.Input.Policy)
		if err != nil {
			return nil, err
		}
		action.Description += "\nApproval persists the policy in the Hub. Runner application remains pending until each runner reports adoption; existing issue holds and active lease policies remain authoritative."
		approval.ProjectID, approval.RequestID, approval.Input = action.ProjectID, action.RequestID, request.Input
		action.Arguments, err = json.Marshal(approval)
		if err != nil {
			return nil, err
		}
	} else {
		args, err := operatortool.DecodeLocalProjectArguments(call.Name, raw, true)
		if err != nil {
			return nil, err
		}
		result, err := executor.projectConfigurationCommand(ctx, call.Name, args, true)
		if err != nil {
			return nil, err
		}
		var current runnerauth.ProjectConfiguration
		if err := json.Unmarshal(result.Content, &current); err != nil {
			return nil, err
		}
		if current.RunnerID == "" || current.Constraint != "" {
			return nil, &operatortool.RequestError{Code: "unavailable", Message: "The selected runner configuration owner is unavailable; read get_runners and local_project_configuration before applying policy"}
		}
		action.Title = "Apply approved policy to runner"
		action.Description = fmt.Sprintf("Runner %s: effective policy %s → %s; source revision %s. Submit application using configuration revision %s and runner revision %d. Application is pending until this runner reports it applied; other runners and existing issue holds are unchanged.", args.RunnerID, current.EffectivePolicy.ID, args.PolicyID, args.SourceRevision, args.ExpectedConfigRevision, args.ExpectedRunnerRevision)
	}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func coordinatorSettingsDiff(before, after any) (string, error) {
	decode := func(value any) (map[string]any, error) {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var fields map[string]any
		err = json.Unmarshal(raw, &fields)
		if descriptor, ok := value.(policy.Descriptor); ok && err == nil {
			gates, ok := fields["gates"].(map[string]any)
			if !ok {
				return nil, errors.New("policy settings have no gates")
			}
			gates["github_pull_request"] = descriptor.Gates.GitHubPullRequest
			gates["human_review"] = descriptor.Gates.HumanReview
		}
		return fields, err
	}
	previous, err := decode(before)
	if err != nil {
		return "", err
	}
	next, err := decode(after)
	if err != nil {
		return "", err
	}
	var lines []string
	var walk func(string, map[string]any, map[string]any) error
	walk = func(prefix string, left, right map[string]any) error {
		keys := make([]string, 0, len(left)+len(right))
		for key := range left {
			keys = append(keys, key)
		}
		for key := range right {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			if reflect.DeepEqual(left[key], right[key]) {
				continue
			}
			path := prefix + key
			leftMap, leftOK := left[key].(map[string]any)
			rightMap, rightOK := right[key].(map[string]any)
			if leftOK && rightOK {
				if err := walk(path+".", leftMap, rightMap); err != nil {
					return err
				}
				continue
			}
			oldValue, err := json.Marshal(left[key])
			if err != nil {
				return err
			}
			newValue, err := json.Marshal(right[key])
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("%s: %s → %s", path, oldValue, newValue))
		}
		return nil
	}
	if err := walk("", previous, next); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No settings changed.", nil
	}
	return strings.Join(lines, "\n"), nil
}

func coordinatorPolicyEvidence(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var normalize func(any)
	normalize = func(value any) {
		switch fields := value.(type) {
		case map[string]any:
			if id, ok := fields["policy_id"].(string); ok && id != "" && fields["schema"] == json.Number("1") {
				if gates, ok := fields["gates"].(map[string]any); ok {
					for _, key := range []string{"human_review", "github_pull_request"} {
						if _, present := gates[key]; !present {
							gates[key] = false
						}
					}
				}
			}
			for _, field := range fields {
				normalize(field)
			}
		case []any:
			for _, field := range fields {
				normalize(field)
			}
		}
	}
	normalize(value)
	return json.Marshal(value)
}

func (t *coordinatorToolset) workflowEvidence(ctx context.Context, record conversationRecord) map[string]any {
	evidence := map[string]any{"organization_id": record.OrganizationID, "project_id": record.ProjectID}
	for _, name := range []string{"get_project_integration", "get_project_policy", "get_runners", operatortool.LocalProjectConfiguration} {
		result, err := t.execute(ctx, runner.AgentToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			evidence[name] = map[string]string{"unavailable": coordinatorActionError(err).Error()}
		} else {
			evidence[name] = result
		}
	}
	return evidence
}

func (s *Service) executeCoordinatorWorkflowAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	executor := hubProjectExecutor{s}
	var raw json.RawMessage
	var err error
	message := "Policy approved in the Hub. Verify effective runner adoption separately."
	if string(action.Kind) == "apply_local_project_policy" {
		var args operatortool.LocalProjectArguments
		args, err = operatortool.DecodeLocalProjectArguments(string(action.Kind), action.Arguments, true)
		if err == nil && (args.ProjectID != action.ProjectID || args.RequestID != action.RequestID) {
			err = operatortool.ErrAccessDenied
		}
		if err == nil {
			var result operatortool.Result
			result, err = executor.localProjectConfiguration(ctx, string(action.Kind), args)
			raw = result.Content
		}
		message = "Policy application submitted. Read local_project_configuration for pending status and the runner's adoption receipt."
	} else {
		var args coordinatorPolicyApprovalArguments
		err = decodeCoordinatorArguments(action.Arguments, &args)
		if err == nil && (args.ProjectID != action.ProjectID || args.RequestID != action.RequestID) {
			err = operatortool.ErrAccessDenied
		}
		if err == nil && (args.RunnerID == "" || args.SelectedID == "") {
			err = operatortool.ErrInvalidArguments
		}
		if err == nil {
			var result operatortool.Result
			result, err = executor.localProjectConfiguration(ctx, operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: args.ProjectID, RunnerID: args.RunnerID})
			if err == nil {
				var configuration runnerauth.ProjectConfiguration
				err = json.Unmarshal(result.Content, &configuration)
				if err == nil && (configuration.Source == "configured_local_workflow_read_only" || configuration.RunnerID != args.RunnerID || configuration.SelectedPolicy == nil || configuration.SelectedPolicy.ID != args.SelectedID || !reflect.DeepEqual(*configuration.SelectedPolicy, args.Input.Policy)) {
					err = policyMismatch("Selected policy changed or is unavailable; read local_project_configuration and preview the change again")
				}
			}
		}
		if err == nil {
			raw, err = json.Marshal(operatortool.ProjectRequest[operatortool.PolicyApprovalInput]{ProjectID: args.ProjectID, RequestID: args.RequestID, Input: args.Input})
			if err == nil {
				raw, err = executor.command(ctx, operatortool.Call{Name: string(action.Kind), Arguments: raw}, projectCommandExecute)
			}
		}
	}
	if err != nil {
		executor.AuditAction(ctx, action, "failed")
		return chat.ActionExecution{}, err
	}
	executor.AuditAction(ctx, action, "submitted")
	return chat.ActionExecution{Message: message, Data: raw}, nil
}
