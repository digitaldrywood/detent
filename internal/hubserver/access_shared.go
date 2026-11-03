package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) sharedAccessSession(ctx context.Context, c echo.Context, claims cloudassert.Claims) (auth.Session, string, error) {
	now := s.config.now()
	identity := auth.HostedIdentity{Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession, CreatedAt: claims.SessionCreatedAt, ExpiresAt: claims.SessionExpiresAt, SupportActor: claims.SupportActor, SupportReason: claims.SupportReason}
	if claims.Audience != string(s.accessOrganization()) || !claims.AccessExpiresAt.After(now) || !claims.SessionExpiresAt.After(now) || !auth.ValidOrganizationRole(claims.Role) || identity.SupportActor != "" && !auth.ValidSupportReason(identity.SupportReason) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	var row accessstore.Session
	missingSession := false
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if tenant.ProviderID != identity.OrganizationID {
			return auth.ErrInvalidSession
		}
		row, err = r.AccessSession(ctx, claims.Binding)
		missingSession = errors.Is(err, corestore.ErrNotFound)
		return err
	})
	if missingSession {
		row = accessstore.Session{OrganizationID: s.accessOrganization(), Hash: claims.Binding, Email: claims.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: now, ExpiresAt: identity.ExpiresAt}
		err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
			tenant, err := w.AccessTenant(ctx)
			if err != nil {
				return err
			}
			if tenant.ProviderID != identity.OrganizationID {
				return auth.ErrInvalidSession
			}
			if err := w.EnsureAccessSession(ctx, row); err != nil {
				return err
			}
			row, err = w.AccessSession(ctx, claims.Binding)
			return err
		})
	}
	session := auth.Session{Email: row.Email, ExpiresAt: row.ExpiresAt, Identity: &auth.HostedIdentity{Subject: row.Subject, OrganizationID: row.ProviderOrganizationID, SessionID: row.SessionID, CreatedAt: row.IdentityCreatedAt, ExpiresAt: row.IdentityExpiresAt, SupportActor: row.SupportActor, SupportReason: row.SupportReason}}
	if err != nil || row.RevokedAt != nil || !session.ExpiresAt.After(now) || !session.Identity.ExpiresAt.After(now) || !sharedSessionMatches(session, identity, claims.Email) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	c.Set("hosted_session", session)
	return session, claims.Binding, nil
}

func (s *Service) storeAccessMember(ctx context.Context, w tenantstore.Writer, identity auth.Identity, membership auth.Membership) error {
	if !identity.EmailVerified || identity.Subject != membership.UserID || membership.Status != "active" || !auth.ValidOrganizationRole(membership.Role.Slug) {
		return auth.ErrHostedIdentity
	}
	tenant, err := w.AccessTenant(ctx)
	if err != nil {
		return err
	}
	if membership.OrganizationID != tenant.ProviderID {
		return auth.ErrHostedIdentity
	}
	principal := "hosted_" + apikey.HashToken(identity.Subject)[:32]
	now := s.config.now()
	if _, err := w.AccessCredential(ctx, principal); errors.Is(err, corestore.ErrNotFound) {
		token, err := s.config.generateToken()
		if err != nil {
			return err
		}
		hash := apikey.HashToken(token)
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: s.accessOrganization(), ID: principal, Name: principal, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	member, err := w.AccessMember(ctx, identity.Subject)
	if errors.Is(err, corestore.ErrNotFound) {
		if err := w.CreateAccessMember(ctx, accessstore.Member{OrganizationID: s.accessOrganization(), UserID: identity.Subject, Email: strings.ToLower(identity.Email), MembershipID: membership.ID, Role: membership.Role.Slug, Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now, NotifyIssues: true}); err != nil {
			return err
		}
		if membership.Role.Slug == "owner" || membership.Role.Slug == "admin" {
			return w.SyncHostedAdministratorProjectAccess(ctx, identity.Subject, principal)
		}
		return nil
	}
	if err != nil {
		return err
	}
	roleChanged := member.Role != membership.Role.Slug
	if member.MembershipID != membership.ID || !member.Active || roleChanged {
		if err := w.ClearAccessMemberGrants(ctx, identity.Subject); err != nil {
			return err
		}
	}
	member.Email, member.MembershipID, member.Role, member.Active, member.UpdatedAt = strings.ToLower(identity.Email), membership.ID, membership.Role.Slug, true, now
	if err := w.UpdateAccessMember(ctx, member); err != nil {
		return err
	}
	if member.Role == "owner" || member.Role == "admin" {
		return w.SyncHostedAdministratorProjectAccess(ctx, identity.Subject, principal)
	}
	return nil
}

func (s *Service) entryAccessKeyCredential(ctx context.Context, claims cloudassert.Claims) (apiCredential, int, error) {
	if !s.hostedShared() || claims.Kind != cloudassert.KindKey || claims.Key == nil || !claims.Key.Valid() || !claims.ExpiresAt.After(s.config.now()) || claims.Audience != string(s.accessOrganization()) || !auth.ValidOrganizationRole(claims.Role) {
		return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
	}
	identity := &auth.HostedIdentity{Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, CreatedAt: claims.IssuedAt, ExpiresAt: claims.ExpiresAt, SessionID: claims.Key.ID}
	credential := apiCredential{EntryKey: claims.Key, Hosted: identity, HostedRole: claims.Role, HostedKeyScope: claims.Key.Permission, HostedProjectAccess: hostedProjectsAll, NativeOnly: true, Scope: apiScopeOperator}
	if claims.Key.Permission == apikey.ScopeAdmin {
		credential.Scope = apiScopeAdmin
	}
	err := s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
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
			token, err := s.config.generateToken()
			if err != nil {
				return err
			}
			now := s.config.now()
			hash := apikey.HashToken(token)
			if err := w.EnsureAccessCredential(ctx, accessstore.Credential{OrganizationID: s.accessOrganization(), ID: id, Name: "Organization service principal " + claims.Key.ID, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "operator", NativeOnly: true, ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
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
			credential.HostedRole = lesserHostedRole(member.Role, claims.Role)
		}
		if principal.RevokedAt != nil {
			return auth.ErrHostedIdentity
		}
		credential.ID, credential.Hash, credential.HostedPrincipal = principal.ID, principal.Hash, principal.ID
		return nil
	})
	if err != nil {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	return credential, http.StatusOK, nil
}
