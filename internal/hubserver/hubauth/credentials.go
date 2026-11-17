package hubauth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Authenticator) AuthenticateAccessHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (Credential, int, error) {
	_, authorityAvailable := s.deps.AccessRepository.(tenantauth.Reader)
	_, runnerAuthorityAvailable := s.deps.AccessRepository.(runnerAuthorityReader)
	if authorityAvailable || runnerAuthorityAvailable {
		credential, status, err := s.AuthenticateRepositoryAPIHash(ctx, s.deps.AccessRepository, s.deps.Organization, hash, renewalRunner, renewalOrganization)
		if err == nil && status == http.StatusOK && s.deps.Hosted == nil && (credential.Hosted != nil || credential.HostedMembership != "") {
			return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		return credential, status, err
	}
	var token accessstore.Credential
	var member accessstore.Member
	var runner runnerauth.Identity
	var provider string
	var RunnerRenewal bool
	err := s.read(ctx, s.deps.Organization, func(r tenantstore.Reader) error {
		var err error
		token, err = r.CredentialByHash(ctx, hash)
		if err != nil {
			return err
		}
		if s.deps.Hosted == nil && (token.UserID != "" || token.MembershipID != "") {
			return accessstore.ErrDenied
		}
		record, identityErr := r.RunnerIdentityByCredential(ctx, token.ID)
		if identityErr == nil {
			if record.OrganizationID != s.deps.Organization || record.RemovedAt != nil || token.Scope != string(Worker) || token.Kind != "service" {
				return accessstore.ErrDenied
			}
			now := s.deps.Now()
			RunnerRenewal = renewalRunner == record.RunnerID && renewalOrganization == string(record.OrganizationID)
			if !ValidRunnerCredentialForRenewal(token, hash, now, RunnerRenewal) || !record.ExpiresAt.After(now) && !RunnerRenewal {
				return accessstore.ErrDenied
			}
			runner, err = RunnerIdentityRecord(ctx, r, record)
			if err != nil {
				return err
			}
		} else if !errors.Is(identityErr, corestore.ErrNotFound) {
			return identityErr
		} else {
			member, err = r.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: token.ID, Hash: hash, UserID: token.UserID, MembershipID: token.MembershipID}, s.deps.Now())
			if err != nil {
				return err
			}
		}
		if s.deps.Hosted == nil {
			return nil
		}
		tenant, err := r.AccessTenant(ctx)
		provider = tenant.ProviderID
		return err
	})
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, corestore.ErrNotFound) {
			return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		return Credential{}, http.StatusServiceUnavailable, err
	}
	credential := Credential{ID: token.ID, Name: token.Name, Scope: TokenScope(token.Scope), Hash: hash, NativeOnly: token.NativeOnly, HostedMembership: token.MembershipID, HostedPrincipal: member.PrincipalID, HostedRole: member.Role, HostedKeyKind: token.Kind, HostedProjectAccess: token.ProjectAccess, Runner: runner, RunnerRenewal: RunnerRenewal}
	if token.UserID != "" {
		credential.Hosted = &auth.HostedIdentity{Subject: token.UserID, OrganizationID: provider}
		current, err := s.Membership(ctx, credential.Hosted)
		if err != nil && errors.Is(err, tenantauth.ErrUnavailable) {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		if err != nil || current.ID != token.MembershipID {
			return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		credential.HostedRole = hubapi.LesserHostedRole(current.Role.Slug, member.Role)
		credential.HostedKeyScope = apikey.Scope(token.KeyScope)
	}
	return credential, http.StatusOK, nil
}

func (s *Authenticator) AuthenticateRepositoryAPIHash(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID, hash, renewalRunner, renewalOrganization string) (Credential, int, error) {
	if repository == nil || organization == "" {
		return Credential{}, http.StatusServiceUnavailable, hubapi.RunEventStorageUnavailable()
	}
	var token accessstore.Credential
	var member accessstore.Member
	var runner runnerauth.Identity
	var provider string
	var RunnerRenewal bool
	var err error
	authority, authorityAvailable := repository.(tenantauth.Reader)
	runnerAuthority, runnerAuthorityAvailable := repository.(runnerAuthorityReader)
	var RequestCapacity *int
	if authorityAvailable || runnerAuthorityAvailable {
		var scope tenantauth.Scope
		input := tenantauth.RequestIdentity{Local: s.deps.Hosted == nil, Organization: organization, CredentialHash: hash, RenewalRunner: renewalRunner, RenewalOrganization: tracker.OrganizationID(renewalOrganization)}
		if authorityAvailable {
			scope, err = authority.ReadTenantAuthority(ctx, input)
		} else {
			var capacity int
			scope, capacity, err = runnerAuthority.ReadRunnerAuthority(ctx, input, s.deps.Now)
			RequestCapacity = &capacity
		}
		if err == nil {
			principal := scope.Principal()
			if scope.Organization() != organization || scope.ValidAt(s.deps.Now()) != nil {
				err = accessstore.ErrDenied
			} else {
				token = accessstore.Credential{ID: principal.CredentialID, Name: principal.Name, Scope: principal.Scope, NativeOnly: principal.NativeOnly, UserID: principal.UserID, MembershipID: principal.MembershipID, KeyScope: principal.KeyScope, ProjectAccess: principal.ProjectAccess, Kind: principal.Kind}
				member = accessstore.Member{PrincipalID: principal.PrincipalID, Role: principal.Role}
				runner, RunnerRenewal, provider = scope.Runner(), scope.Renewal(), principal.ProviderID
			}
		}
	} else {
		err = repository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			token, err = reader.CredentialByHash(ctx, hash)
			if err != nil {
				return err
			}
			record, err := reader.RunnerIdentityByCredential(ctx, token.ID)
			if err == nil {
				if record.OrganizationID != organization || record.RemovedAt != nil || token.Scope != string(Worker) || token.Kind != "service" {
					return accessstore.ErrDenied
				}
				now := s.deps.Now()
				RunnerRenewal = renewalRunner != "" && renewalRunner == record.RunnerID && renewalOrganization == string(record.OrganizationID)
				if !ValidRunnerCredentialForRenewal(token, hash, now, RunnerRenewal) || !record.ExpiresAt.After(now) && !RunnerRenewal {
					return accessstore.ErrDenied
				}
				runner, err = RunnerIdentityRecord(ctx, reader, record)
				return err
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			member, err = reader.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: token.ID, Hash: hash, UserID: token.UserID, MembershipID: token.MembershipID}, s.deps.Now())
			if err != nil {
				return err
			}
			tenant, err := reader.AccessTenant(ctx)
			if err != nil {
				return err
			}
			provider = tenant.ProviderID
			return nil
		})
	}
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, corestore.ErrNotFound) {
			return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		return Credential{}, http.StatusServiceUnavailable, err
	}
	credential := Credential{ID: token.ID, Name: token.Name, Scope: TokenScope(token.Scope), Hash: hash, NativeOnly: token.NativeOnly, HostedMembership: token.MembershipID, HostedPrincipal: member.PrincipalID, HostedRole: member.Role, HostedKeyKind: token.Kind, HostedProjectAccess: token.ProjectAccess, Runner: runner, RunnerRenewal: RunnerRenewal, RequestCapacity: RequestCapacity}
	if token.UserID != "" {
		if s.deps.Hosted == nil || provider == "" {
			return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		credential.Hosted = &auth.HostedIdentity{Subject: token.UserID, OrganizationID: provider}
		current, err := s.Membership(ctx, credential.Hosted)
		if errors.Is(err, tenantauth.ErrUnavailable) {
			return Credential{}, http.StatusServiceUnavailable, err
		}
		if err != nil || current.ID != token.MembershipID {
			return Credential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		credential.HostedRole = hubapi.LesserHostedRole(current.Role.Slug, member.Role)
		credential.HostedKeyScope = apikey.Scope(token.KeyScope)
	}
	return credential, http.StatusOK, nil
}

func RunnerIdentityRecord(ctx context.Context, reader tenantstore.Reader, record runnerstore.Identity) (runnerauth.Identity, error) {
	projects, err := reader.RunnerProjects(ctx, record.RunnerID, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return runnerauth.Identity{}, err
	}
	return runnerauth.Identity{
		Binding: runnerauth.Binding{RunnerID: record.RunnerID, MachineID: record.MachineID}, Scope: record.Scope,
		OrganizationID: record.OrganizationID, ProjectIDs: projects, Operations: append([]string(nil), record.Operations...), ExpiresAt: record.ExpiresAt,
	}, nil
}

func ValidRunnerCredentialForRenewal(token accessstore.Credential, hash string, now time.Time, allowExpired bool) bool {
	if token.Valid(hash, now) {
		return true
	}
	if !allowExpired || token.ExpiresAt == nil {
		return false
	}
	copy := token
	copy.ExpiresAt = nil
	return copy.Valid(hash, now)
}
