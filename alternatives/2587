package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func workBoardToolQuery(arguments operatortool.WorkBoardArguments) (url.Values, error) {
	params := url.Values{"format": {"compact"}, "project": arguments.ProjectIDs, "lane": arguments.Lane, "exclude_lane": arguments.ExcludeLane}
	for name, value := range map[string]string{"lanes": arguments.Lanes, "cursor": arguments.Cursor} {
		if value != "" {
			params.Set(name, value)
		}
	}
	if arguments.Archived || arguments.CompletedWindow != "" {
		state := defaultWorkBoardState()
		state.Archived = arguments.Archived
		if arguments.CompletedWindow != "" {
			state.CompletedWindow = arguments.CompletedWindow
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return nil, err
		}
		params.Set("state", string(raw))
	}
	for name, values := range params {
		if len(values) == 0 {
			delete(params, name)
		}
	}
	return params, nil
}

func (s *Service) executeWorkBoard(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var arguments operatortool.WorkBoardArguments
	if err := operatortool.DecodeArguments(call.Arguments, &arguments); err != nil {
		return operatortool.Result{}, err
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
	if err != nil {
		return operatortool.Result{}, fmt.Errorf("authorize board: %w", err)
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	organization := arguments.OrganizationID
	if organization == "" {
		organization = string(scope.Organization)
	}
	params, err := workBoardToolQuery(arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	request, err := parseWorkBoardRequest(params)
	if err != nil {
		return operatortool.Result{}, hubapi.SafeWorkReadError(err)
	}
	view, err := s.readWorkBoardFor(ctx, scope.Credential, organization, request)
	if err != nil {
		return operatortool.Result{}, hubapi.SafeWorkReadError(err)
	}
	return operatortool.EncodeResult(view.compact)
}
