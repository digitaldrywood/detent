package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) accessRead(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	return s.config.AccessRepository.ReadTenant(ctx, organization, corestore.Serving, read)
}

func (s *Service) accessWrite(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	return s.config.AccessRepository.WriteTenant(ctx, organization, corestore.Serving, write)
}

func (s *Service) accessOrganization() tracker.OrganizationID {
	if s.config.Hosted == nil {
		return ""
	}
	return tracker.OrganizationID(s.config.Hosted.OrganizationID)
}

func (s *Service) accessAuthority(scope nativeScope, write bool) accessstore.Authority {
	authority := accessstore.Authority{CredentialID: scope.credential.ID, Hash: scope.credential.Hash, SessionHash: scope.credential.SessionHash, ProjectID: scope.project, Write: write, Admin: scope.requireHostedAdmin, Action: scope.action, Role: scope.credential.HostedRole}
	if scope.credential.Hosted != nil && (scope.credential.EntryKey == nil || scope.credential.EntryKey.Kind != "service") {
		authority.UserID = scope.credential.Hosted.Subject
		authority.MembershipID = scope.credential.HostedMembership
	}
	return authority
}

func (s *Service) authorizeAccessScope(ctx context.Context, r accessstore.Reader, scope nativeScope, write bool) error {
	if scope.integrationRepository != "" {
		reader, ok := r.(projectstore.Reader)
		if !ok {
			return accessstore.ErrDenied
		}
		project, err := reader.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !strings.EqualFold(project.Settings.Repository, scope.integrationRepository) && !strings.EqualFold(project.Settings.CheckoutRepository, scope.integrationRepository) {
			return accessstore.ErrDenied
		}
		return nil
	}
	if s.config.Hosted != nil && scope.organization != s.accessOrganization() {
		return auth.ErrHostedIdentity
	}
	if scope.credential.EntryKey != nil {
		if scope.project != "" && !scope.credential.EntryKey.Allows(string(scope.project)) {
			return auth.ErrHostedIdentity
		}
		required := apikey.ScopeRead
		if write {
			required = apikey.ScopeWrite
		}
		if scope.requireHostedAdmin {
			required = apikey.ScopeAdmin
		}
		if !hostedKeyAllows(scope.credential.HostedKeyScope, required) || !hostedRoleAllows(scope.credential.HostedRole, required, scope.action) {
			return auth.ErrHostedIdentity
		}
	}
	member, err := r.AuthorizeAccess(ctx, s.accessAuthority(scope, write), s.config.now())
	if err != nil {
		return err
	}
	if scope.credential.Hosted != nil && member.UserID != "" {
		role := lesserHostedRole(member.Role, scope.credential.HostedRole)
		if scope.requireHostedAdmin && role != "owner" && role != "admin" {
			return auth.ErrHostedIdentity
		}
	}
	return nil
}

func (s *Service) accessWebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	var row accessstore.Session
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error { var err error; row, err = r.AccessSession(ctx, hash); return err })
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return auth.Session{}, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	if err != nil {
		return auth.Session{}, fmt.Errorf("read hosted session: %w", err)
	}
	if row.RevokedAt != nil || !row.ExpiresAt.After(now) || !row.IdentityExpiresAt.After(now) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	identity := &auth.HostedIdentity{Subject: row.Subject, OrganizationID: row.ProviderOrganizationID, SessionID: row.SessionID, CreatedAt: row.IdentityCreatedAt, ExpiresAt: row.IdentityExpiresAt, SupportActor: row.SupportActor, SupportReason: row.SupportReason}
	return auth.Session{Email: row.Email, Identity: identity, ExpiresAt: row.ExpiresAt}, nil
}

func (s *Service) createAccessWebSession(ctx context.Context, record auth.SessionRecord) error {
	if s.config.Hosted == nil || record.Identity == nil {
		return auth.ErrInvalidSession
	}
	identity := record.Identity
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		tenant, err := w.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if identity.OrganizationID != tenant.ProviderID {
			return auth.ErrInvalidSession
		}
		return w.CreateAccessSession(ctx, accessstore.Session{OrganizationID: s.accessOrganization(), Hash: record.TokenHash, Email: record.Email, Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt})
	})
}

