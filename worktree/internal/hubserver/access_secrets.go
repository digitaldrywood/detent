package hubserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func accessSecretStatus(secret accessstore.Secret) projectSecretStatus {
	return projectSecretStatus{Kind: secret.Kind, Present: true, OrganizationSlug: secret.OrganizationSlug, KeyVersion: secret.Envelope.Version}
}

func (s *Service) readAccessSecretStatus(ctx context.Context, scope nativeScope) (projectSecretStatus, error) {
	result := projectSecretStatus{Kind: flySpritesToken}
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		secret, err := r.AccessSecret(ctx, scope.project, flySpritesToken)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		result = accessSecretStatus(secret)
		return nil
	})
	return result, err
}

func (s *Service) accessSecretWrite(ctx context.Context, scope nativeScope, write func(tenantstore.Writer) error) error {
	if scope.credential.Hosted != nil {
		_, membership, err := s.hostedMutationIdentity(ctx, scope.credential)
		if err != nil {
			return err
		}
		if membership.ID != scope.credential.HostedMembership {
			return accessstore.ErrDenied
		}
		scope.credential.HostedRole = lesserHostedRole(scope.credential.HostedRole, membership.Role.Slug)
	}
	return s.accessWrite(ctx, scope.organization, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		return write(w)
	})
}

func (s *Service) accessSecretAudit(scope nativeScope, event string, version int) accessstore.SecretAudit {
	return accessstore.SecretAudit{OrganizationID: scope.organization, ProjectID: scope.project, Actor: secretActor(scope), Kind: flySpritesToken, Event: event, KeyVersion: version, RecordedAt: s.config.now()}
}

func (s *Service) setAccessProjectSecret(c echo.Context, scope nativeScope, token []byte) error {
	ctx := c.Request().Context()
	if err := s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		return w.RecordAccessSecretAudit(ctx, s.accessSecretAudit(scope, "use", s.config.SecretKeys.Version()))
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	slug, err := validateSpritesToken(ctx, s.config.SpritesHTTPClient, token)
	if err != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, err.Error())
	}
	envelope, err := s.config.SecretKeys.Seal(token, accessstore.SecretAAD(scope.organization, scope.project, flySpritesToken))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	clear(token)
	secret := accessstore.Secret{OrganizationID: scope.organization, ProjectID: scope.project, Kind: flySpritesToken, OrganizationSlug: slug, Envelope: envelope, UpdatedAt: s.config.now()}
	if err := accessstore.VerifySecret(s.config.SecretKeys, secret); err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		old, err := w.AccessSecret(ctx, scope.project, flySpritesToken)
		event := "set"
		if err == nil {
			event = "replace"
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && old.Envelope.Version <= 0 {
			return accessstore.ErrDenied
		}
		return w.StoreAccessSecret(ctx, secret, s.accessSecretAudit(scope, event, envelope.Version))
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, accessSecretStatus(secret))
}

func (s *Service) removeAccessProjectSecret(ctx context.Context, scope nativeScope) error {
	return s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		old, err := w.AccessSecret(ctx, scope.project, flySpritesToken)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return w.DeleteAccessSecret(ctx, scope.project, flySpritesToken, s.accessSecretAudit(scope, "remove", old.Envelope.Version))
	})
}

func (s *Service) checkAccessSecretKeys(ctx context.Context) error {
	return accessstore.VerifyTenantSecrets(ctx, func(ctx context.Context, read func(accessstore.Reader) error) error {
		return s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error { return read(r) })
	}, s.config.SecretKeys)
}

func (s *Service) rewrapAccessSecrets(ctx context.Context) error {
	var project tracker.ProjectID
	var kind string
	for {
		var page []accessstore.Secret
		err := s.config.AccessRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Background, func(r tenantstore.Reader) error {
			var err error
			page, err = r.AccessSecrets(ctx, project, kind, 200)
			return err
		})
		if err != nil {
			return err
		}
		for _, secret := range page {
			project, kind = secret.ProjectID, secret.Kind
			if secret.Envelope.Version == s.config.SecretKeys.Version() {
				continue
			}
			envelope, err := s.config.SecretKeys.Rewrap(secret.Envelope, accessstore.SecretAAD(secret.OrganizationID, secret.ProjectID, secret.Kind))
			if err != nil {
				return err
			}
			err = s.config.AccessRepository.WriteTenant(ctx, secret.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
				return w.RewrapAccessSecret(ctx, secret, envelope, accessstore.SecretAudit{OrganizationID: secret.OrganizationID, ProjectID: secret.ProjectID, Kind: secret.Kind, Actor: "system:rewrap", Event: "rotate", KeyVersion: envelope.Version, RecordedAt: s.config.now()})
			})
			if err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
				return err
			}
		}
		if len(page) < 200 {
			return nil
		}
	}
}

func (s *Service) resolveSpritesSecret(ctx context.Context, db nativeQueryer, scope nativeScope) (nativeScope, string, hubsecrets.Envelope, error) {
	if s.config.AccessRepository == nil {
		return resolveSpritesSecret(ctx, db, scope)
	}
	var secret accessstore.Secret
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		var err error
		secret, err = r.AccessSecret(ctx, scope.project, flySpritesToken)
		if errors.Is(err, corestore.ErrNotFound) && scope.project != "" {
			secret, err = r.AccessSecret(ctx, "", flySpritesToken)
		}
		return err
	})
	if err != nil {
		return scope, "", hubsecrets.Envelope{}, err
	}
	if err := accessstore.VerifySecret(s.config.SecretKeys, secret); err != nil {
		return scope, "", hubsecrets.Envelope{}, err
	}
	scope.project = secret.ProjectID
	return scope, secret.OrganizationSlug, secret.Envelope, nil
}

func (s *Service) readSecretStatus(ctx context.Context, db nativeQueryer, scope nativeScope) (projectSecretStatus, error) {
	if s.config.AccessRepository != nil {
		return s.readAccessSecretStatus(ctx, scope)
	}
	return readSecretStatus(ctx, db, scope)
}
