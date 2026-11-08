package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func spritePromotionPreview(secret accessstore.Secret) spriteTokenPromotion {
	digest := sha256.Sum256(secret.Envelope.Ciphertext)
	return spriteTokenPromotion{ProjectID: secret.ProjectID, Fingerprint: hex.EncodeToString(digest[:])}
}

func (s *Service) firstProjectSpritesTokenRecord(ctx context.Context, scope nativeScope) (spriteTokenPromotion, string, hubsecrets.Envelope, error) {
	source, err := s.spriteTokenPromotionSource(ctx, scope)
	return spritePromotionPreview(source), source.OrganizationSlug, source.Envelope, err
}

func (s *Service) spriteTokenPromotionSource(ctx context.Context, scope nativeScope) (accessstore.Secret, error) {
	var source accessstore.Secret
	if s.config.TenantRepository == nil {
		return source, accessstore.ErrTransactionRequired
	}
	if !canManageProjectSecrets(scope.credential) {
		return source, accessstore.ErrDenied
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if _, err := r.AccessSecret(ctx, "", flySpritesToken); err == nil {
			return nativePublicInvalid("An organization Sprites token is already configured")
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		var err error
		source, err = owner.SpritePromotionSource(ctx)
		return err
	})
	return source, err
}

func (s *Service) promoteSpritesTokenRecord(ctx context.Context, scope nativeScope, change spriteTokenPromotion) error {
	if s.config.SecretKeys == nil {
		return hubsecrets.ErrUnavailable
	}
	source, err := s.spriteTokenPromotionSource(ctx, scope)
	if err != nil {
		return err
	}
	if spritePromotionPreview(source) != change {
		return nativePublicInvalid("The source Sprites token changed; request a new preview")
	}
	plain, err := s.config.SecretKeys.Open(source.Envelope, accessstore.SecretAAD(scope.organization, source.ProjectID, flySpritesToken))
	if err != nil {
		return err
	}
	defer clear(plain)
	envelope, err := s.config.SecretKeys.Seal(plain, accessstore.SecretAAD(scope.organization, "", flySpritesToken))
	if err != nil {
		return err
	}
	promoted := accessstore.Secret{OrganizationID: scope.organization, Kind: flySpritesToken, OrganizationSlug: source.OrganizationSlug, Envelope: envelope, UpdatedAt: s.config.now()}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		if err := owner.PromoteSpriteSecret(ctx, source, promoted); err != nil {
			return err
		}
		return w.RecordAccessSecretAudit(ctx, accessstore.SecretAudit{OrganizationID: scope.organization, Actor: secretActor(scope), Kind: flySpritesToken, Event: "set", KeyVersion: envelope.Version, RecordedAt: promoted.UpdatedAt})
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nativePublicInvalid("The Sprites token changed; request a new preview")
	}
	return err
}
