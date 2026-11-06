package cloudentry

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) resolveTenantKey(ctx context.Context, allocation Organization, origin entryauthority.Origin) (cloudassert.Claims, error) {
	if origin.Issuer != s.config.Issuer || origin.Generation != allocation.Generation || !safeID(origin.KeyID) || (origin.Subject == "" || len(origin.Subject) > 256) || len(origin.CredentialBinding) != 64 {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	snapshot, err := s.auth.store.db.postgres.TenantKey(ctx, origin.KeyID, tracker.OrganizationID(allocation.ID))
	if errors.Is(err, platformstore.ErrNoSession) {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	if err != nil {
		return cloudassert.Claims{}, err
	}
	organization, key := snapshot.Organization, snapshot.Key
	if organization.Generation != allocation.Generation || organization.Endpoint != allocation.Endpoint || organization.ProviderID != allocation.ProviderID || subtle.ConstantTimeCompare([]byte(snapshot.Authority.CredentialBinding), []byte(origin.CredentialBinding)) != 1 || oauthGrantID(key.ID) && snapshot.Resource != s.config.PublicURL+"/mcp" {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	role, subject := "admin", "service:"+key.ID
	if key.Kind == "personal" {
		member, err := s.keyMembership(ctx, key.OwnerSubject, Organization{ID: allocation.ID, ProviderID: organization.ProviderID})
		if err != nil {
			var refusal *apikey.Refusal
			if errors.As(err, &refusal) {
				return cloudassert.Claims{}, cloudassert.ErrInvalid
			}
			return cloudassert.Claims{}, err
		}
		role, subject = member.Role.Slug, key.OwnerSubject
	}
	if subject != origin.Subject {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	claims, err := s.claims(Organization{ID: allocation.ID, Slug: organization.Slug, Generation: organization.Generation}, cloudassert.KindKey, http.MethodPost, "/internal/v1/keys/call", nil)
	if err != nil {
		return cloudassert.Claims{}, err
	}
	claims.Key, claims.Subject, claims.Email, claims.ProviderOrganization, claims.Role = &snapshot.Authority, subject, key.OwnerEmail, organization.ProviderID, role
	if key.ExpiresAt != nil && key.ExpiresAt.Before(claims.ExpiresAt) {
		claims.ExpiresAt = *key.ExpiresAt
	}
	return claims, nil
}
