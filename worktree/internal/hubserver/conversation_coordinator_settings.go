package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coordinatorSettingsNames() []string {
	return []string{operatortool.UpdateRunnerCapacity, operatortool.UpdateRunnerRouting, operatortool.UpdateProjectModelSelection, "bind_native_repository", "revoke_project_policy", "remove_project_secret", "save_onboarding"}
}

func coordinatorSettingsTool(name string) bool {
	return slices.Contains(coordinatorSettingsNames(), name)
}

func coordinatorSettingsTools() []runner.AgentTool {
	tools := []runner.AgentTool{}
	for _, name := range coordinatorSettingsNames() {
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
			tools = append(tools, coordinatorTool(name, definition.Description+" Prepare a confirmation card using the current viewer's authority. Read configuration-map and current settings first. The project and retry key are supplied by chat. Submission reads back the result; a preview has not executed.", string(raw)))
		}
	}
	return tools
}

func coordinatorSettingsRequirement(name, project string) operatortool.Requirement {
	if name == operatortool.UpdateRunnerCapacity || name == operatortool.UpdateRunnerRouting {
		r := hubFleetRequirement(name)
		r.ProjectID = project
		return r
	}
	return operatortool.Requirement{Scope: projectToolScope(name, false), ProjectID: project}
}

func coordinatorSettingsRefusal(name string) error {
	permission, path := "project settings administration (organization owner or admin)", "Settings > Project"
	if name == "save_onboarding" {
		permission = "project write access"
	}
	if name == operatortool.UpdateRunnerCapacity || name == operatortool.UpdateRunnerRouting {
		permission, path = "runner administration (manage_runner grants)", "Settings > Providers & runners > runner"
	}
	return fmt.Errorf("you don't have access to %s; ask an administrator at %s: %w", permission, path, operatortool.ErrAccessDenied)
}

func (s *Service) coordinatorSettingsTarget(ctx context.Context, record conversationRecord, name string, raw json.RawMessage) error {
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
	r, err := readRunnerWithClock(ctx, s.database.reader, record.OrganizationID, target.RunnerID, s.config.now)
	if err != nil || !r.AllowsProject(record.ProjectID) {
		return coordinatorSettingsRefusal(name)
	}
	if name == operatortool.UpdateRunnerRouting {
		var change runnerRoutingRequest
		if operatortool.DecodeArguments(target.Change, &change) != nil {
			return operatortool.ErrInvalidArguments
		}
		for _, project := range append(slices.Clone(r.ProjectIDs), change.ProjectIDs...) {
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(project)}); err != nil {
				return coordinatorSettingsRefusal(name)
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

func (t *coordinatorToolset) settingsTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, coordinatorSettingsRefusal(call.Name)
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, coordinatorSettingsRequirement(call.Name, string(record.ProjectID)))
	if err != nil {
		return nil, coordinatorSettingsRefusal(call.Name)
	}
	s := t.coordinator.service.server
	var fields map[string]json.RawMessage
	if len(call.Arguments) > coordinatorToolArgumentBytes || json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return nil, operatortool.ErrInvalidArguments
	}
	for _, key := range []string{"project_id", "request_id"} {
		if _, present := fields[key]; present {
			return nil, operatortool.ErrInvalidArguments
		}
	}
	digest := sha256.Sum256([]byte(t.state.users[len(t.state.users)-1].ID + ":" + call.Name + ":" + string(call.Arguments)))
	requestID := "luna_" + hex.EncodeToString(digest[:])
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
	if err := s.coordinatorSettingsTarget(ctx, record, call.Name, raw); err != nil {
		return nil, err
	}
	action := chat.Action{Kind: chat.ActionKind(call.Name), ProjectID: string(record.ProjectID), RequestID: requestID, Title: strings.ReplaceAll(call.Name, "_", " "), Arguments: raw, Material: true}
	if hubFleetTool(call.Name) {
		credential, err := currentHubOperator(ctx)
		if err != nil || !s.hubFleetReadPermitted(call.Name, credential) {
			return nil, coordinatorSettingsRefusal(call.Name)
		}
		proposal, err := (hubFleetExecutor{s}).proposal(ctx, call.Name, raw)
		if err != nil {
			return nil, err
		}
		action.IssueID, action.Identifier, action.CurrentState, action.MaterialChange = proposal.IssueID, proposal.Identifier, proposal.CurrentState, proposal.MaterialChange
	} else if _, err := (hubProjectExecutor{s}).command(ctx, operatortool.Call{Name: call.Name, Arguments: raw}, projectCommandPreview); err != nil {
		if errors.Is(err, operatortool.ErrAccessDenied) {
			return nil, coordinatorSettingsRefusal(call.Name)
		}
		return nil, err
	}
	action.Description = "Confirm " + action.Title + ": " + string(call.Arguments)
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (s *Service) executeCoordinatorSettingsAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	record := conversationRecord{OrganizationID: tracker.OrganizationID(action.OrganizationID), ProjectID: tracker.ProjectID(action.ProjectID)}
	name := string(action.Kind)
	if err := s.coordinatorSettingsTarget(ctx, record, name, action.Arguments); err != nil {
		return chat.ActionExecution{}, err
	}
	if !hubFleetTool(name) {
		return (hubProjectExecutor{s}).ExecuteAction(ctx, action)
	}
	credential, err := currentHubOperator(ctx)
	if err != nil || !s.hubFleetReadPermitted(name, credential) {
		return chat.ActionExecution{}, coordinatorSettingsRefusal(name)
	}
	result, err := (hubFleetExecutor{s}).ExecuteAction(ctx, action)
	if err != nil {
		return result, err
	}
	if name == operatortool.UpdateRunnerCapacity {
		var args hubFleetRequest
		if operatortool.DecodeArguments(action.Arguments, &args) != nil {
			return result, operatortool.ErrInvalidArguments
		}
		var change runnerCapacityChange
		if operatortool.DecodeArguments(args.Change, &change) != nil {
			return result, operatortool.ErrInvalidArguments
		}
		view, readErr := s.readRunnerCapacity(ctx, nativeScope{organization: record.OrganizationID, credential: credential}, args.RunnerID, change.Backend)
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
		view, readErr := s.coordinatorProjectRunners(ctx, record, credential, true)
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
