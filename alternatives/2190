package hubtest_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) {
	hubtest.Main(m)
}

func TestSeededTenantsAreUsableAndIsolated(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	first := hubtest.Organization(t, repository, hubtest.OrganizationOptions{})
	second := hubtest.Organization(t, repository, hubtest.OrganizationOptions{})
	projects := map[tracker.OrganizationID][]tracker.NativeProject{}
	for _, organization := range []tracker.OrganizationID{first.OrganizationID, second.OrganizationID} {
		for range 2 {
			projects[organization] = append(projects[organization], hubtest.Project(t, repository, organization, hubtest.ProjectOptions{}))
		}
	}
	for _, test := range []struct {
		name    string
		reader  tracker.OrganizationID
		project tracker.NativeProject
		want    error
	}{
		{"first organization first project", first.OrganizationID, projects[first.OrganizationID][0], nil},
		{"first organization second project", first.OrganizationID, projects[first.OrganizationID][1], nil},
		{"second organization first project", second.OrganizationID, projects[second.OrganizationID][0], nil},
		{"second organization second project", second.OrganizationID, projects[second.OrganizationID][1], nil},
		{"first organization cannot read second", first.OrganizationID, projects[second.OrganizationID][0], corestore.ErrNotFound},
		{"second organization cannot read first", second.OrganizationID, projects[first.OrganizationID][1], corestore.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := repository.ReadTenant(t.Context(), test.reader, corestore.Serving, func(r tenantstore.Reader) error {
				record, err := r.ProjectRecord(t.Context(), test.project.ID)
				if err != nil {
					return err
				}
				if record.Core.OrganizationID != test.reader || record.Core.Slug != test.project.Slug || len(record.States) != len(hubtest.States()) {
					t.Errorf("project record = %+v states=%d", record.Core, len(record.States))
				}
				organization, err := r.AccessOrganization(t.Context())
				if err != nil {
					return err
				}
				if organization.OrganizationID != test.reader {
					t.Errorf("organization = %q, want %q", organization.OrganizationID, test.reader)
				}
				return nil
			})
			if !errors.Is(err, test.want) || (test.want == nil) != (err == nil) {
				t.Fatalf("read = %v, want %v", err, test.want)
			}
		})
	}
	if projects[first.OrganizationID][0].Slug == projects[first.OrganizationID][1].Slug {
		t.Fatal("two projects in one organization share a slug")
	}
}

