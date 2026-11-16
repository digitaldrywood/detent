package access_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedAdministratorGrantChangeRefused(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	owner := f.User(t, "owner", "owner", "owner@example.test", "", "")
	f.User(t, "admin", "admin", "admin@example.test", "", "")
	f.User(t, "member", "member", "member@example.test", "", "")
	grants := func(user string) int {
		t.Helper()
		var count int
		if err := f.Admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_project_grants WHERE organization_id=$1 AND user_id=$2`, string(f.Primary), user).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	for _, target := range []string{"owner", "admin"} {
		for i, change := range []map[string]any{{"write": false}, {"revoke": true}, {"write": true, "runner": true}} {
			change["project_id"], change["idempotency_key"] = string(f.Project), fmt.Sprintf("grant-%s-%d", target, i)
			response := f.Request(t, owner, http.MethodPut, "/api/v2/organizations/org_security/members/membership_user_"+target+"/grants", change)
			hubfixture.RequireCode(t, response, http.StatusConflict, "role_grants_project_access")
		}
		response := f.Request(t, owner, http.MethodPost, "/organization/grants", url.Values{"user": {"user_" + target}, "project": {string(f.Project)}, "revoke": {"true"}})
		hubfixture.RequireStatus(t, response, http.StatusConflict)
		if count := grants("user_" + target); count != 0 {
			t.Fatalf("%s grant rows=%d after refused changes, want 0", target, count)
		}
	}
	response := f.Request(t, owner, http.MethodPut, "/api/v2/organizations/org_security/members/membership_user_member/grants", map[string]any{"project_id": string(f.Project), "write": true, "idempotency_key": "grant-member"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var explicit bool
	if err := f.Admin.QueryRowContext(t.Context(), `SELECT explicit FROM hosted_project_grants WHERE organization_id=$1 AND user_id='user_member' AND project_id=$2`, string(f.Primary), string(f.Project)).Scan(&explicit); err != nil || !explicit {
		t.Fatalf("member grant explicit=%t err=%v, want an explicit row", explicit, err)
	}
}

func TestHostedAdministratorRoleAccessAcrossOrganizations(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	second := tracker.OrganizationID("org_role_second")
	hubfixture.SeedOrganization(t, f.Repository, second, f.AdminToken(), f.Now())
	now := f.Now().UTC()
	stored := map[string]string{"owner": "owner", "admin": "admin", "member": "member", "granted": "owner"}
	for _, organization := range []tracker.OrganizationID{f.Primary, second} {
		if err := f.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []tracker.ProjectID{"prj_role_a", "prj_role_b"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Profile: "native"}, States: hubfixture.States()}); err != nil {
					return err
				}
			}
			for name, role := range stored {
				user, principal := string(organization)+"_"+name, string(organization)+"_principal_"+name
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: principal, Hash: "hash_" + principal, Scope: "operator", NativeOnly: true, ProjectAccess: "selected", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: role, Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := f.Admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM hosted_project_grants WHERE organization_id IN ($1,$2))+(SELECT count(*) FROM token_grants g JOIN hosted_members m ON m.organization_id=g.organization_id AND m.principal_id=g.token_id WHERE g.organization_id IN ($1,$2))`, string(f.Primary), string(second)).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("role fixture grant rows=%d err=%v, want 0", rows, err)
	}
	for _, organization := range []tracker.OrganizationID{f.Primary, second} {
		if err := f.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: string(organization) + "_granted", ProjectID: "prj_role_a", CanWrite: true}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: string(organization) + "_principal_granted", ProjectID: "prj_role_a"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	type access struct{ read, write, runner bool }
	full, none := access{true, true, true}, access{}
	tests := []struct {
		organization, member tracker.OrganizationID
		name, effective      string
		want                 int
		project              access
		authorized           access
	}{
		{f.Primary, f.Primary, "member", "member", 0, none, none},
		{second, f.Primary, "admin", "admin", 0, none, none},
		{f.Primary, second, "owner", "owner", 0, none, none},
	}
	for organization, projects := range map[tracker.OrganizationID]int{f.Primary: 3, second: 2} {
		tests = append(tests, []struct {
			organization, member tracker.OrganizationID
			name, effective      string
			want                 int
			project              access
			authorized           access
		}{
			{organization, organization, "owner", "owner", projects, full, access{true, true, false}},
			{organization, organization, "admin", "admin", projects, full, access{true, true, false}},
			{organization, organization, "owner", "member", 0, none, none},
			{organization, organization, "owner", "viewer", 0, none, none},
			{organization, organization, "admin", "member", 0, none, none},
			{organization, organization, "admin", "reporter", 0, none, none},
			{organization, organization, "member", "owner", 0, none, none},
			{organization, organization, "granted", "owner", projects, full, access{true, true, false}},
			{organization, organization, "granted", "member", 1, access{true, true, false}, access{true, true, false}},
			{organization, organization, "granted", "viewer", 1, access{true, true, false}, access{true, false, false}},
		}...)
	}
	for _, test := range tests {
		label := fmt.Sprintf("%s %s acting as %s in %s", test.member, test.name, test.effective, test.organization)
		user, principal := string(test.member)+"_"+test.name, string(test.member)+"_principal_"+test.name
		if err := f.Repository.ReadTenant(t.Context(), test.organization, corestore.Serving, func(r tenantstore.Reader) error {
			projects, err := r.(accessstore.ProjectAccessPageReader).ProjectAccessPage(t.Context(), accessstore.ProjectAccessFilter{UserID: user, Role: test.effective, PrincipalID: principal, CredentialID: principal, Limit: 10})
			if err != nil {
				return err
			}
			if len(projects) != test.want {
				return fmt.Errorf("%s sees %d projects, want %d", label, len(projects), test.want)
			}
			for _, project := range projects {
				want := full
				if test.want == 1 {
					want = test.project
				}
				if project.CanWrite != want.write || project.ManageRunner != want.runner {
					return fmt.Errorf("%s project %s write/runner=%t/%t, want %t/%t", label, project.ProjectID, project.CanWrite, project.ManageRunner, want.write, want.runner)
				}
			}
			grant, err := r.AccessProjectGrant(t.Context(), user, "prj_role_a", test.effective)
			switch {
			case !test.project.read && !errors.Is(err, corestore.ErrNotFound):
				return fmt.Errorf("%s project grant %+v err=%v, want not found", label, grant, err)
			case test.project.read && (err != nil || grant.CanWrite != test.project.write || grant.ManageRunner != test.project.runner):
				return fmt.Errorf("%s project grant %+v err=%v, want write/runner %t/%t", label, grant, err, test.project.write, test.project.runner)
			}
			if _, err := r.AccessTokenGrant(t.Context(), principal, "prj_role_b", test.effective); (err == nil) != (test.want > 1) {
				return fmt.Errorf("%s principal grant on the ungranted project err=%v", label, err)
			}
			for _, write := range []bool{false, true} {
				allowed := test.authorized.read
				if write {
					allowed = test.authorized.write
				}
				_, err := r.AuthorizeAccess(t.Context(), accessstore.Authority{CredentialID: principal, Hash: "hash_" + principal, UserID: user, MembershipID: "membership_" + user, Role: test.effective, ProjectID: "prj_role_a", Write: write}, f.Now())
				if allowed && err != nil || !allowed && !errors.Is(err, accessstore.ErrDenied) {
					return fmt.Errorf("%s authorize write=%t err=%v, want allowed=%t", label, write, err, allowed)
				}
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
}

func TestHostedAdministratorRoleChangesApplyAtNextRequest(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	owner := f.User(t, "owner", "owner", "owner@example.test", "", "")
	admin := f.User(t, "admin", "member", "admin@example.test", "", "")
	member := f.User(t, "member", "member", "member@example.test", "", "")
	other := f.SeedProject(t, "prj_explicit", "explicit-project-sentinel")
	role := func(user, value string) {
		t.Helper()
		response := f.Request(t, owner, http.MethodPut, "/api/v2/organizations/org_security/members/membership_user_"+user+"/role", map[string]any{"role": value, "idempotency_key": user + "-" + value})
		hubfixture.RequireStatus(t, response, http.StatusOK)
	}
	explicit := f.Request(t, owner, http.MethodPut, "/api/v2/organizations/org_security/members/membership_user_admin/grants", map[string]any{"project_id": string(other.ID), "idempotency_key": "explicit"})
	hubfixture.RequireStatus(t, explicit, http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, admin, http.MethodGet, f.Base, nil), http.StatusNotFound)
	role("admin", "admin")
	hubfixture.RequireStatus(t, f.Request(t, admin, http.MethodGet, f.Base, nil), http.StatusOK)
	if _, err := f.Admin.ExecContext(t.Context(), `WITH legacy AS (INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner) VALUES($1,'user_admin',$2,true,true)) INSERT INTO token_grants(organization_id,token_id,project_id) SELECT $1,principal_id,$2 FROM hosted_members WHERE organization_id=$1 AND user_id='user_admin'`, string(f.Primary), string(f.Project)); err != nil {
		t.Fatal(err)
	}
	role("admin", "member")
	hubfixture.RequireStatus(t, f.Request(t, admin, http.MethodGet, f.Base, nil), http.StatusNotFound)
	hubfixture.RequireStatus(t, f.Request(t, admin, http.MethodGet, hubfixture.ProjectBase(f.Primary, other.ID), nil), http.StatusOK)
	var legacy, kept int
	if err := f.Admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE NOT explicit),count(*) FILTER (WHERE explicit AND project_id=$2) FROM hosted_project_grants WHERE organization_id=$1 AND user_id='user_admin'`, string(f.Primary), string(other.ID)).Scan(&legacy, &kept); err != nil || legacy != 0 || kept != 1 {
		t.Fatalf("demoted admin legacy/explicit rows=%d/%d err=%v, want 0/1", legacy, kept, err)
	}
	hubfixture.RequireStatus(t, f.Request(t, member, http.MethodGet, f.Base, nil), http.StatusNotFound)
	role("member", "admin")
	hubfixture.RequireStatus(t, f.Request(t, member, http.MethodGet, f.Base, nil), http.StatusOK)
	command := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "promoted"}, Title: "promoted write", Body: "promoted" + hubfixture.IssueContractSections, State: "Todo"}
	hubfixture.RequireStatus(t, f.Request(t, member, http.MethodPost, f.Base+"/work-items", command), http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodDelete, hubfixture.ProjectBase(f.Primary, other.ID), map[string]string{"idempotency_key": "delete", "confirm_name": "explicit-project-sentinel"}), http.StatusOK)
	for _, user := range []hubfixture.HostedUser{owner, member} {
		hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, hubfixture.ProjectBase(f.Primary, other.ID), nil), http.StatusNotFound)
	}
}

func TestHostedProviderRoleBoundsRoleAccess(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	users := map[string]hubfixture.HostedUser{}
	for _, user := range []struct{ name, role string }{{"owner", "owner"}, {"admin", "admin"}, {"demoted", "owner"}, {"viewer", "admin"}, {"granted", "owner"}} {
		users[user.name] = f.User(t, user.name, user.role, user.name+"@example.test", "", "")
	}
	f.Grant(t, users["granted"], f.Project, false, false)
	if _, err := f.Admin.ExecContext(t.Context(), `WITH legacy AS (INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner) VALUES($1,'user_demoted',$2,true,true)) INSERT INTO token_grants(organization_id,token_id,project_id) SELECT $1,principal_id,$2 FROM hosted_members WHERE organization_id=$1 AND user_id='user_demoted'`, string(f.Primary), string(f.Project)); err != nil {
		t.Fatal(err)
	}
	provider := func(name, role string) {
		t.Helper()
		if err := f.Provider.SetMembershipRole(t.Context(), "membership_user_"+name, role); err != nil {
			t.Fatal(err)
		}
	}
	writes := 0
	check := func(name string, read, write int) {
		t.Helper()
		hubfixture.RequireStatus(t, f.Request(t, users[name], http.MethodGet, f.Base, nil), read)
		writes++
		command := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("write-%d", writes)}, Title: "role write", Body: "role write" + hubfixture.IssueContractSections, State: "Todo"}
		hubfixture.RequireStatus(t, f.Request(t, users[name], http.MethodPost, f.Base+"/work-items", command), write)
	}
	for _, name := range []string{"owner", "admin", "demoted", "viewer", "granted"} {
		check(name, http.StatusOK, http.StatusOK)
	}
	provider("demoted", "member")
	provider("viewer", "viewer")
	provider("granted", "member")
	check("demoted", http.StatusNotFound, http.StatusNotFound)
	check("viewer", http.StatusNotFound, http.StatusNotFound)
	check("granted", http.StatusOK, http.StatusForbidden)
	check("owner", http.StatusOK, http.StatusOK)
	check("admin", http.StatusOK, http.StatusOK)
	provider("granted", "viewer")
	check("granted", http.StatusOK, http.StatusForbidden)
	provider("demoted", "owner")
	provider("viewer", "admin")
	check("demoted", http.StatusOK, http.StatusOK)
	check("viewer", http.StatusOK, http.StatusOK)
	var stored int
	if err := f.Admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND user_id IN ('user_demoted','user_viewer','user_granted') AND role IN ('owner','admin')`, string(f.Primary)).Scan(&stored); err != nil || stored != 3 {
		t.Fatalf("stored administrator roles=%d err=%v, want 3 unchanged", stored, err)
	}
}
