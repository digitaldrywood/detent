package hubtest

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SessionOptions struct {
	TTL time.Duration
	Now time.Time
}

func Session(t *testing.T, repository tenantstore.Repository, member SeededMember, options SessionOptions) string {
	t.Helper()
	now := seedTime(options.Now)
	if options.TTL == 0 {
		options.TTL = time.Hour
	}
	organization := member.Member.OrganizationID
	hash := apikey.HashToken(hubapi.NewID("session"))
	write(t, repository, organization, "session", func(w tenantstore.Writer) error {
		return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: organization, Hash: hash, Email: member.Member.Email, Subject: member.Member.UserID, ProviderOrganizationID: "provider_" + string(organization), SessionID: hubapi.NewID("provider_session"), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(options.TTL), CreatedAt: now, ExpiresAt: now.Add(options.TTL)})
	})
	return hash
}

type HostedFacts struct {
	Member       SeededMember
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
	ProviderRole string
	Action       string
	Admin        bool
	SessionHash  string
	Key          *SeededCredential
	EntryKey     *apikey.KeyAuthority
	KeyScope     apikey.Scope
	Now          func() time.Time
}

func HostedScope(t *testing.T, facts HostedFacts) hubapi.Scope {
	t.Helper()
	member := facts.Member.Member
	if member.UserID == "" || facts.Member.Principal.ID == "" {
		t.Fatal("hubtest hosted scope requires a seeded member")
	}
	now := facts.Now
	if now == nil {
		now = time.Now
	}
	organization := facts.Organization
	if organization == "" {
		organization = member.OrganizationID
	}
	provider := facts.ProviderRole
	if provider == "" {
		provider = member.Role
	}
	role := hubapi.LesserHostedRole(provider, member.Role)
	credential := facts.Member.Principal
	if facts.Key != nil {
		credential = *facts.Key
	}
	entry, keyScope, action, admin := facts.EntryKey, facts.KeyScope, facts.Action, facts.Admin
	scopeFacts := hubapi.Facts{Organization: organization, Project: facts.Project, Actor: tracker.Actor{Kind: "human", PrincipalID: facts.Member.Principal.ID}, CredentialID: credential.ID, HostedRole: role, Admin: admin}
	return hubapi.NewProjectScope(scopeFacts, func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error {
		if entry != nil {
			if project != "" && !entry.Allows(string(project)) {
				return auth.ErrHostedIdentity
			}
			required := apikey.ScopeRead
			if write {
				required = apikey.ScopeWrite
			}
			if admin {
				required = apikey.ScopeAdmin
			}
			if !apikey.ValidScope(keyScope) || !apikey.HasScope([]string{string(keyScope)}, required) || !accessstore.RoleAllows(role, required, action) {
				return auth.ErrHostedIdentity
			}
		}
		authority := accessstore.Authority{CredentialID: credential.ID, Hash: credential.Hash, SessionHash: facts.SessionHash, ProjectID: project, Write: write, Admin: admin, Action: action, Role: role}
		if entry == nil || entry.Kind != "service" {
			authority.UserID, authority.MembershipID = member.UserID, member.MembershipID
		}
		authorized, err := reader.AuthorizeAccess(ctx, authority, now())
		if err != nil {
			return err
		}
		if authorized.UserID != "" {
			effective := hubapi.LesserHostedRole(authorized.Role, role)
			if admin && effective != "owner" && effective != "admin" {
				return auth.ErrHostedIdentity
			}
		}
		return nil
	})
}
