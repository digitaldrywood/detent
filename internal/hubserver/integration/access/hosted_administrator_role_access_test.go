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
	members := map[tracker.OrganizationID][]string{f.Primary: {"owner", "admin", "member"}, second: {"owner", "admin", "member"}}
	for organization, roles := range members {
		if err := f.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []tracker.ProjectID{"prj_role_a", "prj_role_b"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Profile: "native"}, States: hubfixture.States()}); err != nil {
					return err
				}
			}
			for _, role := range roles {
				user, principal := string(organization)+"_"+role, string(organization)+"_principal_"+role
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
	for _, test := range []struct {
		organization, member tracker.OrganizationID
		role                 string
		want                 int
	}{
		{f.Primary, f.Primary, "owner", 3},
		{f.Primary, f.Primary, "admin", 3},
		{f.Primary, f.Primary, "member", 0},
		{second, second, "owner", 2},
		{second, second, "admin", 2},
		{second, second, "member", 0},
		{second, f.Primary, "admin", 0},
		{f.Primary, second, "owner", 0},
	} {
		user, principal := string(test.member)+"_"+test.role, string(test.member)+"_principal_"+test.role
		if err := f.Repository.ReadTenant(t.Context(), test.organization, corestore.Serving, func(r tenantstore.Reader) error {
			projects, err := r.(accessstore.ProjectAccessPageReader).ProjectAccessPage(t.Context(), accessstore.ProjectAccessFilter{UserID: user, PrincipalID: principal, CredentialID: principal, Limit: 10})
			if err != nil {
				return err
			}
			if len(projects) != test.want {
				return fmt.Errorf("%s %s in %s sees %d projects, want %d", test.member, test.role, test.organization, len(projects), test.want)
			}
			for _, project := range projects {
				if !project.CanWrite || !project.ManageRunner {
					return fmt.Errorf("%s %s project %s write/runner=%t/%t", test.member, test.role, project.ProjectID, project.CanWrite, project.ManageRunner)
				}
				grant, err := r.AccessProjectGrant(t.Context(), user, project.ProjectID)
				if err != nil || !grant.CanWrite || !grant.ManageRunner {
					return fmt.Errorf("%s %s project grant %+v err=%v", test.member, test.role, grant, err)
				}
				if _, err := r.AccessTokenGrant(t.Context(), principal, project.ProjectID); err != nil {
					return fmt.Errorf("%s %s principal grant err=%v", test.member, test.role, err)
				}
			}
			if test.want == 0 {
				if _, err := r.AccessProjectGrant(t.Context(), user, "prj_role_a"); !errors.Is(err, corestore.ErrNotFound) {
					return fmt.Errorf("%s %s in %s project grant err=%v, want not found", test.member, test.role, test.organization, err)
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
