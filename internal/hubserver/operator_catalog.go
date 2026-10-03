package hubserver

import (
	"context"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type operatorCatalogKey struct{}

type operatorCatalogAuthority struct {
	identity           operatortool.Identity
	credential         apiCredential
	runners            bool
	support            bool
	createOrganization bool
}

func (s *Service) withOperatorCatalog(ctx context.Context, credential apiCredential, organization string) context.Context {
	catalog := operatorCatalogAuthority{identity: operatorIdentity(credential, organization), credential: credential}
	if credential.Hosted != nil && credential.HostedRole != "account" {
		catalog.runners = credential.HostedRole != "viewer" && s.hostedAllRunnerGrants(ctx, credential)
	}
	if s.config.Hosted != nil && !s.hostedShared() && credential.Hosted != nil && credential.SessionHash != "" && credential.Hosted.SupportActor == "" {
		if session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now()); err == nil {
			catalog.support = hostedEmailListed(s.config.Hosted.SupportActors, session.Email)
			if credential.Hosted.Subject == s.config.Hosted.BootstrapSubject && !hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
				var count int
				catalog.createOrganization = s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_members").Scan(&count) == nil && count == 0
			}
		}
	}
	return context.WithValue(ctx, operatorCatalogKey{}, catalog)
}

func operatorCatalog(ctx context.Context) (operatorCatalogAuthority, error) {
	catalog, ok := ctx.Value(operatorCatalogKey{}).(operatorCatalogAuthority)
	if !ok || !catalog.identity.Valid() || catalog.identity != operatortool.ConnectionIdentity(ctx) {
		return operatorCatalogAuthority{}, operatortool.ErrAccessDenied
	}
	return catalog, nil
}

func (s *Service) catalogBillingCredential(ctx context.Context, kind string, write bool) (apiCredential, error) {
	scope := apikey.ScopeRead
	if write {
		scope = apikey.ScopeWrite
	}
	if _, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: scope, ResourceKind: kind}); err != nil {
		return apiCredential{}, err
	}
	catalog, err := operatorCatalog(ctx)
	if err != nil || s.config.Hosted == nil || catalog.credential.Hosted == nil {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	return catalog.credential, nil
}

func (s *Service) authorizeCatalog(ctx context.Context, requirement operatortool.Requirement) (context.Context, error) {
	catalog, err := operatorCatalog(ctx)
	if err != nil || !apikey.ValidScope(requirement.Scope) || requirement.ProjectID != "" || requirement.ResourceID != "" || requirement.OrganizationID != "" && requirement.OrganizationID != catalog.identity.OrganizationID {
		return ctx, operatortool.ErrAccessDenied
	}
	credential := catalog.credential
	if credential.HostedRole == "account" {
		if requirement.ResourceKind != "" {
			return ctx, operatortool.ErrAccessDenied
		}
		return ctx, nil
	}
	if requirement.ResourceKind == "credentials" && s.config.Hosted != nil {
		if credential.Hosted == nil || credential.SessionHash == "" || credential.HostedKeyScope != "" || credential.Hosted.SupportActor != "" {
			return ctx, operatortool.ErrAccessDenied
		}
		return ctx, nil
	}
	if credential.HostedKeyScope != "" && !hostedKeyAllows(credential.HostedKeyScope, requirement.Scope) || requirement.OrganizationWide && credential.Hosted == nil && credential.NativeOnly {
		return ctx, operatortool.ErrAccessDenied
	}
	if requirement.ResourceKind == "runners" {
		if credential.Hosted != nil && !catalog.runners || credential.Hosted == nil && (credential.NativeOnly || credential.Scope != apiScopeAdmin) {
			return ctx, operatortool.ErrAccessDenied
		}
		return ctx, nil
	}
	if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
		if credential.Hosted == nil || s.config.Hosted == nil || catalog.identity.OrganizationID != s.config.Hosted.OrganizationID || requirement.ResourceKind == "billing" && (credential.HostedRole != "owner" || credential.Hosted.SupportActor != "") || requirement.ResourceKind == "plan" && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
			return ctx, operatortool.ErrAccessDenied
		}
	}
	if requirement.Scope != apikey.ScopeRead {
		projectResource := requirement.ResourceKind == "work_item" || requirement.ResourceKind == "workspace" || requirement.ResourceKind == "project"
		if credential.Hosted != nil && (!hostedRoleAllows(credential.HostedRole, requirement.Scope) || !projectResource && credential.HostedRole != "owner" && credential.HostedRole != "admin") || credential.Hosted == nil && requirement.Scope == apikey.ScopeAdmin && credential.Scope != apiScopeAdmin {
			return ctx, operatortool.ErrAccessDenied
		}
	}
	return ctx, nil
}

func (s *Service) administrationCatalog(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		return nil, err
	}
	credential := catalog.credential
	definitions := []operatortool.Definition{}
	for _, d := range operatortool.AdministrationCatalog() {
		if !slices.Contains(s.administration.Names, d.Name) {
			continue
		}
		if _, err := s.authorizeCatalog(ctx, operatortool.AdministrationRequirement(d.Name)); err != nil {
			continue
		}
		var allowed bool
		switch d.Name {
		case operatortool.OrganizationSession:
			allowed = true
		case operatortool.OrganizationList:
			allowed = s.config.Hosted != nil || credential.NativeOnly || credential.Scope == apiScopeAdmin
		case operatortool.OrganizationSwitch:
			allowed = s.config.Hosted == nil || !s.hostedShared() && credential.Hosted.SupportActor == "" && credential.HostedKeyScope == ""
		case operatortool.SessionLogout:
			allowed = s.config.Hosted != nil && !s.hostedShared() && credential.SessionHash != ""
		case operatortool.SupportStart:
			allowed = catalog.support
		case operatortool.OrganizationCreate:
			allowed = catalog.createOrganization
		case operatortool.InvitationAccept:
			allowed = s.config.Hosted != nil && !s.hostedShared() && credential.Hosted.SupportActor == "" && credential.HostedKeyScope == ""
		default:
			if s.config.Hosted == nil {
				allowed = d.Name == operatortool.CredentialList && credential.Scope == apiScopeAdmin && !credential.NativeOnly
			} else {
				allowed = strings.HasPrefix(d.Name, "credential_") || d.Name == operatortool.MembershipList || credential.HostedRole == "owner" || credential.HostedRole == "admin"
			}
		}
		if allowed {
			definitions = append(definitions, d)
		}
	}
	for _, name := range []string{operatortool.ActionResult, operatortool.ConnectionInfo} {
		definition, _ := operatortool.Lookup(name)
		definitions = append(definitions, definition)
	}
	return definitions, nil
}
