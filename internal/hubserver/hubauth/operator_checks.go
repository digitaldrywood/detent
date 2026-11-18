package hubauth

import (
	"context"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Authenticator) CurrentAuthority(ctx context.Context, credential Credential, organization string, options AuthorityOptions) (operatortool.Authority, error) {
	if organization == "" || credential.Runner.RunnerID != "" || credential.Scope != Operator && credential.Scope != Admin || s.deps.Hosted != nil && credential.Hosted == nil {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	if credential.HostedRole == "account" {
		if s.deps.Hosted == nil || s.shared() || organization != s.deps.Hosted.OrganizationID {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		return operatortool.Authority{Identity: Identity(credential, organization), Account: operatortool.Account{Subject: credential.Hosted.Subject, Role: "account"}, Check: func(_ context.Context, r operatortool.Requirement) error {
			if r.ProjectID != "" || r.ResourceID != "" || r.ResourceKind != "" {
				return operatortool.ErrAccessDenied
			}
			return nil
		}}, nil
	}

	scope := Input{Organization: tracker.OrganizationID(organization), Credential: credential}
	if err := s.AuthorizeOrganization(ctx, scope); err != nil {
		return operatortool.Authority{}, options.Classify(err)
	}
	account := operatortool.Account{}
	if credential.Hosted != nil {
		account = operatortool.Account{Subject: credential.Hosted.Subject, Role: credential.HostedRole, SupportActor: credential.Hosted.SupportActor}
	}
	result := options.Base(scope)
	result.Account, result.Identity = account, Identity(credential, organization)
	result.Check = func(ctx context.Context, requirement operatortool.Requirement) (err error) {
		defer func() { err = options.Classify(err) }()
		ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
		defer cancel()
		if credential.HostedRole == "reporter" && !RoleAllows(credential.HostedRole, requirement.Scope, requirement.Action) {
			return operatortool.ErrAccessDenied
		}
		if requirement.ResourceKind == "credentials" && s.deps.Hosted != nil {
			if requirement.ProjectID != "" || requirement.ResourceID != "" {
				return operatortool.ErrAccessDenied
			}
			_, err := options.KeyCredential(ctx, credential)
			return err
		}
		if requirement.ResourceKind == "credentials" {
			requirement.ResourceKind = ""
		}
		if credential.HostedKeyScope != "" && !KeyAllows(credential.HostedKeyScope, requirement.Scope) {
			return operatortool.ErrAccessDenied
		}
		if requirement.OrganizationWide && credential.Hosted == nil && credential.NativeOnly {
			return operatortool.ErrAccessDenied
		}
		if requirement.ResourceKind == "runners" {
			if credential.NativeOnly && credential.Hosted == nil || credential.Runner.RunnerID != "" {
				return operatortool.ErrAccessDenied
			}
			if credential.Hosted != nil {
				return options.RunnerAdministration(ctx, credential)
			}
			if credential.Scope != Admin {
				return operatortool.ErrAccessDenied
			}
			return nil
		}
		if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
			if credential.Hosted == nil || requirement.ResourceID != "" || s.deps.Hosted == nil || organization != s.deps.Hosted.OrganizationID {
				return operatortool.ErrAccessDenied
			}
			if requirement.ResourceKind == "billing" && (credential.HostedRole != "owner" || credential.Hosted.SupportActor != "") {
				return operatortool.ErrAccessDenied
			}
			if requirement.ResourceKind == "plan" && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return operatortool.ErrAccessDenied
			}
		}
		checkScope := scope
		checkScope.Action = requirement.Action
		if requirement.ProjectID != "" {
			checkScope.Project = tracker.ProjectID(requirement.ProjectID)
			if err := s.AuthorizeProject(ctx, checkScope, requirement.Scope != apikey.ScopeRead); err != nil {
				return err
			}
			if err := s.RequireActiveProject(ctx, checkScope); err != nil {
				return err
			}
		}
		if requirement.Scope != apikey.ScopeRead {
			if credential.Hosted != nil {
				if !RoleAllows(credential.HostedRole, requirement.Scope, requirement.Action) {
					return operatortool.ErrAccessDenied
				}
				if requirement.ProjectID != "" {
					return options.Resource(ctx, checkScope, requirement)
				}
				checkScope.Admin = requirement.Scope == apikey.ScopeAdmin
				if operatortool.IsThreadTool(requirement.Action) {
					return s.AuthorizeProject(ctx, checkScope, true)
				}
				if err := options.RecheckMutation(ctx, checkScope); err != nil {
					return err
				}
				if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
					return nil
				}
				return options.Resource(ctx, checkScope, requirement)
			}
			if requirement.Scope == apikey.ScopeAdmin && credential.Scope != Admin {
				return operatortool.ErrAccessDenied
			}
		}
		if requirement.ResourceKind == "billing" || requirement.ResourceKind == "plan" {
			return nil
		}
		return options.Resource(ctx, checkScope, requirement)
	}
	return result, nil
}
