package web

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
)

func (s *Server) localProjectTool(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, err := operatortool.DecodeLocalProjectArguments(call.Name, call.Arguments, true)
	if err != nil {
		return operatortool.Result{}, err
	}
	scope := apikey.ScopeAdmin
	if call.Name == operatortool.LocalProjectConfiguration {
		scope = apikey.ScopeRead
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	if s.projectConfigOwner == nil {
		return operatorResult(project.MissingConfigurationOwner(r.ProjectID))
	}
	if call.Name == operatortool.LocalProjectConfiguration {
		return operatorResult(s.projectConfigOwner.Read(ctx, r.ProjectID))
	}
	return s.executeOperatorMutation(ctx, call)
}

func localProjectAction(name string, raw json.RawMessage) (chatpkg.Action, error) {
	r, err := operatortool.DecodeLocalProjectArguments(name, raw, false)
	if err != nil {
		return chatpkg.Action{}, err
	}
	return chatpkg.Action{Kind: chatpkg.ActionKind(name), ProjectID: r.ProjectID, Title: strings.ReplaceAll(name, "_", " "), Reason: "Use the selected local configuration owner and preserve Cloud routing and native Change authority."}, nil
}

func (s *Server) executeLocalProjectAction(ctx context.Context, action chatpkg.Action) (chatpkg.ActionExecution, error) {
	r, err := operatortool.DecodeLocalProjectArguments(string(action.Kind), action.Arguments, false)
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	view := s.projectConfigOwner.Apply(ctx, string(action.Kind), project.ManagedConfigRequest{ProjectID: r.ProjectID, ExpectedConfigRevision: r.ExpectedConfigRevision, ExpectedPolicyID: r.ExpectedPolicyID, PolicyID: r.PolicyID, SourceRevision: r.SourceRevision, Checkpoint: r.Checkpoint})
	result, err := operatorResult(view)
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	return chatpkg.ActionExecution{Message: string(result.Content), Data: result.Content, ResourceID: r.ProjectID}, nil
}