func (s *Service) accessMemberCredential(ctx context.Context, session auth.Session, hash string, membership auth.Membership) (apiCredential, int, error) {
	if session.Identity == nil || !auth.ValidOrganizationRole(membership.Role.Slug) {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	credential := apiCredential{Scope: apiScopeOperator, NativeOnly: true, Hosted: session.Identity, SessionHash: hash, HostedRole: membership.Role.Slug, HostedMembership: membership.ID}
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		tenant, err := r.AccessTenant(ctx)
		if err != nil {
			return err
		}
		if tenant.ProviderID != session.Identity.OrganizationID {
			return auth.ErrHostedIdentity
		}
		member, err := r.AccessMember(ctx, session.Identity.Subject)
		if err != nil {
			return err
		}
		if !member.Active || member.MembershipID != membership.ID || !auth.ValidOrganizationRole(member.Role) {
			return auth.ErrHostedIdentity
		}
		principal, err := r.AccessCredential(ctx, member.PrincipalID)
		if err != nil {
			return err
		}
		if principal.RevokedAt != nil {
			return auth.ErrHostedIdentity
		}
		credential.ID, credential.Hash, credential.HostedPrincipal = principal.ID, principal.Hash, principal.ID
		credential.HostedRole = lesserHostedRole(credential.HostedRole, member.Role)
		return nil
	})
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	if err != nil {
		return apiCredential{}, http.StatusInternalServerError, err
	}
	return credential, http.StatusOK, nil
}

func (s *Service) authenticateAccessHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	var token accessstore.Credential
	var member accessstore.Member
	var runner runnerauth.Identity
	var provider string
	var runnerRenewal bool
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		token, err = r.CredentialByHash(ctx, hash)
		if err != nil {
			return err
		}
		record, identityErr := r.RunnerIdentityByCredential(ctx, token.ID)
		if identityErr == nil {
			if record.OrganizationID != s.accessOrganization() || record.RemovedAt != nil || token.Scope != string(apiScopeWorker) || token.Kind != "service" {
				return accessstore.ErrDenied
			}
			now := s.config.now()
			runnerRenewal = renewalRunner == record.RunnerID && renewalOrganization == string(record.OrganizationID)
			if !validRunnerCredentialForRenewal(token, hash, now, runnerRenewal) || !record.ExpiresAt.After(now) && !runnerRenewal {
				return accessstore.ErrDenied
			}
			runner, err = runnerIdentityRecord(ctx, r, record)
			if err != nil {
				return err
			}
		} else if !errors.Is(identityErr, corestore.ErrNotFound) {
			return identityErr
		} else {
			member, err = r.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: token.ID, Hash: hash, UserID: token.UserID, MembershipID: token.MembershipID}, s.config.now())
			if err != nil {
				return err
			}
		}
		tenant, err := r.AccessTenant(ctx)
		provider = tenant.ProviderID
		return err
	})
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, corestore.ErrNotFound) {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	credential := apiCredential{ID: token.ID, Name: token.Name, Scope: apiScope(token.Scope), Hash: hash, NativeOnly: token.NativeOnly, HostedMembership: token.MembershipID, HostedPrincipal: member.PrincipalID, HostedRole: member.Role, HostedKeyKind: token.Kind, HostedProjectAccess: hostedProjectAccess(token.ProjectAccess), Runner: runner, runnerRenewal: runnerRenewal}
	if token.UserID != "" {
		credential.Hosted = &auth.HostedIdentity{Subject: token.UserID, OrganizationID: provider}
		current, err := s.hostedMembership(ctx, credential.Hosted)
		if err != nil && errors.Is(err, tenantauth.ErrUnavailable) {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		if err != nil || current.ID != token.MembershipID {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		credential.HostedRole = lesserHostedRole(current.Role.Slug, member.Role)
		credential.HostedKeyScope = apikey.Scope(token.KeyScope)
	}
	return credential, http.StatusOK, nil
}

func (s *Service) activeAccessMember(ctx context.Context, user string) (accessstore.Member, error) {
	var member accessstore.Member
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, user)
		if err != nil {
			return err
		}
		principal, err := r.AccessCredential(ctx, member.PrincipalID)
		if err != nil {
			return err
		}
		_, err = r.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: principal.ID, Hash: principal.Hash, UserID: user, MembershipID: member.MembershipID}, s.config.now())
		return err
	})
	return member, err
}
