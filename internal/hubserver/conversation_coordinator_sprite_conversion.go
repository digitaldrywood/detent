package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spriteTokenPromotion struct {
	ProjectID   tracker.ProjectID `json:"project_id"`
	Fingerprint string            `json:"fingerprint"`
}

func firstProjectSpritesToken(ctx context.Context, q nativeQueryer, scope nativeScope) (spriteTokenPromotion, string, hubsecrets.Envelope, error) {
	var change spriteTokenPromotion
	var slug string
	var envelope hubsecrets.Envelope
	err := q.QueryRowContext(ctx, `SELECT s.project_id,s.organization_slug,s.ciphertext,s.nonce,s.wrapped_data_key,s.master_key_version FROM project_secrets s JOIN projects p ON p.id=s.project_id AND p.organization_id=s.organization_id WHERE s.organization_id=? AND s.kind=? AND p.deleted_at IS NULL ORDER BY p.scheduling_rank,p.id LIMIT 1`, scope.organization, flySpritesToken).Scan(&change.ProjectID, &slug, &envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	digest := sha256.Sum256(envelope.Ciphertext)
	change.Fingerprint = hex.EncodeToString(digest[:])
	return change, slug, envelope, err
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
	existing, err := readSecretStatus(ctx, s.database.db, scope)
	if err != nil {
		return nil, err
	}
	if existing.Present {
		return nil, nativeInvalid("An organization Sprites token is already configured")
	}
	change, _, _, err := firstProjectSpritesToken(ctx, s.database.db, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nativeInvalid("No existing project Sprites token is available")
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
	err = s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		if err := requireCredentialAuthority(ctx, tx, scope.credential, s.config.now()); err != nil {
			return err
		}
		existing, err := readSecretStatus(ctx, tx, scope)
		if err != nil {
			return err
		}
		if existing.Present {
			return nativeInvalid("An organization Sprites token is already configured")
		}
		current, slug, envelope, err := firstProjectSpritesToken(ctx, tx, scope)
		if err != nil {
			return err
		}
		if current != change {
			return nativeInvalid("The source Sprites token changed; request a new preview")
		}
		token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(change.ProjectID), flySpritesToken))
		if err != nil {
			return err
		}
		defer clear(token)
		promoted, err := s.config.SecretKeys.Seal(token, secretAAD(string(scope.organization), "", flySpritesToken))
		if err != nil {
			return err
		}
		now := formatHubTime(s.config.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO organization_secrets(organization_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES(?,?,?,?,?,?,?,?)`, scope.organization, flySpritesToken, slug, promoted.Ciphertext, promoted.Nonce, promoted.WrappedKey, promoted.Version, now); err != nil {
			return err
		}
		return secretAudit(ctx, tx, string(scope.organization), "", secretActor(scope), flySpritesToken, "set", promoted.Version, now)
	})
	if err != nil {
		return chat.ActionExecution{}, err
	}
	return chat.ActionExecution{Message: "Organization Sprites token configured from the confirmed project token. All project overrides are preserved.", ResourceID: string(scope.organization)}, nil
}
