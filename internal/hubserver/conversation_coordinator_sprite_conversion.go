package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spriteTokenPromotion struct {
	ProjectID   tracker.ProjectID `json:"project_id"`
	Fingerprint string            `json:"fingerprint"`
}

func firstProjectSpritesToken(ctx context.Context, r accessstore.Reader) (spriteTokenPromotion, accessstore.Secret, error) {
	secret, err := r.FirstProjectAccessSecret(ctx, flySpritesToken)
	if err != nil {
		return spriteTokenPromotion{}, secret, err
	}
	digest := sha256.Sum256(secret.Envelope.Ciphertext)
	return spriteTokenPromotion{ProjectID: secret.ProjectID, Fingerprint: hex.EncodeToString(digest[:])}, secret, nil
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
	var change spriteTokenPromotion
	err = s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if _, err := r.AccessSecret(ctx, "", flySpritesToken); err == nil {
			return nativePublicInvalid("An organization Sprites token is already configured")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		var err error
		change, _, err = firstProjectSpritesToken(ctx, r)
		if errors.Is(err, corestore.ErrNotFound) {
			return nativePublicInvalid("No existing project Sprites token is available")
		}
		return err
	})
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
	err = s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		if _, err := w.AccessSecret(ctx, "", flySpritesToken); err == nil {
			return nativePublicInvalid("An organization Sprites token is already configured")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		current, source, err := firstProjectSpritesToken(ctx, w)
		if errors.Is(err, corestore.ErrNotFound) {
			return nativePublicInvalid("The source Sprites token changed; request a new preview")
		}
		if err != nil {
			return err
		}
		if current != change {
			return nativePublicInvalid("The source Sprites token changed; request a new preview")
		}
		token, err := s.config.SecretKeys.Open(source.Envelope, accessstore.SecretAAD(scope.organization, source.ProjectID, flySpritesToken))
		if err != nil {
			return err
		}
		defer clear(token)
		promoted, err := s.config.SecretKeys.Seal(token, accessstore.SecretAAD(scope.organization, "", flySpritesToken))
		if err != nil {
			return err
		}
		secret := accessstore.Secret{OrganizationID: scope.organization, Kind: flySpritesToken, OrganizationSlug: source.OrganizationSlug, Envelope: promoted, UpdatedAt: s.config.now()}
		err = w.PromoteAccessSecret(ctx, source, secret, s.accessSecretAudit(scope, "set", promoted.Version))
		if errors.Is(err, corestore.ErrUniqueViolation) {
			return nativePublicInvalid("An organization Sprites token is already configured")
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nativePublicInvalid("The source Sprites token changed; request a new preview")
		}
		return err
	})
	if err != nil {
		return chat.ActionExecution{}, err
	}
	return chat.ActionExecution{Message: "Organization Sprites token configured from the confirmed project token. All project overrides are preserved.", ResourceID: string(scope.organization)}, nil
}
