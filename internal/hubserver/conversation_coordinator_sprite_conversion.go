package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spriteTokenPromotion struct {
	ProjectID   tracker.ProjectID `json:"project_id"`
	Fingerprint string            `json:"fingerprint"`
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
	s := t.coordinator.service.server
	scope, err := s.coordinatorScope(ctx, "", true)
	if err != nil {
		return nil, err
	}
	existing, err := s.readSecretStatus(ctx, s.database.reader, scope)
	if err != nil {
		return nil, err
	}
	if existing.Present {
		return nil, nativePublicInvalid("An organization Sprites token is already configured")
	}
	change, _, _, err := s.firstProjectSpritesTokenRecord(ctx, scope)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nativePublicInvalid("No existing project Sprites token is available")
	}
	if err != nil {
		return nil, err
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
	var change spriteTokenPromotion
	if decodeCoordinatorArguments(action.Arguments, &change) != nil || change.ProjectID == "" || change.Fingerprint == "" {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	if s.config.SecretKeys == nil {
		return chat.ActionExecution{}, hubsecrets.ErrUnavailable
	}
	if err := s.promoteSpritesTokenRecord(ctx, scope, change); err != nil {
		return chat.ActionExecution{}, err
	}
	return chat.ActionExecution{Message: "Organization Sprites token configured from the confirmed project token. All project overrides are preserved.", ResourceID: string(scope.organization)}, nil
}
