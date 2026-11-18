package hubtest_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func refused(err error) bool {
	return errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity)
}

func TestHostedScopeEnforcesRolesAndGrants(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	tenants := twoTenants(t, repository, clock.Now())
	home, away := tenants[0], tenants[1]
	granted, ungranted := home.project.ID, home.other.ID
	member := func(organization tracker.OrganizationID, project tracker.ProjectID, role string, readOnly bool) hubtest.SeededMember {
		return hubtest.Member(t, repository, organization, hubtest.MemberOptions{Role: role, Projects: []tracker.ProjectID{project}, ReadOnly: readOnly, Now: clock.Now()})
	}
	owner, admin, regular, viewer, reporter := member(home.organization, granted, "owner", false), member(home.organization, granted, "admin", false), member(home.organization, granted, "member", false), member(home.organization, granted, "viewer", false), member(home.organization, granted, "reporter", false)
	readOnly := member(home.organization, granted, "member", true)
	foreign := member(away.organization, away.project.ID, "owner", false)
	type access struct{ read, write, admin bool }
	for _, test := range []struct {
		name  string
		facts hubtest.HostedFacts
		want  access
	}{
		{"owner on a granted project", hubtest.HostedFacts{Member: owner, Project: granted}, access{true, true, true}},
		{"admin on a granted project", hubtest.HostedFacts{Member: admin, Project: granted}, access{true, true, true}},
		{"member on a granted project", hubtest.HostedFacts{Member: regular, Project: granted}, access{true, true, false}},
		{"viewer on a granted project", hubtest.HostedFacts{Member: viewer, Project: granted}, access{true, false, false}},
		{"reporter without an action", hubtest.HostedFacts{Member: reporter, Project: granted}, access{true, false, false}},
		{"reporter filing an issue", hubtest.HostedFacts{Member: reporter, Project: granted, Action: operatortool.FileIssue}, access{true, true, false}},
		{"read-only grant", hubtest.HostedFacts{Member: readOnly, Project: granted}, access{true, false, false}},
		{"owner without a project grant has role access", hubtest.HostedFacts{Member: owner, Project: ungranted}, access{true, true, true}},
		{"admin without a project grant has role access", hubtest.HostedFacts{Member: admin, Project: ungranted}, access{true, true, true}},
		{"member without a project grant", hubtest.HostedFacts{Member: regular, Project: ungranted}, access{false, false, false}},
		{"owner at organization level", hubtest.HostedFacts{Member: owner}, access{true, true, true}},
		{"viewer at organization level", hubtest.HostedFacts{Member: viewer}, access{true, false, false}},
		{"provider downgrades owner to viewer", hubtest.HostedFacts{Member: owner, Project: granted, ProviderRole: "viewer"}, access{true, false, false}},
		{"provider downgrades owner to member", hubtest.HostedFacts{Member: owner, Project: granted, ProviderRole: "member"}, access{true, true, false}},
		{"provider cannot upgrade a viewer", hubtest.HostedFacts{Member: viewer, Project: granted, ProviderRole: "owner"}, access{true, false, false}},
		{"foreign member against this organization", hubtest.HostedFacts{Member: foreign, Organization: home.organization, Project: granted}, access{false, false, false}},
		{"foreign member against its own project here", hubtest.HostedFacts{Member: foreign, Organization: home.organization, Project: away.project.ID}, access{false, false, false}},
		{"foreign member at home", hubtest.HostedFacts{Member: foreign, Project: away.project.ID}, access{true, true, true}},
		{"member on another organization's project", hubtest.HostedFacts{Member: owner, Project: away.project.ID}, access{false, false, false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, attempt := range []struct {
				name         string
				write, admin bool
				want         bool
			}{
				{"read", false, false, test.want.read}, {"write", true, false, test.want.write}, {"admin", true, true, test.want.admin},
			} {
				facts := test.facts
				facts.Admin, facts.Now = attempt.admin, clock.Now
				scope := hubtest.HostedScope(t, facts)
				err := read(t, repository, scope.Organization, func(r tenantstore.Reader) error { return scope.Authorize(t.Context(), r, attempt.write) })
				if attempt.want && err != nil || !attempt.want && !refused(err) {
					t.Errorf("%s = %v, want allowed %t", attempt.name, err, attempt.want)
				}
			}
		})
	}
	scope := hubtest.HostedScope(t, hubtest.HostedFacts{Member: owner, Project: granted, ProviderRole: "admin", Now: clock.Now})
	if scope.HostedRole != "admin" || scope.Organization != home.organization || scope.CredentialID != owner.Principal.ID || scope.Actor.Kind != "human" {
		t.Fatalf("hosted scope facts = %+v", scope.Facts)
	}
}

