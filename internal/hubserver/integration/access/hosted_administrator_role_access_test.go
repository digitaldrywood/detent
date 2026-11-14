package access_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
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
