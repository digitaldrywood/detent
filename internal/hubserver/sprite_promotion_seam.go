package hubserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/spritepromotion"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (s *Service) spritePromotionService(scope hubscope.NativeScope) *spritepromotion.Service {
	return spritepromotion.New(spritepromotion.Deps{
		Organization: scope.Organization, Keys: s.config.SecretKeys, Now: s.config.now, Decode: decodeCoordinatorArguments,
		Read: func(ctx context.Context, read func(tenantstore.Reader) error) error {
			return s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
				if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
					return err
				}
				return read(r)
			})
		},
		Write: func(ctx context.Context, write func(tenantstore.Writer) error) error {
			return s.accessSecretWrite(ctx, scope, write)
		},
		Audit: func(event string, version int) accessstore.SecretAudit {
			return s.accessSecretAudit(scope, event, version)
		},
	})
}

func (t *coordinatorToolset) promoteSpritesTokenTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct{}
	if decodeCoordinatorArguments(call.Arguments, &args) != nil {
		return nil, operatortool.ErrInvalidArguments
	}
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	s := t.coordinator.server
	scope, err := s.coordinatorScope(ctx, "", true)
	if err != nil {
		return nil, err
	}
	change, err := s.spritePromotionService(scope).Preview(ctx)
	if err != nil {
		return nil, hubapi.FromFeatureError(err)
	}
	raw, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	return t.submitCoordinatorAction(ctx, record, call, chat.Action{Kind: chat.ActionKind(call.Name), Title: "Use existing Sprites token for the organization", Description: fmt.Sprintf("Use the Sprites token from project %s as the organization default. Every project token stays as an override. The token remains write-only. Confirm this organization-wide credential change.", change.ProjectID), ProjectID: string(record.ProjectID), Arguments: raw, Material: true})
}

func (s *Service) executePromoteSpritesToken(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	scope, err := s.coordinatorScope(ctx, "", true)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	result, err := s.spritePromotionService(scope).Execute(ctx, action.Arguments)
	return result, hubapi.FromFeatureError(err)
}
