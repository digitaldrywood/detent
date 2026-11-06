package cloudentry

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func (s *Service) resolveTenantKey(ctx context.Context, allocation Organization, origin entryauthority.Origin) (cloudassert.Claims, error) {
	if origin.Issuer != s.config.Issuer || origin.Generation != allocation.Generation || !safeID(origin.KeyID) || (origin.Subject == "" || len(origin.Subject) > 256) || len(origin.CredentialBinding) != 64 {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	organization, err := s.registry.Organization(ctx, allocation.ID)
	if err != nil || !organization.Managed || organization.State != "ready" || organization.Generation != allocation.Generation || organization.Endpoint != allocation.Endpoint || organization.ProviderID != allocation.ProviderID {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	key, err := s.auth.accessKey(ctx, "id", origin.KeyID)
	if oauthGrantID(origin.KeyID) {
		key, err = s.oauthKey(ctx, "id", origin.KeyID, true)
	}
	if err != nil || !key.active(s.config.now()) || subtle.ConstantTimeCompare([]byte(key.credentialBinding()), []byte(origin.CredentialBinding)) != 1 {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	claims, err := s.keyClaims(ctx, key, organization, http.MethodPost, "/internal/v1/keys/call", nil)
	if err != nil || claims.Subject != origin.Subject {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	return claims, nil
}
