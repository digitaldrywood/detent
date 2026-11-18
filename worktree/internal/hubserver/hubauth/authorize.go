package hubauth

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func (s *Authenticator) Authorize(ctx context.Context, r accessstore.Reader, scope Input, write bool) error {
	if scope.IntegrationRepository != "" {
		reader, ok := r.(projectstore.Reader)
		if !ok {
			return accessstore.ErrDenied
		}
		project, err := reader.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !strings.EqualFold(project.Settings.Repository, scope.IntegrationRepository) && !strings.EqualFold(project.Settings.CheckoutRepository, scope.IntegrationRepository) {
			return accessstore.ErrDenied
		}
		if scope.IntegrationFence != nil {
			if err := scope.IntegrationFence(ctx, r); err != nil {
				return err
			}
		}

		if scope.IntegrationOwned {
			return nil
		}
	}
	if s.deps.Hosted != nil && scope.Organization != s.deps.Organization {
		return hubapi.NotFound()
	}
	if scope.Credential.EntryKey != nil {
		if scope.Project != "" && !scope.Credential.EntryKey.Allows(string(scope.Project)) {
			return hubapi.NotFound()
		}
		required := apikey.ScopeRead
		if write {
			required = apikey.ScopeWrite
		}
		if scope.Admin {
			required = apikey.ScopeAdmin
		}
		if !KeyAllows(scope.Credential.HostedKeyScope, required) || !RoleAllows(scope.Credential.HostedRole, required, scope.Action) {
			return s.HiddenDenial(ctx, r, scope, write, auth.ErrHostedIdentity)
		}
	}
	member, err := r.AuthorizeAccess(ctx, Authority(scope, write), s.deps.Now())
	if err != nil {
		return s.HiddenDenial(ctx, r, scope, write, err)
	}
	if scope.Credential.Hosted != nil && member.UserID != "" {
		role := hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole)
		if scope.Admin && role != "owner" && role != "admin" {
			return auth.ErrHostedIdentity
		}
	}
	return nil
}

func Authority(scope Input, write bool) accessstore.Authority {
	authority := accessstore.Authority{CredentialID: scope.Credential.ID, Hash: scope.Credential.Hash, SessionHash: scope.Credential.SessionHash, ProjectID: scope.Project, Write: write, Admin: scope.Admin, Action: scope.Action, Role: scope.Credential.HostedRole}
	if scope.Credential.Hosted != nil && (scope.Credential.EntryKey == nil || scope.Credential.EntryKey.Kind != "service") {
		authority.UserID = scope.Credential.Hosted.Subject
		authority.MembershipID = scope.Credential.HostedMembership
	}
	return authority
}

func (s *Authenticator) HiddenDenial(ctx context.Context, r accessstore.Reader, scope Input, write bool, denial error) error {
	if scope.Project == "" || !errors.Is(denial, accessstore.ErrDenied) && !errors.Is(denial, auth.ErrHostedIdentity) || errors.Is(denial, tenantauth.ErrUnavailable) {
		return denial
	}
	visibility := Authority(scope, false)
	visibility.Admin, visibility.Action = false, ""
	if write || scope.Admin {
		if _, err := r.AuthorizeAccess(ctx, visibility, s.deps.Now()); err == nil {
			return denial
		} else if !errors.Is(err, accessstore.ErrDenied) || errors.Is(err, tenantauth.ErrUnavailable) {
			return err
		}
	}
	visibility.ProjectID = ""
	if _, err := r.AuthorizeAccess(ctx, visibility, s.deps.Now()); err != nil {
		return err
	}
	return hubapi.NotFound()
}
