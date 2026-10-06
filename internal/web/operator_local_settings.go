package web

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func localSettingsTool(name string) bool {
	return operatortool.IsModelSelection(name) || name == operatortool.OrganizationProjectRank || name == operatortool.OrganizationProjectRankUpdate || name == operatortool.GetRunnerRouting || name == operatortool.UpdateRunnerRouting
}

func (s *Server) usesLocalSettingsTool(ctx context.Context, name string) bool {
	return localSettingsTool(name) && s.hasLocalSettings(ctx)
}

func localSettingsReceipt(name string) bool {
	return localSettingsTool(name) || operatortool.IsLocalProjectTool(name)
}

func (s *Server) hasLocalSettings(ctx context.Context) bool {
	if s.store == nil || s.currentGlobalConfig().Client.Configured() {
		return false
	}
	_, err := s.store.LocalProjectRank(ctx, nil)
	return err == nil
}

func (s *Server) localSettingsTools(ctx context.Context) []operatortool.Definition {
	if !s.hasLocalSettings(ctx) {
		return nil
	}
	definitions := operatortool.ModelSelectionCatalog()
	for _, d := range append(operatortool.AdministrationCatalog(), operatortool.FleetCatalog()...) {
		if localSettingsTool(d.Name) {
			definitions = append(definitions, d)
		}
	}
	out := []operatortool.Definition{}
	for _, d := range definitions {
		if _, err := operatortool.AuthorizeCurrent(ctx, localSettingsRequirement(d.Name, "")); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) localSettingsExecute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if !s.hasLocalSettings(ctx) {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	project, err := localSettingsProject(call)
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, localSettingsRequirement(call.Name, project))
	if err != nil {
		return operatortool.Result{}, err
	}
	d, ok := operatortool.Lookup(call.Name)
	if !ok {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if !d.Annotations.ReadOnly {
		return s.executeOperatorMutation(ctx, call)
	}
	value, err := s.applyLocalSettings(ctx, call)
	if err != nil {
		return operatortool.Result{}, err
	}
	return operatorResult(value)
}

func localSettingsProject(call operatortool.Call) (string, error) {
	if operatortool.IsModelSelection(call.Name) {
		if call.Name == operatortool.GetOrganizationModelSelection || call.Name == operatortool.GetProjectModelSelection {
			var r operatortool.ModelSelectionReadRequest
			if err := operatortool.DecodeModelSelectionArguments(call, &r); err != nil {
				return "", err
			}
			return r.ProjectID, nil
		}
		var r operatortool.ProjectRequest[operatortool.ModelSelectionInput]
		if err := operatortool.DecodeModelSelectionArguments(call, &r); err != nil {
			return "", err
		}
		return r.ProjectID, nil
	}
	if call.Name == operatortool.GetRunnerRouting || call.Name == operatortool.UpdateRunnerRouting {
		if err := operatortool.ValidateFleetArguments(call.Name, call.Arguments); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (s *Server) localSettingsAction(name string, raw json.RawMessage) (chatpkg.Action, error) {
	var fields struct {
		ProjectID string `json:"project_id,omitempty"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return chatpkg.Action{}, operatortool.ErrInvalidArguments
	}
	return chatpkg.Action{Kind: chatpkg.ActionKind(name), ProjectID: fields.ProjectID, Title: strings.ReplaceAll(name, "_", " "), Reason: "Update the local SQLite configuration through the shared settings owner."}, nil
}

func (s *Server) executeLocalSettingsAction(ctx context.Context, action chatpkg.Action) (chatpkg.ActionExecution, error) {
	value, err := s.applyLocalSettings(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments})
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	result, err := operatorResult(value)
	return chatpkg.ActionExecution{Message: string(result.Content), Data: result.Content, ResourceID: action.ProjectID}, err
}

func (s *Server) applyLocalSettings(ctx context.Context, call operatortool.Call) (any, error) {
	switch call.Name {
	case operatortool.OrganizationProjectRank:
		var empty struct{}
		if err := operatortool.DecodeArguments(call.Arguments, &empty); err != nil {
			return nil, err
		}
		return s.store.LocalProjectRank(ctx, nil)
	case operatortool.OrganizationProjectRankUpdate:
		var change projectsettings.RankChange
		if err := operatortool.DecodeArguments(call.Arguments, &change); err != nil {
			return nil, err
		}
		return s.store.LocalProjectRank(ctx, &change)
	case operatortool.GetOrganizationModelSelection, operatortool.GetProjectModelSelection:
		var r operatortool.ModelSelectionReadRequest
		if err := operatortool.DecodeArguments(call.Arguments, &r); err != nil {
			return nil, err
		}
		return s.store.LocalModelSelection(ctx, tracker.ProjectID(r.ProjectID), nil)
	case operatortool.UpdateOrganizationModelSelection, operatortool.UpdateProjectModelSelection:
		var r operatortool.ProjectRequest[operatortool.ModelSelectionInput]
		if err := operatortool.DecodeArguments(call.Arguments, &r); err != nil {
			return nil, err
		}
		change := projectsettings.ModelSelectionChange{ExpectedRevision: r.Input.ExpectedRevision, Selection: r.Input.Selection}
		return s.store.LocalModelSelection(ctx, tracker.ProjectID(r.ProjectID), &change)
	case operatortool.GetRunnerRouting, operatortool.UpdateRunnerRouting:
		var r struct {
			RunnerID string                    `json:"runner_id"`
			Change   *runnerauth.RoutingChange `json:"change,omitempty"`
		}
		if err := operatortool.DecodeArguments(call.Arguments, &r); err != nil || r.RunnerID != store.LocalRunner {
			return nil, operatortool.ErrInvalidArguments
		}
		var change *store.AllowedProjectsChange
		if call.Name == operatortool.UpdateRunnerRouting {
			if r.Change == nil {
				return nil, operatortool.ErrInvalidArguments
			}
			current, err := s.localRouting(ctx, nil)
			if err != nil {
				return nil, err
			}
			expected := current.Routing
			expected.ProjectIDs = r.Change.ProjectIDs
			left, err := json.Marshal(expected)
			if err != nil {
				return nil, err
			}
			right, err := json.Marshal(r.Change.Routing)
			if err != nil || string(left) != string(right) {
				return nil, operatortool.ErrInvalidArguments
			}
			change = &store.AllowedProjectsChange{ExpectedRevision: r.Change.ExpectedRevision, ProjectIDs: r.Change.ProjectIDs}
		}
		return s.localRouting(ctx, change)
	default:
		return nil, operatortool.ErrInvalidArguments
	}
}

func (s *Server) localRouting(ctx context.Context, change *store.AllowedProjectsChange) (runnerauth.RoutingSnapshot, error) {
	value, err := s.store.LocalAllowedProjects(ctx, change)
	if err != nil {
		return runnerauth.RoutingSnapshot{}, err
	}
	cfg := s.currentGlobalConfig()
	name := strings.TrimSpace(cfg.InstanceName)
	if name == "" {
		name = "Local instance"
	}
	return runnerauth.RoutingSnapshot{RunnerID: store.LocalRunner, Revision: value.Revision, Routing: runnerauth.Routing{DisplayName: name, Tags: []string{}, State: "active", ProjectIDs: value.ProjectIDs, IsolationTier: "local", HostServices: []string{}, Availability: runnerauth.Availability{Windows: []string{}}}}, nil
}

func localSettingsRequirement(name, project string) operatortool.Requirement {
	requirement := operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: project, OrganizationWide: name != operatortool.GetProjectModelSelection && name != operatortool.UpdateProjectModelSelection}
	if name == operatortool.GetProjectModelSelection || name == operatortool.GetOrganizationModelSelection {
		requirement.Scope = apikey.ScopeRead
	}
	return requirement
}
