package hubserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedAdministratorProjectGrants(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"UI", "API", "native insertion", "repository alias", "backfill", "foreign organization"} {
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
					operatorSQL(t, f, "UPDATE hosted_members SET active=0 WHERE user_id=?", u.identity.Subject)
				}
			}
			project, organization := "prj_new", "org_security"
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
			case "repository alias":
				operatorSQL(t, f, "UPDATE organizations SET local=(id='org_security')")
				operatorSQL(t, f, "INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES ('repo_new','digitaldrywood','detent','now','now')")
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM projects WHERE organization_id='org_security' AND name='digitaldrywood/detent'").Scan(&project); err != nil {
					t.Fatal(err)
				}
			case "backfill":
				project = string(f.project)
				operatorSQL(t, f, "DROP TRIGGER projects_hosted_administrator_grants")
				operatorSQL(t, f, "DROP TRIGGER projects_hosted_grants_delete")
				operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id='user_other-owner'")
				operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=(SELECT principal_id FROM hosted_members WHERE user_id='user_other-owner')")
				operatorSQL(t, f, "DELETE FROM hub_schema_version WHERE version_id=20261006202000")
				if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
					t.Fatal(err)
				}
			case "foreign organization":
				organization = "org_foreign"
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES ('org_foreign','Foreign','now')"); err == nil || !strings.Contains(err.Error(), "hosted organization is isolated") {
					t.Fatalf("foreign organization creation=%v", err)
				}
			default:
				operatorSQL(t, f, "INSERT INTO projects(id,organization_id,name,profile,created_at) VALUES (?,?,'New project','native','now')", project, organization)
			}
			for _, user := range users {
				var grants, write, runner, tokens int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*),COALESCE(max(can_write),0),COALESCE(max(manage_runner),0) FROM hosted_project_grants WHERE user_id=? AND organization_id=? AND project_id=?", "user_"+user.name, organization, project).Scan(&grants, &write, &runner); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM token_grants WHERE token_id=(SELECT principal_id FROM hosted_members WHERE user_id=?) AND organization_id=? AND project_id=?", "user_"+user.name, organization, project).Scan(&tokens); err != nil {
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
			if path == "repository alias" {
				operatorSQL(t, f, "DELETE FROM projects WHERE id=?", project)
				var remaining int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_project_grants WHERE project_id=?)+(SELECT count(*) FROM token_grants WHERE project_id=?)", project, project).Scan(&remaining); err != nil || remaining != 0 {
					t.Fatalf("deleted alias grants=%d error=%v", remaining, err)
				}
			}
		})
	}
}