func TestScopeAuthorizesSeededCredentials(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	home := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: clock.Now()}).OrganizationID
	foreign := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: clock.Now()}).OrganizationID
	project := hubtest.Project(t, repository, home, hubtest.ProjectOptions{}).ID
	other := hubtest.Project(t, repository, home, hubtest.ProjectOptions{}).ID
	foreignProject := hubtest.Project(t, repository, foreign, hubtest.ProjectOptions{}).ID
	service := hubtest.Credential(t, repository, home, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project}, Now: clock.Now()})
	reader := hubtest.Credential(t, repository, home, hubtest.CredentialOptions{KeyScope: "read", Projects: []tracker.ProjectID{project}, Now: clock.Now()})
	outsider := hubtest.Credential(t, repository, foreign, hubtest.CredentialOptions{Projects: []tracker.ProjectID{foreignProject}, Now: clock.Now()})
	owner := hubtest.Member(t, repository, home, hubtest.MemberOptions{Projects: []tracker.ProjectID{project}, Now: clock.Now()})
	viewer := hubtest.Member(t, repository, home, hubtest.MemberOptions{Role: "member", Projects: []tracker.ProjectID{project}, ReadOnly: true, Now: clock.Now()})
	personal := hubtest.Credential(t, repository, home, hubtest.CredentialOptions{Member: &owner, Projects: []tracker.ProjectID{project}, Now: clock.Now()})
	expiry := clock.Now().Add(time.Hour)
	expiring := hubtest.Credential(t, repository, home, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project}, ExpiresAt: &expiry, Now: clock.Now()})
	late := func() time.Time { return expiry.Add(time.Second) }
	for _, test := range []struct {
		name   string
		tenant tracker.OrganizationID
		facts  hubtest.ScopeFacts
		write  bool
		denied bool
	}{
		{"service credential reads its project", home, hubtest.ScopeFacts{Credential: service, Project: project}, false, false},
		{"service credential writes its project", home, hubtest.ScopeFacts{Credential: service, Project: project}, true, false},
		{"service credential lacks the other project", home, hubtest.ScopeFacts{Credential: service, Project: other}, false, true},
		{"read key reads", home, hubtest.ScopeFacts{Credential: reader, Project: project}, false, false},
		{"read key cannot write", home, hubtest.ScopeFacts{Credential: reader, Project: project}, true, true},
		{"foreign credential is denied", home, hubtest.ScopeFacts{Credential: outsider, Project: project}, false, true},
		{"foreign credential works at home", foreign, hubtest.ScopeFacts{Credential: outsider, Project: foreignProject}, true, false},
		{"owner principal writes", home, hubtest.ScopeFacts{Credential: owner.Principal, Project: project}, true, false},
		{"owner principal is admin", home, hubtest.ScopeFacts{Credential: owner.Principal, Project: project, Admin: true}, true, false},
		{"owner principal reaches the other project by role", home, hubtest.ScopeFacts{Credential: owner.Principal, Project: other}, true, false},
		{"member principal lacks the other project", home, hubtest.ScopeFacts{Credential: viewer.Principal, Project: other}, false, true},
		{"personal key writes", home, hubtest.ScopeFacts{Credential: personal, Project: project}, true, false},
		{"personal key is not admin", home, hubtest.ScopeFacts{Credential: personal, Project: project, Admin: true}, true, true},
		{"read-only member reads", home, hubtest.ScopeFacts{Credential: viewer.Principal, Project: project}, false, false},
		{"read-only member cannot write", home, hubtest.ScopeFacts{Credential: viewer.Principal, Project: project}, true, true},
		{"expiring credential before expiry", home, hubtest.ScopeFacts{Credential: expiring, Project: project}, false, false},
		{"expiring credential after expiry", home, hubtest.ScopeFacts{Credential: expiring, Project: project, Now: late}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := test.facts
			if facts.Now == nil {
				facts.Now = clock.Now
			}
			scope := hubtest.Scope(t, facts)
			if scope.CredentialID != facts.Credential.ID || scope.Project != facts.Project || scope.Organization != facts.Credential.Organization {
				t.Fatalf("scope facts = %+v", scope.Facts)
			}
			err := repository.ReadTenant(t.Context(), test.tenant, corestore.Serving, func(r tenantstore.Reader) error {
				return scope.Authorize(t.Context(), r, test.write)
			})
			if test.denied {
				if !errors.Is(err, accessstore.ErrDenied) {
					t.Fatalf("Authorize = %v, want access denied", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize = %v", err)
			}
		})
	}
	if (hubapi.Scope{}).Authorize(t.Context(), nil, false) == nil {
		t.Fatal("zero scope authorized")
	}
}

func TestRepositoriesShareTheConnectionBudget(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	first := hubtest.RepositoryOn(t, admin)
	second := hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(4))
	if limit, want := hubtest.ConnectionLimit(t, admin), 2+hubtest.DefaultMaxConnections+4; limit != want {
		t.Fatalf("connection limit = %d, want %d", limit, want)
	}
	organization := hubtest.Organization(t, first, hubtest.OrganizationOptions{}).OrganizationID
	project := hubtest.Project(t, first, organization, hubtest.ProjectOptions{}).ID
	read := func(r tenantstore.Reader) error {
		_, err := r.ProjectRecord(t.Context(), project)
		return err
	}
	err := first.ReadTenant(t.Context(), organization, corestore.Serving, func(outer tenantstore.Reader) error {
		if err := read(outer); err != nil {
			return err
		}
		if err := first.ReadTenant(t.Context(), organization, corestore.Background, read); err != nil {
			return err
		}
		return second.ReadTenant(t.Context(), organization, corestore.Serving, func(inner tenantstore.Reader) error {
			if err := read(inner); err != nil {
				return err
			}
			if err := second.ReadTenant(t.Context(), organization, corestore.Background, read); err != nil {
				return err
			}
			var one int
			return admin.QueryRowContext(t.Context(), "SELECT 1").Scan(&one)
		})
	})
	if err != nil {
		t.Fatalf("concurrent pools: %v", err)
	}
}

func TestPostgresConfigConnectsToTheSameDatabase(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	seeder := hubtest.RepositoryOn(t, admin)
	config := hubtest.PostgresConfig(t, admin, hubtest.WithMaxConnections(4))
	if limit, want := hubtest.ConnectionLimit(t, admin), 2+hubtest.DefaultMaxConnections+4; limit != want {
		t.Fatalf("connection limit = %d, want %d", limit, want)
	}
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := postgres.Open(t.Context(), config, migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		organization := hubtest.Organization(t, seeder, hubtest.OrganizationOptions{}).OrganizationID
		project := hubtest.Project(t, seeder, organization, hubtest.ProjectOptions{}).ID
		if err := opened.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.ProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatalf("read %s through the configured pool: %v", organization, err)
		}
	}
}
