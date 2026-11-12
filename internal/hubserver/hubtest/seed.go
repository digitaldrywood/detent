package hubtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func States() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

func seedTime(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC().Truncate(time.Microsecond)
}

func write(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, step string, operation func(tenantstore.Writer) error) {
	t.Helper()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, operation); err != nil {
		t.Fatalf("seed %s: %v", step, err)
	}
}

type OrganizationOptions struct {
	ID   tracker.OrganizationID
	Name string
	Slug string
	Now  time.Time
}

func Organization(t *testing.T, repository tenantstore.Repository, options OrganizationOptions) accessstore.Organization {
	t.Helper()
	if options.ID == "" {
		options.ID = tracker.OrganizationID(hubapi.NewID("org"))
	}
	if options.Name == "" {
		options.Name = string(options.ID)
	}
	if options.Slug == "" {
		options.Slug = strings.ReplaceAll(string(options.ID), "_", "-")
	}
	row := accessstore.Organization{OrganizationID: options.ID, Name: options.Name, Slug: options.Slug, CreatedAt: seedTime(options.Now)}
	write(t, repository, options.ID, "organization", func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), row); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: options.ID, ProviderID: "provider_" + string(options.ID)})
	})
	return row
}

type ProjectOptions struct {
	ID                  tracker.ProjectID
	Name                string
	States              []tracker.NativeState
	RequireDependencies bool
}

func Project(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, options ProjectOptions) tracker.NativeProject {
	t.Helper()
	if options.ID == "" {
		options.ID = tracker.ProjectID(hubapi.NewID("prj"))
	}
	if options.Name == "" {
		options.Name = "Example project " + string(options.ID)
	}
	if options.States == nil {
		options.States = States()
	}
	var record projectstore.Record
	write(t, repository, organization, "project", func(w tenantstore.Writer) error {
		var err error
		record, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: options.ID, Name: options.Name, Profile: "native", RequireDependencies: options.RequireDependencies}, States: options.States})
		return err
	})
	return tracker.NativeProject{OrganizationID: organization, ID: options.ID, Name: record.Core.Name, Slug: record.Core.Slug, States: options.States, Profile: "native"}
}

type SeededCredential struct {
	Organization tracker.OrganizationID
	ID           string
	Token        string
	Hash         string
	Kind         string
	UserID       string
	MembershipID string
}

type CredentialOptions struct {
	Scope      string
	KeyScope   string
	Projects   []tracker.ProjectID
	AllProject bool
	Member     *SeededMember
	ExpiresAt  *time.Time
	Now        time.Time
}

func Credential(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, options CredentialOptions) SeededCredential {
	t.Helper()
	var seeded SeededCredential
	write(t, repository, organization, "credential", func(w tenantstore.Writer) error {
		var err error
		seeded, err = createCredential(t.Context(), w, organization, options)
		return err
	})
	return seeded
}

func createCredential(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, options CredentialOptions) (SeededCredential, error) {
	now := seedTime(options.Now)
	seeded := SeededCredential{Organization: organization, ID: hubapi.NewID("credential"), Token: hubapi.NewID("token"), Kind: "service"}
	seeded.Hash = apikey.HashToken(seeded.Token)
	if options.Scope == "" {
		options.Scope = "operator"
	}
	row := accessstore.Credential{OrganizationID: organization, ID: seeded.ID, Name: "Example credential " + seeded.ID, Hash: seeded.Hash, Scope: options.Scope, NativeOnly: options.Scope != "admin", KeyScope: options.KeyScope, Kind: seeded.Kind, ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: options.ExpiresAt}
	if options.AllProject {
		row.ProjectAccess = "all"
	}
	if options.Member != nil {
		seeded.Kind, seeded.UserID, seeded.MembershipID = "personal", options.Member.Member.UserID, options.Member.Member.MembershipID
		row.Kind, row.UserID, row.MembershipID = seeded.Kind, seeded.UserID, seeded.MembershipID
		if row.KeyScope == "" {
			row.KeyScope = "write"
		}
	}
	if err := w.CreateAccessCredential(ctx, row); err != nil {
		return SeededCredential{}, err
	}
	for _, project := range options.Projects {
		if err := w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: seeded.ID, ProjectID: project}); err != nil {
			return SeededCredential{}, err
		}
	}
	return seeded, nil
}

type SeededMember struct {
	Member    accessstore.Member
	Principal SeededCredential
}

type MemberOptions struct {
	Role          string
	Email         string
	Projects      []tracker.ProjectID
	ReadOnly      bool
	ManageRunners bool
	Now           time.Time
}

func Member(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, options MemberOptions) SeededMember {
	t.Helper()
	now := seedTime(options.Now)
	if options.Role == "" {
		options.Role = "owner"
	}
	suffix := strings.TrimPrefix(hubapi.NewID("member"), "member_")
	member := accessstore.Member{OrganizationID: organization, UserID: "user_" + suffix, Email: suffix + "@example.test", MembershipID: "membership_" + suffix, Role: options.Role, Active: true, CreatedAt: now, UpdatedAt: now}
	if options.Email != "" {
		member.Email = options.Email
	}
	seeded := SeededMember{}
	write(t, repository, organization, "member", func(w tenantstore.Writer) error {
		keyScope := "write"
		if options.Role == "owner" || options.Role == "admin" {
			keyScope = "admin"
		}
		principal, err := createCredential(t.Context(), w, organization, CredentialOptions{KeyScope: keyScope, Projects: options.Projects, Member: &SeededMember{Member: member}, Now: now})
		if err != nil {
			return err
		}
		member.PrincipalID = principal.ID
		if err := w.CreateAccessMember(t.Context(), member); err != nil {
			return err
		}
		for _, project := range options.Projects {
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: project, CanWrite: !options.ReadOnly, ManageRunner: options.ManageRunners}); err != nil {
				return err
			}
		}
		seeded = SeededMember{Member: member, Principal: principal}
		return nil
	})
	return seeded
}

type ScopeFacts struct {
	Credential SeededCredential
	Project    tracker.ProjectID
	Actor      tracker.Actor
	HostedRole string
	Action     string
	Admin      bool
	Runner     runnerauth.Identity
	Now        func() time.Time
}

func Scope(t *testing.T, facts ScopeFacts) hubapi.Scope {
	t.Helper()
	if facts.Credential.ID == "" || facts.Credential.Organization == "" {
		t.Fatal("hubtest scope requires a seeded credential")
	}
	now := facts.Now
	if now == nil {
		now = time.Now
	}
	actor := facts.Actor
	if actor == (tracker.Actor{}) {
		actor = tracker.Actor{Kind: "integration", PrincipalID: facts.Credential.ID}
		if facts.Credential.Kind == "personal" {
			actor.Kind = "human"
		}
	}
	credential := facts.Credential
	return hubapi.NewProjectScope(hubapi.Facts{Organization: credential.Organization, Project: facts.Project, Actor: actor, CredentialID: credential.ID, HostedRole: facts.HostedRole, Admin: facts.Admin, Runner: facts.Runner},
		func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error {
			_, err := reader.AuthorizeAccess(ctx, accessstore.Authority{Action: facts.Action, CredentialID: credential.ID, Hash: credential.Hash, UserID: credential.UserID, MembershipID: credential.MembershipID, Role: facts.HostedRole, ProjectID: project, Write: write, Admin: facts.Admin}, now())
			return err
		})
}
