package hubserver

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedAdministratorProjectGrants(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"UI", "API", "native insertion", "compatibility insertion", "backfill", "foreign organization"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "read", "")
			users := []struct {
				name, role string
				active     bool
			}{
				{"owner", "owner", true}, {"other-owner", "owner", true}, {"admin", "admin", true},
				{"member", "member", true}, {"viewer", "viewer", true}, {"inactive-admin", "admin", false},
			}
			for _, user := range users[1:] {
				u := f.user(t, user.name, user.role, user.name+"@example.test", "read", "")
				if !user.active {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
						member, err := w.AccessMember(t.Context(), u.identity.Subject)
						if err != nil {
							return err
						}
						member.Active = false
						member.UpdatedAt = f.service.config.Clock().UTC()
						return w.UpdateAccessMember(t.Context(), member)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			principals := make(map[string]string, len(users))
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				for _, user := range users {
					member, err := r.AccessMember(t.Context(), "user_"+user.name)
					if err != nil {
						return err
					}
					principals[user.name] = member.PrincipalID
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			project, organization := "prj_new", "org_security"
			create := func(profile string) {
				t.Helper()
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.service.accessOrganization(), ID: tracker.ProjectID(project), Name: "New project", Profile: profile}, States: nativeFixtureStates()}); err != nil {
						return err
					}
					return w.InitializeProjectAccess(t.Context(), tracker.ProjectID(project), principals["owner"])
				}); err != nil {
					t.Fatal(err)
				}
			}
			switch path {
			case "UI":
				response := f.request(t, owner, http.MethodPost, "/projects", url.Values{"name": {"New project"}, "grant_access": {"true"}})
				requireNativeStatus(t, response, http.StatusSeeOther)
				project = strings.TrimPrefix(response.Header().Get("Location"), "/projects/")
			case "API":
				response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"name": "New project", "grant_access": true, "idempotency_key": "new-project"})
				requireNativeStatus(t, response, http.StatusCreated)
				var created tracker.NativeProject
				decodeHubResponse(t, response, &created)
				project = string(created.ID)
			case "compatibility insertion":
				create("github_compatible")
			case "backfill":
				project = string(f.project)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.DeleteAccessProjectGrant(t.Context(), "user_other-owner", f.project); err != nil {
						return err
					}
					return w.DeleteAccessTokenGrant(t.Context(), principals["other-owner"], f.project)
				}); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
						for _, user := range users {
							if err := w.SyncHostedAdministratorProjectAccess(t.Context(), "user_"+user.name, principals[user.name]); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign organization":
				foreign, err := f.service.createNativeOrganizationFor(t.Context(), "Foreign")
				if err != nil {
					t.Fatal(err)
				}
				organization = string(foreign.ID)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), foreign.ID, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: foreign.ID, ID: tracker.ProjectID(project), Name: "Foreign project", Profile: "native"}, States: nativeFixtureStates()})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: foreign.ID, ID: tracker.ProjectID(project), Name: "Wrong scope", Profile: "native"}, States: nativeFixtureStates()})
					return err
				}); !errors.Is(err, corestore.ErrTenantMismatch) {
					t.Fatalf("foreign organization creation=%v", err)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					if _, err := r.ProjectRecord(t.Context(), tracker.ProjectID(project)); !errors.Is(err, corestore.ErrNotFound) {
						return errors.New("foreign project appeared in selected tenant")
					}
					_, err := r.ProjectRecord(t.Context(), f.project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), foreign.ID, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.ProjectRecord(t.Context(), tracker.ProjectID(project))
					if err == nil && (row.Core.OrganizationID != foreign.ID || row.Core.Name != "Foreign project") {
						return errors.New("foreign project changed")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			default:
				create("native")
			}
			for _, user := range users {
				var grants, write, runner, tokens int
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
					grant, err := r.AccessProjectGrant(t.Context(), "user_"+user.name, tracker.ProjectID(project))
					if err == nil {
						grants = 1
						if grant.CanWrite {
							write = 1
						}
						if grant.ManageRunner {
							runner = 1
						}
					} else if !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					_, err = r.AccessTokenGrant(t.Context(), principals[user.name], tracker.ProjectID(project))
					if err == nil {
						tokens = 1
					} else if !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				want, wantWrite := 0, 0
				if organization == "org_security" && user.active && (user.role == "owner" || user.role == "admin") {
					want, wantWrite = 1, 1
				} else if path == "backfill" {
					want = 1
				}
				if grants != want || tokens != want || write != wantWrite || runner != wantWrite {
					t.Fatalf("%s grants/write/runner/tokens=%d/%d/%d/%d, want %d/%d/%d/%d", user.name, grants, write, runner, tokens, want, wantWrite, wantWrite, want)
				}
			}
			if path == "compatibility insertion" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.ProjectRecord(t.Context(), tracker.ProjectID(project))
					if err != nil {
						return err
					}
					if err := w.(accessstore.ProjectAccessCleaner).DeleteAccessProjectGrants(t.Context(), tracker.ProjectID(project)); err != nil {
						return err
					}
					return w.RetireProject(t.Context(), tracker.ProjectID(project), row.Settings.Revision, f.service.config.Clock())
				}); err != nil {
					t.Fatal(err)
				}
				var remaining int
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_project_grants WHERE organization_id=$1 AND project_id=$2)+(SELECT count(*) FROM token_grants WHERE organization_id=$1 AND project_id=$2)", f.service.accessOrganization(), project).Scan(&remaining); err != nil || remaining != 0 {
					t.Fatalf("deleted compatibility project grants=%d error=%v", remaining, err)
				}
			}

		})
	}
}
