package cloudentry

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func createTestAccessKey(t *testing.T, b *browser, context string, organizations []keyOrganization, serviceOrg string) accessKey {
	t.Helper()
	path := "/api/cloud/account/api-keys"
	if serviceOrg != "" {
		path = "/api/cloud/organizations/" + serviceOrg + "/service-keys"
	}
	input := map[string]any{"name": "Multi-org test", "permission": "write", "access_context": context, "organizations": organizations, "expires_days": 7}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := attachmentRequest(t, b, http.MethodPost, path, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if response.Code != http.StatusCreated {
		t.Fatalf("create key=%d %s", response.Code, response.Body.String())
	}
	var key accessKey
	if err := json.Unmarshal(response.Body.Bytes(), &key); err != nil {
		t.Fatal(err)
	}
	if key.Token == "" {
		t.Fatal("created key has no token")
	}
	return key
}

func TestAccessKeyCrossOrganizationMCP(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	key := createTestAccessKey(t, owner, "global", nil, "")
	client := newBrowser(t, f.service.Handler())
	call := entryMCPClient(t, client, key.Token, "2025-11-25", "/mcp")
	orgs, failed := call(operatortool.OrganizationList, map[string]any{})
	if failed || !strings.Contains(string(orgs), "org_alpha") || !strings.Contains(string(orgs), "org_beta") {
		t.Fatalf("organization_list=%s failed=%v", orgs, failed)
	}
	catalog, failed := call("tools/list", nil)
	if failed || !strings.Contains(string(catalog), `"organization_id"`) {
		t.Fatalf("catalog missing organization selector; failed=%v", failed)
	}
	for _, org := range []string{"org_alpha", "org_beta"} {
		raw, failed := call("list_projects", map[string]any{"organization_id": org})
		if failed {
			t.Fatalf("list projects %s=%s", org, raw)
		}
		var projects struct {
			Data struct {
				Projects []struct {
					ID string `json:"project_id"`
				} `json:"projects"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &projects); err != nil || len(projects.Data.Projects) == 0 {
			t.Fatalf("projects=%s err=%v", raw, err)
		}
		project := projects.Data.Projects[0].ID
		created, failed := call(operatortool.FileIssue, map[string]any{"organization_id": org, "project_id": project, "request_id": "cross-org-" + org, "title": "One key across organizations"})
		if failed || !strings.Contains(string(created), "One key across organizations") {
			t.Fatalf("write %s=%s failed=%v", org, created, failed)
		}
	}
	f.provider.mu.Lock()
	delete(f.provider.memberships, "om_user_alice_porg_beta")
	f.provider.mu.Unlock()
	denied, failed := call("list_projects", map[string]any{"organization_id": "org_beta"})
	if !failed || !strings.Contains(string(denied), "membership_denied") {
		t.Fatalf("removed member=%s failed=%v", denied, failed)
	}
	allowed, failed := call("list_projects", map[string]any{"organization_id": "org_alpha"})
	if failed || !strings.Contains(string(allowed), "Alpha secret project") {
		t.Fatalf("remaining org=%s failed=%v", allowed, failed)
	}
	orgs, failed = call(operatortool.OrganizationList, map[string]any{})
	if failed || strings.Contains(string(orgs), "org_beta") {
		t.Fatalf("org removal not visible=%s", orgs)
	}
}

func TestAccessKeyOrganizationControls(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	key := createTestAccessKey(t, owner, "global", nil, "")
	unused := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/organizations/org_beta/external-keys", nil, nil)
	if unused.Code != http.StatusOK || !strings.Contains(unused.Body.String(), key.ID) || !strings.Contains(unused.Body.String(), `"last_used_at":null`) {
		t.Fatalf("unused key visibility=%d %s", unused.Code, unused.Body.String())
	}
	client := newBrowser(t, f.service.Handler())
	call := entryMCPClient(t, client, key.Token, "2025-11-25", "/mcp")
	if result, failed := call("list_projects", map[string]any{"organization_id": "org_beta"}); failed {
		t.Fatalf("initial reach=%s", result)
	}
	response := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/organizations/org_beta/external-keys", nil, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), key.ID) || !strings.Contains(response.Body.String(), `"read_only":true`) || strings.Contains(response.Body.String(), key.Token) {
		t.Fatalf("org key visibility=%d %s", response.Code, response.Body.String())
	}
	change := attachmentRequest(t, owner, http.MethodPut, "/api/cloud/organizations/org_beta/key-policy", strings.NewReader(`{"allow_external_keys":false}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if change.Code != http.StatusOK {
		t.Fatalf("policy change=%d %s", change.Code, change.Body.String())
	}
	denied, failed := call("list_projects", map[string]any{"organization_id": "org_beta"})
	if !failed || !strings.Contains(string(denied), "organization_key_policy_denied") {
		t.Fatalf("policy refusal=%s failed=%v", denied, failed)
	}
	if result, failed := call("list_projects", map[string]any{"organization_id": "org_alpha"}); failed {
		t.Fatalf("policy affected other org=%s", result)
	}
	change = attachmentRequest(t, owner, http.MethodPut, "/api/cloud/organizations/org_beta/key-policy", strings.NewReader(`{"allow_external_keys":true}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if change.Code != http.StatusOK {
		t.Fatal(change.Body.String())
	}
	change = attachmentRequest(t, owner, http.MethodPut, "/api/cloud/organizations/org_beta/key-policy", strings.NewReader(`{"personal_keys":"approval"}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if change.Code != http.StatusOK {
		t.Fatalf("approval policy=%d %s", change.Code, change.Body.String())
	}
	if result, failed := call("list_projects", map[string]any{"organization_id": "org_beta"}); !failed || !strings.Contains(string(result), "organization_key_policy_denied") {
		t.Fatalf("unapproved key=%s failed=%v", result, failed)
	}
	queue := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/organizations/org_beta/external-keys", nil, nil)
	if queue.Code != http.StatusOK || !strings.Contains(queue.Body.String(), `"status":"pending"`) {
		t.Fatalf("approval queue=%d %s", queue.Code, queue.Body.String())
	}
	approved := attachmentRequest(t, owner, http.MethodPut, "/api/cloud/organizations/org_beta/external-keys/"+key.ID+"/approve", strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if approved.Code != http.StatusNoContent {
		t.Fatalf("approve=%d %s", approved.Code, approved.Body.String())
	}
	if result, failed := call("list_projects", map[string]any{"organization_id": "org_beta"}); failed {
		t.Fatalf("approved key=%s", result)
	}
	blocked := attachmentRequest(t, owner, http.MethodPut, "/api/cloud/organizations/org_beta/external-keys/"+key.ID+"/block", strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if blocked.Code != http.StatusNoContent {
		t.Fatalf("block=%d %s", blocked.Code, blocked.Body.String())
	}
	denied, failed = call("list_projects", map[string]any{"organization_id": "org_beta"})
	if !failed || !strings.Contains(string(denied), "organization_key_blocked") {
		t.Fatalf("block refusal=%s failed=%v", denied, failed)
	}
	if result, failed := call("list_projects", map[string]any{"organization_id": "org_alpha"}); failed {
		t.Fatalf("block affected other org=%s", result)
	}
	db, err := sql.Open("sqlite", f.fixtures["org_beta"].path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, event := range []string{"key_call", "organization_key_policy_denied", "organization_key_blocked", "personal_key_blocked"} {
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE event=? AND mutation_json LIKE ?", event, "%"+key.ID+"%").Scan(&count); err != nil || count == 0 {
			t.Fatalf("missing target audit %s count=%d err=%v", event, count, err)
		}
	}
	notices := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/account/key-notifications", nil, nil)
	if notices.Code != http.StatusOK || !strings.Contains(notices.Body.String(), key.ID) || !strings.Contains(notices.Body.String(), "org_beta") {
		t.Fatalf("owner notification=%d %s", notices.Code, notices.Body.String())
	}
}

func TestAccessKeyScopeAndServiceLifetime(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	global := createTestAccessKey(t, owner, "global", nil, "")
	client := newBrowser(t, f.service.Handler())
	call := entryMCPClient(t, client, global.Token, "2025-11-25", "/mcp")
	raw, failed := call("list_projects", map[string]any{"organization_id": "org_alpha"})
	if failed {
		t.Fatal(string(raw))
	}
	var projects struct {
		Data struct {
			Projects []struct {
				ID string `json:"project_id"`
			} `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &projects); err != nil || len(projects.Data.Projects) == 0 {
		t.Fatalf("projects=%s err=%v", raw, err)
	}
	project := projects.Data.Projects[0].ID
	for _, context := range []string{"project", "selected"} {
		access := "project"
		if context == "selected" {
			access = "selected"
		}
		key := createTestAccessKey(t, owner, context, []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: access, Projects: []string{project}}}}, "")
		scoped := entryMCPClient(t, client, key.Token, "2025-11-25", "/mcp")
		denied, failed := scoped(operatortool.WorkList, map[string]any{"organization_id": "org_alpha", "project_id": "prj_foreign"})
		if !failed || !strings.Contains(string(denied), "key_project_denied") {
			t.Fatalf("%s project refusal=%s failed=%v", context, denied, failed)
		}
		denied, failed = scoped("list_projects", map[string]any{"organization_id": "org_beta"})
		if !failed || !strings.Contains(string(denied), "key_organization_denied") {
			t.Fatalf("%s org refusal=%s failed=%v", context, denied, failed)
		}
		allowed, failed := scoped(operatortool.WorkList, map[string]any{"organization_id": "org_alpha", "project_id": project})
		if failed {
			t.Fatalf("%s allowed read=%s", context, allowed)
		}
	}
	service := createTestAccessKey(t, owner, "selected", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}, "org_alpha")
	serviceCall := entryMCPClient(t, client, service.Token, "2025-11-25", "/mcp")
	f.provider.mu.Lock()
	delete(f.provider.memberships, "om_user_alice_porg_alpha")
	f.provider.mu.Unlock()
	listed, listFailed := serviceCall("list_projects", map[string]any{"organization_id": "org_alpha"})
	if listFailed || !strings.Contains(string(listed), project) {
		t.Fatalf("service discovery after removal=%s failed=%v", listed, listFailed)
	}
	created, failed := serviceCall(operatortool.FileIssue, map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": "service-after-removal", "title": "Service key survives creator departure"})
	if failed || !strings.Contains(string(created), "Service key survives creator departure") {
		t.Fatalf("service write=%s failed=%v", created, failed)
	}
}

func TestAccessKeyMembershipAndPolicyMatrix(t *testing.T) {
	f := newEntryFixture(t)
	org, err := f.service.readyOrganization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"personal", "service"} {
		for _, access := range []string{"global", "selected", "project"} {
			for _, role := range []string{"owner", "admin", "member", "viewer"} {
				for _, member := range []bool{true, false} {
					for _, allowed := range []bool{true, false} {
						name := fmt.Sprintf("%s/%s/%s/member=%v/policy=%v", kind, access, role, member, allowed)
						t.Run(name, func(t *testing.T) {
							owner := strings.ReplaceAll(name, "/", "_")
							if member {
								f.provider.member(owner, "porg_alpha", role)
							}
							if _, err := f.service.auth.store.db.ExecContext(t.Context(), "INSERT INTO organization_key_policy(organization_id,personal_keys) VALUES('org_alpha',CASE WHEN ? THEN 'allowed' ELSE 'blocked' END) ON CONFLICT(organization_id) DO UPDATE SET personal_keys=excluded.personal_keys", allowed); err != nil {
								t.Fatal(err)
							}
							k := accessKey{ID: owner, Owner: owner, Name: "matrix", Kind: kind, Permission: apikey.ScopeWrite, AccessContext: access}
							if access == "selected" {
								k.Organizations = []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}, {OrganizationID: "org_beta", ProjectContext: apikey.ProjectContext{Access: "selected", Projects: []string{"prj_beta"}}}}
							}
							if access == "project" {
								k.Organizations = []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{"prj_alpha"}}}}
							}
							if kind == "service" {
								k.ServiceOrganization, k.AccessContext = "org_alpha", "selected"
								k.Organizations = []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}
							}
							claims, err := f.service.keyClaims(t.Context(), k, org, http.MethodGet, "/api/v2/organizations/org_alpha/projects", nil)
							want := ""
							if kind == "personal" && !member {
								want = "membership_denied"
							} else if kind == "personal" && !allowed {
								want = "organization_key_policy_denied"
							}
							var refusal *apikey.Refusal
							if want != "" {
								if !errors.As(err, &refusal) || refusal.Code != want {
									t.Fatalf("authorization=%v want=%s", err, want)
								}
								return
							}
							if err != nil {
								t.Fatal(err)
							}
							wantRole := role
							if kind == "service" {
								wantRole = "admin"
							}
							if claims.Role != wantRole || claims.Key.Permission != apikey.ScopeWrite {
								t.Fatalf("wrong authority: role=%s permission=%s", claims.Role, claims.Key.Permission)
							}
						})
					}
				}
			}
		}
	}
}

func TestAccessKeyLifecycleAndREST(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	key := createTestAccessKey(t, owner, "global", nil, "")
	client := newBrowser(t, f.service.Handler())
	headers := map[string]string{"Authorization": "Bearer " + key.Token, "X-Detent-Organization": "org_alpha"}
	response := attachmentRequest(t, client, http.MethodGet, "/api/v2/projects", nil, headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Alpha secret project") || strings.Contains(response.Body.String(), "Beta secret project") {
		t.Fatalf("REST target=%d %s", response.Code, response.Body.String())
	}
	call := entryMCPClient(t, client, key.Token, "2025-11-25", "/organizations/org_alpha/mcp")
	if raw, failed := call("list_projects", map[string]any{}); failed || !strings.Contains(string(raw), "Alpha secret project") {
		t.Fatalf("per-org endpoint=%s failed=%v", raw, failed)
	}
	sessionOnly := attachmentRequest(t, client, http.MethodGet, "/api/v2/members", nil, headers)
	if sessionOnly.Code != http.StatusForbidden || !strings.Contains(sessionOnly.Body.String(), "session_required") {
		t.Fatalf("key entered browser settings=%d %s", sessionOnly.Code, sessionOnly.Body.String())
	}
	listed := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/account/api-keys", nil, nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), key.ID) || strings.Contains(listed.Body.String(), key.Token) {
		t.Fatalf("account list=%d %s", listed.Code, listed.Body.String())
	}
	rotated := attachmentRequest(t, owner, http.MethodPost, "/api/cloud/account/api-keys/"+key.ID+"/rotate", strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	var replacement accessKey
	if rotated.Code != http.StatusCreated || json.Unmarshal(rotated.Body.Bytes(), &replacement) != nil || replacement.ID != key.ID || replacement.Token == key.Token {
		t.Fatalf("rotate=%d %s", rotated.Code, rotated.Body.String())
	}
	response = attachmentRequest(t, client, http.MethodGet, "/api/v2/projects", nil, headers)
	if response.Code != http.StatusForbidden {
		t.Fatalf("old token still accepted=%d", response.Code)
	}
	headers["Authorization"] = "Bearer " + replacement.Token
	response = attachmentRequest(t, client, http.MethodGet, "/api/v2/projects", nil, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("replacement token=%d %s", response.Code, response.Body.String())
	}
	revoked := attachmentRequest(t, owner, http.MethodDelete, "/api/cloud/account/api-keys/"+key.ID, nil, nil)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke=%d %s", revoked.Code, revoked.Body.String())
	}
	response = attachmentRequest(t, client, http.MethodGet, "/api/v2/projects", nil, headers)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "key_inactive") {
		t.Fatalf("revoked key=%d %s", response.Code, response.Body.String())
	}
	expiring := createTestAccessKey(t, owner, "global", nil, "")
	if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE access_keys SET expires_at=? WHERE id=?", formatTime(time.Now().Add(-time.Second)), expiring.ID); err != nil {
		t.Fatal(err)
	}
	headers["Authorization"] = "Bearer " + expiring.Token
	response = attachmentRequest(t, client, http.MethodGet, "/api/v2/projects", nil, headers)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "key_inactive") {
		t.Fatalf("expired key=%d %s", response.Code, response.Body.String())
	}
}

func TestAccessKeyMemberSettingsIsolation(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	other := createTestAccessKey(t, owner, "global", nil, "")
	member := newBrowser(t, f.service.Handler())
	member.login("/organizations", "user_bob:")
	own := createTestAccessKey(t, member, "global", nil, "")
	listed := attachmentRequest(t, member, http.MethodGet, "/api/cloud/account/api-keys", nil, nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), own.ID) || strings.Contains(listed.Body.String(), other.ID) {
		t.Fatalf("member keys=%d %s", listed.Code, listed.Body.String())
	}
	for _, test := range []struct{ method, path, body string }{
		{http.MethodGet, "external-keys", ""}, {http.MethodGet, "key-policy", ""}, {http.MethodPut, "key-policy", `{"personal_keys":"blocked"}`},
		{http.MethodPut, "external-keys/" + other.ID + "/block", `{}`}, {http.MethodPut, "external-keys/" + other.ID + "/approve", `{}`},
		{http.MethodPost, "service-keys", `{}`}, {http.MethodGet, "service-keys", ""},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			response := attachmentRequest(t, member, test.method, "/api/cloud/organizations/org_beta/"+test.path, strings.NewReader(test.body), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
			if response.Code != http.StatusForbidden {
				t.Fatalf("non-admin access=%d %s", response.Code, response.Body.String())
			}
		})
	}
	for _, test := range []struct{ method, path string }{{http.MethodPost, "/rotate"}, {http.MethodDelete, ""}} {
		response := attachmentRequest(t, member, test.method, "/api/cloud/account/api-keys/"+other.ID+test.path, strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
		if response.Code != http.StatusNotFound {
			t.Fatalf("other owner's key mutation=%d", response.Code)
		}
	}
}
