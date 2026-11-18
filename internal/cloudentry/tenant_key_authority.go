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
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) resolveTenantKey(ctx context.Context, allocation Organization, origin entryauthority.Origin) (result cloudassert.Claims, resultErr error) {
	defer s.auditKeyRefusal(ctx, allocation, origin.KeyID, origin.Subject, &resultErr)
	if origin.Issuer != s.config.Issuer || origin.Generation != allocation.Generation || !safeID(origin.KeyID) || (origin.Subject == "" || len(origin.Subject) > 256) || len(origin.CredentialBinding) != 64 {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	snapshot, err := s.auth.store.db.postgres.PlatformKey(ctx, tenantauth.PlatformKeyRequest{KeyID: origin.KeyID, Organization: tracker.OrganizationID(allocation.ID), Managed: s.sharedHub == nil})
	if errors.Is(err, platformstore.ErrNoSession) {
		return cloudassert.Claims{}, errors.Join(cloudassert.ErrInvalid, err)
	}
	if err != nil {
		return cloudassert.Claims{}, err
	}
	key := snapshot.Authority
	if snapshot.Generation != allocation.Generation || snapshot.Endpoint != allocation.Endpoint || snapshot.ProviderID != allocation.ProviderID || subtle.ConstantTimeCompare([]byte(snapshot.Authority.CredentialBinding), []byte(origin.CredentialBinding)) != 1 || oauthGrantID(key.ID) && snapshot.Resource != s.config.PublicURL+"/mcp" {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	role, subject := "admin", "service:"+key.ID
	if key.Kind == "personal" {
		member, err := s.keyMembership(ctx, snapshot.OwnerSubject, Organization{ID: allocation.ID, ProviderID: snapshot.ProviderID})
		if err != nil {
			var refusal *apikey.Refusal
			if errors.As(err, &refusal) {
				return cloudassert.Claims{}, errors.Join(cloudassert.ErrInvalid, err)
			}
			return cloudassert.Claims{}, err
		}
		role, subject = member.Role.Slug, snapshot.OwnerSubject
	}
	if subject != origin.Subject {
		return cloudassert.Claims{}, cloudassert.ErrInvalid
	}
	claims, err := s.claims(Organization{ID: allocation.ID, Slug: snapshot.Slug, Generation: snapshot.Generation}, cloudassert.KindKey, http.MethodPost, "/internal/v1/keys/call", nil)
	if err != nil {
		return cloudassert.Claims{}, err
	}
	claims.Key, claims.Subject, claims.Email, claims.ProviderOrganization, claims.Role = &snapshot.Authority, subject, snapshot.OwnerEmail, snapshot.ProviderID, role
	if snapshot.ExpiresAt != nil && snapshot.ExpiresAt.Before(claims.ExpiresAt) {
		claims.ExpiresAt = *snapshot.ExpiresAt
	}
	return claims, nil
}