func TestHostedScopeFollowsGrantSessionAndKeyChanges(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	tenants := twoTenants(t, repository, clock.Now())
	home := tenants[0]
	project := home.project.ID
	options := hubtest.MemberOptions{Role: "member", Projects: []tracker.ProjectID{project}, Now: clock.Now()}
	first, second := hubtest.Member(t, repository, home.organization, options), hubtest.Member(t, repository, home.organization, options)
	session := hubtest.Session(t, repository, first, hubtest.SessionOptions{TTL: time.Hour, Now: clock.Now()})
	personal := hubtest.Credential(t, repository, home.organization, hubtest.CredentialOptions{Member: &first, KeyScope: "read", Projects: []tracker.ProjectID{project}, Now: clock.Now()})
	late := func() time.Time { return clock.Now().Add(2 * time.Hour) }
	personalKey := func(permission apikey.Scope, projects ...string) *apikey.KeyAuthority {
		return &apikey.KeyAuthority{ID: "key_example", Kind: "personal", Permission: permission, ProjectContext: apikey.ProjectContext{Access: "selected", Projects: projects}}
	}
	authorize := func(facts hubtest.HostedFacts, write bool) error {
		if facts.Now == nil {
			facts.Now = clock.Now
		}
		scope := hubtest.HostedScope(t, facts)
		return read(t, repository, home.organization, func(r tenantstore.Reader) error { return scope.Authorize(t.Context(), r, write) })
	}
	for _, test := range []struct {
		name    string
		facts   hubtest.HostedFacts
		write   bool
		allowed bool
	}{
		{"own session", hubtest.HostedFacts{Member: first, Project: project, SessionHash: session}, true, true},
		{"another member's session", hubtest.HostedFacts{Member: second, Project: project, SessionHash: session}, false, false},
		{"unknown session", hubtest.HostedFacts{Member: first, Project: project, SessionHash: "missing"}, false, false},
		{"expired session", hubtest.HostedFacts{Member: first, Project: project, SessionHash: session, Now: late}, false, false},
		{"personal read key reads", hubtest.HostedFacts{Member: first, Project: project, Key: &personal}, false, true},
		{"personal read key cannot write", hubtest.HostedFacts{Member: first, Project: project, Key: &personal}, true, false},
		{"personal key of another member", hubtest.HostedFacts{Member: second, Project: project, Key: &personal}, false, false},
		{"entry key allows its project", hubtest.HostedFacts{Member: first, Project: project, EntryKey: personalKey(apikey.ScopeWrite, string(project)), KeyScope: apikey.ScopeWrite}, true, true},
		{"entry key read permission blocks writes", hubtest.HostedFacts{Member: first, Project: project, EntryKey: personalKey(apikey.ScopeRead, string(project)), KeyScope: apikey.ScopeRead}, true, false},
		{"entry key lacks the project", hubtest.HostedFacts{Member: first, Project: project, EntryKey: personalKey(apikey.ScopeWrite, string(home.other.ID)), KeyScope: apikey.ScopeWrite}, false, false},
		{"entry key without a key scope", hubtest.HostedFacts{Member: first, Project: project, EntryKey: personalKey(apikey.ScopeWrite, string(project))}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := authorize(test.facts, test.write)
			if test.allowed && err != nil || !test.allowed && !refused(err) {
				t.Fatalf("Authorize = %v, want allowed %t", err, test.allowed)
			}
		})
	}
	if err := repository.WriteTenant(t.Context(), home.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.DeleteAccessProjectGrant(t.Context(), first.Member.UserID, project); err != nil {
			return err
		}
		demoted := second.Member
		demoted.Role, demoted.UpdatedAt = "viewer", clock.Now()
		return w.UpdateAccessMember(t.Context(), demoted)
	}); err != nil {
		t.Fatal(err)
	}
	if err := authorize(hubtest.HostedFacts{Member: first, Project: project}, false); !refused(err) {
		t.Fatalf("read after grant revocation = %v, want refused", err)
	}
	if err := authorize(hubtest.HostedFacts{Member: second, Project: project}, false); err != nil {
		t.Fatalf("read after role downgrade = %v", err)
	}
	if err := authorize(hubtest.HostedFacts{Member: second, Project: project}, true); !refused(err) {
		t.Fatalf("write after role downgrade = %v, want refused", err)
	}
}

