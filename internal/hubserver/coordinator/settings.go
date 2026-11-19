package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func SettingsNames() []string {
	return []string{operatortool.UpdateRunnerCapacity, operatortool.UpdateRunnerRouting, operatortool.UpdateProjectModelSelection, "bind_native_repository", "revoke_project_policy", "remove_project_secret", "save_onboarding"}
}

func SettingsTool(name string) bool {
	return slices.Contains(SettingsNames(), name)
}

func SettingsTools() []runner.AgentTool {
	tools := []runner.AgentTool{}
	for _, name := range SettingsNames() {
		definition, ok := operatortool.Lookup(name)
		if !ok || definition.Annotations.ReadOnly {
			continue
		}
		var schema map[string]any
		if json.Unmarshal(definition.InputSchema, &schema) != nil || schema == nil {
			continue
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			continue
		}
		delete(properties, "project_id")
		delete(properties, "request_id")
		if required, ok := schema["required"].([]any); ok {
			schema["required"] = slices.DeleteFunc(required, func(value any) bool { return value == "project_id" || value == "request_id" })
		}
		raw, err := json.Marshal(schema)
		if err == nil {
			tools = append(tools, Tool(name, definition.Description+" Prepare a confirmation card using the current viewer's authority. Read configuration-map and current settings first. The project and retry key are supplied by chat. Submission reads back the result; a preview has not executed.", string(raw)))
		}
	}
	return tools
}

func SettingsRequirement(name, project string, fleetRequirement func(string) operatortool.Requirement, projectScope func(string, bool) apikey.Scope) operatortool.Requirement {
	if name == operatortool.UpdateRunnerCapacity || name == operatortool.UpdateRunnerRouting {
		r := fleetRequirement(name)
		r.ProjectID = project
		return r
	}
	return operatortool.Requirement{Scope: projectScope(name, false), ProjectID: project}
}

func SettingsRefusal(name string) error {
	permission, path := "project settings administration (organization owner or admin)", "Settings > Project"
	if name == "save_onboarding" {
		permission = "project write access"
	}
	if name == operatortool.UpdateRunnerCapacity || name == operatortool.UpdateRunnerRouting {
		permission, path = "runner administration (manage_runner grants)", "Settings > Providers & runners > runner"
	}
	return fmt.Errorf("you don't have access to %s; ask an administrator at %s: %w", permission, path, operatortool.ErrAccessDenied)
}

func (s SettingsDeps) CheckTarget(ctx context.Context, record conversations.ConversationRecord, name string, raw json.RawMessage) error {
	var target struct {
		ProjectID string          `json:"project_id"`
		RunnerID  string          `json:"runner_id"`
		Change    json.RawMessage `json:"change"`
	}
	if json.Unmarshal(raw, &target) != nil || target.ProjectID != string(record.ProjectID) {
		return operatortool.ErrInvalidArguments
	}
	if name != operatortool.UpdateRunnerCapacity && name != operatortool.UpdateRunnerRouting {
		return nil
	}
	credential, err := s.Operator(ctx)
	if err != nil {
		return SettingsRefusal(name)
	}
	r, err := s.Runner(ctx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, target.RunnerID)
	if err != nil || !r.AllowsProject(record.ProjectID) {
		return SettingsRefusal(name)
	}
	if name == operatortool.UpdateRunnerRouting {
		change, err := s.DecodeRouting(target.Change)
		if err != nil {
			return operatortool.ErrInvalidArguments
		}
		for _, project := range append(slices.Clone(r.ProjectIDs), change.ProjectIDs...) {
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(project)}); err != nil {
				return SettingsRefusal(name)
			}
		}
		for _, project := range r.ProjectIDs {
			if project != record.ProjectID && (!slices.Contains(change.ProjectIDs, project) || change.ProjectRanks[project] != r.ProjectRanks[project]) {
				return fmt.Errorf("%w: preserve other projects' routing; use Settings > Providers & runners for shared runner routing", operatortool.ErrInvalidArguments)
			}
		}
		for _, project := range change.ProjectIDs {
			if project != record.ProjectID && !slices.Contains(r.ProjectIDs, project) {
				return operatortool.ErrInvalidArguments
			}
		}
		for project, rank := range change.ProjectRanks {
			if project != record.ProjectID && (!slices.Contains(r.ProjectIDs, project) || rank != r.ProjectRanks[project]) {
				return operatortool.ErrInvalidArguments
			}
		}
	}
	return nil
}

