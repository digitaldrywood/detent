package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runnerIdentityRecord(ctx context.Context, reader tenantstore.Reader, record runnerstore.Identity) (runnerauth.Identity, error) {
	projects, err := reader.RunnerProjects(ctx, record.RunnerID, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return runnerauth.Identity{}, err
	}
	return runnerauth.Identity{
		Binding: runnerauth.Binding{RunnerID: record.RunnerID, MachineID: record.MachineID}, Scope: record.Scope,
		OrganizationID: record.OrganizationID, ProjectIDs: projects, Operations: append([]string(nil), record.Operations...), ExpiresAt: record.ExpiresAt,
	}, nil
}

func (s *Service) authenticateTenantAPIHash(ctx context.Context, organization tracker.OrganizationID, hash, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	if s.config.TenantRepository == nil || organization == "" {
		return apiCredential{}, http.StatusServiceUnavailable, nativeRunEventStorageUnavailable()
	}
	var token accessstore.Credential
	var member accessstore.Member
	var runner runnerauth.Identity
	var provider string
	var runnerRenewal bool
	var err error
	if authority, ok := s.config.TenantRepository.(tenantauth.Reader); ok {
		var scope tenantauth.Scope
		scope, err = authority.ReadTenantAuthority(ctx, tenantauth.RequestIdentity{Organization: organization, CredentialHash: hash, RenewalRunner: renewalRunner, RenewalOrganization: tracker.OrganizationID(renewalOrganization)})
		if err == nil {
			principal := scope.Principal()
			if scope.Organization() != organization || scope.ValidAt(s.config.now()) != nil {
				err = accessstore.ErrDenied
			} else {
				token = accessstore.Credential{ID: principal.CredentialID, Name: principal.Name, Scope: principal.Scope, NativeOnly: principal.NativeOnly, UserID: principal.UserID, MembershipID: principal.MembershipID, KeyScope: principal.KeyScope, ProjectAccess: principal.ProjectAccess, Kind: principal.Kind}
				member = accessstore.Member{PrincipalID: principal.PrincipalID, Role: principal.Role}
				runner, runnerRenewal, provider = scope.Runner(), scope.Renewal(), principal.ProviderID
			}
		}
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			token, err = reader.CredentialByHash(ctx, hash)
			if err != nil {
				return err
			}
			record, err := reader.RunnerIdentityByCredential(ctx, token.ID)
			if err == nil {
				if record.OrganizationID != organization || record.RemovedAt != nil || token.Scope != string(apiScopeWorker) || token.Kind != "service" {
					return accessstore.ErrDenied
				}
				now := s.config.now()
				runnerRenewal = renewalRunner != "" && renewalRunner == record.RunnerID && renewalOrganization == string(record.OrganizationID)
				if !validRunnerCredentialForRenewal(token, hash, now, runnerRenewal) || !record.ExpiresAt.After(now) && !runnerRenewal {
					return accessstore.ErrDenied
				}
				runner, err = runnerIdentityRecord(ctx, reader, record)
				return err
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			member, err = reader.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: token.ID, Hash: hash, UserID: token.UserID, MembershipID: token.MembershipID}, s.config.now())
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
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, corestore.ErrNotFound) {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	credential := apiCredential{ID: token.ID, Name: token.Name, Scope: apiScope(token.Scope), Hash: hash, NativeOnly: token.NativeOnly, HostedMembership: token.MembershipID, HostedPrincipal: member.PrincipalID, HostedRole: member.Role, HostedKeyKind: token.Kind, HostedProjectAccess: hostedProjectAccess(token.ProjectAccess), Runner: runner, runnerRenewal: runnerRenewal}
	if token.UserID != "" {
		if s.config.Hosted == nil || provider == "" {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
		credential.Hosted = &auth.HostedIdentity{Subject: token.UserID, OrganizationID: provider}
		current, err := s.hostedMembership(ctx, credential.Hosted)
		if errors.Is(err, tenantauth.ErrUnavailable) {
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

func (s *Service) readRunnerIdentityTenant(ctx context.Context, organization tracker.OrganizationID, runnerID string) (runnerauth.Identity, error) {
	if s.config.TenantRepository == nil {
		return runnerauth.Identity{}, nativeRunEventStorageUnavailable()
	}
	var identity runnerauth.Identity
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		record, err := reader.RunnerIdentity(ctx, runnerID)
		if err != nil {
			return err
		}
		if record.OrganizationID != organization || record.RemovedAt != nil {
			return corestore.ErrNotFound
		}
		identity, err = runnerIdentityRecord(ctx, reader, record)
		return err
	})
	return identity, err
}

func validRunnerCredentialForRenewal(token accessstore.Credential, hash string, now time.Time, allowExpired bool) bool {
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