func TestScopeForProjectReauthorizesThroughTheStore(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	tenants := twoTenants(t, repository, clock.Now())
	home, away := tenants[0], tenants[1]
	third := hubtest.Project(t, repository, home.organization, hubtest.ProjectOptions{}).ID
	both := []tracker.ProjectID{home.project.ID, home.other.ID}
	service := hubtest.Credential(t, repository, home.organization, hubtest.CredentialOptions{Projects: both, Now: clock.Now()})
	member := hubtest.Member(t, repository, home.organization, hubtest.MemberOptions{Role: "member", Projects: both, Now: clock.Now()})
	for _, origin := range []struct {
		name  string
		scope func() hubtest.ScopeFacts
	}{
		{"service credential", func() hubtest.ScopeFacts {
			return hubtest.ScopeFacts{Credential: service, Project: home.project.ID, Now: clock.Now}
		}},
		{"member principal", func() hubtest.ScopeFacts {
			return hubtest.ScopeFacts{Credential: member.Principal, Project: home.project.ID, Now: clock.Now}
		}},
	} {
		for _, test := range []struct {
			name    string
			project tracker.ProjectID
			allowed bool
		}{
			{"second granted project", home.other.ID, true},
			{"ungranted project in the same organization", third, false},
			{"project in another organization", away.project.ID, false},
			{"back to the first project", home.project.ID, true},
		} {
			t.Run(origin.name+"/"+test.name, func(t *testing.T) {
				original := hubtest.Scope(t, origin.scope())
				rebound := original.ForProject(test.project)
				if rebound.Project != test.project || rebound.Organization != home.organization || original.Project != home.project.ID {
					t.Fatalf("rebound = %+v original = %+v", rebound.Facts, original.Facts)
				}
				err := read(t, repository, rebound.Organization, func(r tenantstore.Reader) error { return rebound.Authorize(t.Context(), r, true) })
				if test.allowed && err != nil || !test.allowed && !errors.Is(err, accessstore.ErrDenied) {
					t.Fatalf("Authorize = %v, want allowed %t", err, test.allowed)
				}
			})
		}
	}
	hosted := hubtest.HostedScope(t, hubtest.HostedFacts{Member: member, Project: home.project.ID, Now: clock.Now})
	for project, allowed := range map[tracker.ProjectID]bool{home.other.ID: true, third: false, away.project.ID: false} {
		err := read(t, repository, home.organization, func(r tenantstore.Reader) error {
			return hosted.ForProject(project).Authorize(t.Context(), r, true)
		})
		if allowed && err != nil || !allowed && !refused(err) {
			t.Fatalf("hosted scope rebound to %s = %v, want allowed %t", project, err, allowed)
		}
	}
}