func PreviewSettings(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall, deps SettingsPreviewDeps) (any, error) {
	ctx, err := deps.ActionContext(ctx, record)
	if err != nil {
		return nil, SettingsRefusal(call.Name)
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, deps.Requirement(call.Name, string(record.ProjectID)))
	if err != nil {
		return nil, SettingsRefusal(call.Name)
	}
	s := deps.Settings
	var fields map[string]json.RawMessage
	if len(call.Arguments) > hubapi.CoordinatorToolArgumentBytes || json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return nil, operatortool.ErrInvalidArguments
	}
	for _, key := range []string{"project_id", "request_id"} {
		if _, present := fields[key]; present {
			return nil, operatortool.ErrInvalidArguments
		}
	}
	requestID := ActionRequestID(deps.MessageID(), call)
	fields["project_id"], err = json.Marshal(record.ProjectID)
	if err != nil {
		return nil, err
	}
	fields["request_id"], err = json.Marshal(requestID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if err := s.CheckTarget(ctx, record, call.Name, raw); err != nil {
		return nil, err
	}
	action := chat.Action{Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), RequestID: requestID, Title: strings.ReplaceAll(call.Name, "_", " "), Arguments: raw, Material: true}
	if s.FleetTool(call.Name) {
		credential, err := s.Operator(ctx)
		if err != nil || !s.FleetPermitted(call.Name, credential) {
			return nil, SettingsRefusal(call.Name)
		}
		proposal, err := deps.PreviewFleet(ctx, call.Name, raw)
		if err != nil {
			return nil, err
		}
		action.IssueID, action.Identifier, action.CurrentState, action.MaterialChange = proposal.IssueID, proposal.Identifier, proposal.CurrentState, proposal.MaterialChange
	} else if _, err := deps.PreviewProject(ctx, operatortool.Call{Name: call.Name, Arguments: raw}); err != nil {
		if errors.Is(deps.ProjectError(err), operatortool.ErrAccessDenied) {
			return nil, SettingsRefusal(call.Name)
		}
		return nil, err
	}
	action.Description = "Confirm " + action.Title + ": " + string(call.Arguments)
	return deps.Submit(ctx, record, call, action)
}

func (s SettingsDeps) Execute(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	record := conversations.ConversationRecord{OrganizationID: tracker.OrganizationID(action.OrganizationID), ProjectID: tracker.ProjectID(action.ProjectID)}
	name := string(action.Kind)
	if err := s.CheckTarget(ctx, record, name, action.Arguments); err != nil {
		return chat.ActionExecution{}, err
	}
	if !s.FleetTool(name) {
		return s.ExecuteProject(ctx, action)
	}
	credential, err := s.Operator(ctx)
	if err != nil || !s.FleetPermitted(name, credential) {
		return chat.ActionExecution{}, SettingsRefusal(name)
	}
	result, err := s.ExecuteFleet(ctx, action)
	if err != nil {
		return result, err
	}
	if name == operatortool.UpdateRunnerCapacity {
		runnerID, backend, err := s.DecodeCapacity(action.Arguments)
		if err != nil {
			return result, operatortool.ErrInvalidArguments
		}
		view, readErr := s.ReadCapacity(ctx, hubscope.NativeScope{Organization: record.OrganizationID, Credential: credential}, runnerID, backend)
		if readErr != nil {
			result.Message = "Capacity change accepted; current application evidence is unavailable. Read get_runner_capacity again."
			return result, nil
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return result, err
		}
		result.Message = "Capacity change accepted. Current configuration and application evidence: " + string(raw)
	} else {
		view, readErr := s.ReadProjectRunners(ctx, record, credential, true)
		if readErr != nil {
			result.Message = "Routing change accepted; current evidence is unavailable. Read get_project_runners again."
			return result, nil
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return result, err
		}
		result.Message = "Routing changed. Current project runners: " + string(raw)
	}
	return result, nil
}
