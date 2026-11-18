package hubauth

import (
	"context"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Authenticator) EntryKeyCredential(ctx context.Context, claims cloudassert.Claims) (Credential, int, error) {
	if !s.shared() || claims.Kind != cloudassert.KindKey || claims.Key == nil || !claims.Key.Valid() || !claims.ExpiresAt.After(s.deps.Now()) || claims.Audience != string(s.deps.Organization) || !auth.ValidOrganizationRole(claims.Role) {
		return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
	}
	identity := &auth.HostedIdentity{Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, CreatedAt: claims.IssuedAt, ExpiresAt: claims.ExpiresAt, SessionID: claims.Key.ID}
	credential := Credential{EntryKey: claims.Key, Hosted: identity, HostedRole: claims.Role, HostedKeyScope: claims.Key.Permission, HostedProjectAccess: "all", NativeOnly: true, Scope: Operator}
	if claims.Key.Permission == apikey.ScopeAdmin {
		credential.Scope = Admin
	}
	if _, scoped := s.deps.BoardCredential(ctx); !scoped && claims.Key.Kind == "service" {
		if err := s.EnsureEntryServicePrincipal(ctx, claims); err != nil {
			err = s.deps.Classify(err)
			if !errors.Is(err, operatortool.ErrAccessDenied) {
				return Credential{}, http.StatusServiceUnavailable, err
			}
			return Credential{}, http.StatusForbidden, err
		}
	}
	err := s.read(ctx, s.deps.Organization, func(w tenantstore.Reader) error {
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if tenant.ProviderID != claims.ProviderOrganization {
			return auth.ErrHostedIdentity
		}
		var principal accessstore.Credential
		if claims.Key.Kind == "service" {
			id := "service_" + claims.Key.ID
			principal, err = w.AccessCredential(ctx, id)
			if err != nil {
				return err
			}
		} else {
			member, err := w.AccessMember(ctx, claims.Subject)
			if err != nil {
				return err
			}
			if !member.Active {
				return auth.ErrHostedIdentity
			}
			principal, err = w.AccessCredential(ctx, member.PrincipalID)
			if err != nil {
				return err
			}
			credential.HostedMembership = member.MembershipID
			credential.HostedRole = hubapi.LesserHostedRole(member.Role, claims.Role)
		}
		if principal.RevokedAt != nil {
			return auth.ErrHostedIdentity
		}
		credential.ID, credential.Hash, credential.HostedPrincipal = principal.ID, principal.Hash, principal.ID
		return nil
	})
	if err != nil {
		err = s.deps.Classify(err)
		if !errors.Is(err, operatortool.ErrAccessDenied) {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		return Credential{}, http.StatusForbidden, err
	}
	return credential, http.StatusOK, nil
}

func (s *Authenticator) EnsureEntryServicePrincipal(ctx context.Context, claims cloudassert.Claims) error {
	id := "service_" + claims.Key.ID
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		_, err := r.AccessCredential(ctx, id)
		return err
	})
	if !errors.Is(err, corestore.ErrNotFound) || !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
		return err
	}
	return s.write(ctx, s.deps.Organization, func(w tenantstore.Writer) error {
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if tenant.ProviderID != claims.ProviderOrganization {
			return auth.ErrHostedIdentity
		}
		token, err := s.deps.GenerateToken()
		if err != nil {
			return err
		}
		now := s.deps.Now()
		hash := apikey.HashToken(token)
		return w.EnsureAccessCredential(ctx, accessstore.Credential{OrganizationID: s.deps.Organization, ID: id, Name: "Organization service principal " + claims.Key.ID, Hash: hash, Fingerprint: fingerprint(hash), Scope: "operator", NativeOnly: true, ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
	})
}
