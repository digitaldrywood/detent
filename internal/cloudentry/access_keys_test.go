package cloudentry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/mcp"
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
	for id, tenant := range f.fixtures {
		if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed=1 WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
		organization, err := f.service.registry.Organization(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		tenant.config.Hosted.SharedEntry.ResolveKey = func(ctx context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return f.service.resolveTenantKey(ctx, organization, origin)
		}
	}
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
		approveEntryFixturePolicy(t, owner, org, project)
		createdConversation, failed := call("create_conversation", map[string]any{"organization_id": org, "project_id": project, "request_id": "conversation-" + org, "input": map[string]any{"title": "Command results"}})
		var conversationResult struct {
			Data struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			} `json:"data"`
		}
		if failed || json.Unmarshal(createdConversation, &conversationResult) != nil || conversationResult.Data.Conversation.ID == "" {
			t.Fatalf("create conversation=%s failed=%v", createdConversation, failed)
		}
		command := map[string]any{"organization_id": org, "project_id": project, "conversation_id": conversationResult.Data.Conversation.ID, "request_id": "command-" + org, "input": map[string]any{"kind": "message", "text": "A single message"}}
		type commandResult struct {
			ID     string          `json:"action_id"`
			Status string          `json:"status"`
			Data   json.RawMessage `json:"data"`
		}
		var actionID string
		var originalData json.RawMessage
		reconnected := entryMCPClient(t, client, key.Token, "2025-11-25", "/mcp")
		for _, current := range []struct {
			name string
			call func(string, any) (json.RawMessage, bool)
		}{{"submit", call}, {"same-session replay", call}, {"new-session replay", reconnected}} {
			if current.name == "new-session replay" {
				result, failed := current.call(operatortool.ActionResult, map[string]any{"organization_id": org, "action_id": actionID})
				if !failed || !strings.Contains(string(result), `"not_found"`) {
					t.Fatalf("unreplayed connection read=%s failed=%v", result, failed)
				}
			}
			raw, failed := current.call("post_conversation_command", command)
			var receipt commandResult
			if failed || json.Unmarshal(raw, &receipt) != nil || receipt.ID == "" || receipt.Status != "succeeded" {
				t.Fatalf("%s command=%s failed=%v", current.name, raw, failed)
			}
			if actionID != "" && receipt.ID != actionID {
				t.Fatalf("replay changed action: %s != %s", receipt.ID, actionID)
			}
			actionID = receipt.ID
			if originalData != nil && !bytes.Equal(receipt.Data, originalData) {
				t.Fatalf("%s changed command receipt: %s != %s", current.name, receipt.Data, originalData)
			}
			originalData = receipt.Data
			result, failed := current.call(operatortool.ActionResult, map[string]any{"organization_id": org, "action_id": actionID})
			var read commandResult
			if failed || json.Unmarshal(result, &read) != nil || read.ID != actionID || read.Status != "succeeded" || !bytes.Equal(read.Data, receipt.Data) {
				t.Fatalf("action result=%s want=%s failed=%v", result, raw, failed)
			}
		}
		otherKey := createTestAccessKey(t, owner, "global", nil, "")
		other := entryMCPClient(t, client, otherKey.Token, "2025-11-25", "/mcp")
		denied, failed := other(operatortool.ActionResult, map[string]any{"organization_id": org, "action_id": actionID})
		if !failed || !strings.Contains(string(denied), `"not_found"`) {
			t.Fatalf("other key read=%s failed=%v", denied, failed)
		}
		otherOrg := "org_beta"
		if org == otherOrg {
			otherOrg = "org_alpha"
		}
		denied, failed = call(operatortool.ActionResult, map[string]any{"organization_id": otherOrg, "action_id": actionID})
		if !failed || !strings.Contains(string(denied), `"not_found"`) {
			t.Fatalf("other organization read=%s failed=%v", denied, failed)
		}
		for _, test := range []struct {
			name string
			args map[string]any
			code string
		}{
			{"missing action", map[string]any{"organization_id": org, "action_id": "missing"}, "not_found"},
			{"missing id", map[string]any{"organization_id": org}, "invalid_request"},
			{"wrong id type", map[string]any{"organization_id": org, "action_id": 1}, "invalid_request"},
			{"unknown field", map[string]any{"organization_id": org, "action_id": actionID, "approve": true}, "invalid_request"},
		} {
			result, failed := call(operatortool.ActionResult, test.args)
			if !failed || !strings.Contains(string(result), `"`+test.code+`"`) {
				t.Fatalf("%s=%s failed=%v", test.name, result, failed)
			}
		}
		messages, failed := call("list_conversation_messages", map[string]any{"organization_id": org, "project_id": project, "conversation_id": conversationResult.Data.Conversation.ID})
		var history struct {
			Data struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			} `json:"data"`
		}
		if failed || json.Unmarshal(messages, &history) != nil {
			t.Fatalf("conversation messages=%s failed=%v", messages, failed)
		}
		userMessages := 0
		for _, message := range history.Data.Messages {
			if message.Role == "user" {
				userMessages++
			}
		}
		if userMessages != 1 {
			t.Fatalf("replay or result read executed command again: %s", messages)
		}
		created, failed := call(operatortool.FileIssue, map[string]any{"organization_id": org, "project_id": project, "request_id": "cross-org-" + org, "title": "One key across organizations", "description": issueContractTestSections})
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

func TestOrganizationKeyDiscoveryPagination(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	key := createTestAccessKey(t, owner, "global", nil, "")
	db := f.service.auth.store.db
	for i := range 55 {
		id := fmt.Sprintf("key_page_%03d", i)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO access_keys SELECT ?,?,name,kind,owner_subject,owner_email,permission,access_context,organizations_json,service_organization_id,created_at,expires_at,revoked_at FROM access_keys WHERE id=?`, id, id, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE access_keys SET access_context='project',organizations_json='[{"organization_id":"org_beta","project_access":"project","project_ids":["prj_absent"]}]' WHERE id='key_page_000'`); err != nil {
		t.Fatal(err)
	}
	transport := f.service.config.transport
	serviceCalls := 0
	f.service.config.transport = func(org Organization) (http.RoundTripper, error) {
		serviceCalls++
		return transport(org)
	}
	for _, test := range []struct {
		name, owner, access, organizations string
	}{
		{"unrelated owner", "user_outside", "global", "not-json"},
		{"other organization", key.Owner, "selected", `[{"organization_id":"org_alpha","project_access":"all","project_ids":[]}]`},
	} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO access_keys SELECT ?,?,name,kind,?,?,permission,?,?,service_organization_id,created_at,expires_at,revoked_at FROM access_keys WHERE id=?`, test.name, test.name, test.owner, key.OwnerEmail, test.access, test.organizations, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for page, want := range []int{50, 6} {
		serviceCalls = 0
		f.provider.mu.Lock()
		before := f.provider.membershipLists
		f.provider.mu.Unlock()
		response := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/organizations/org_beta/external-keys?cursor="+cursor, nil, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("page %d=%d %s", page, response.Code, response.Body.String())
		}
		var listed struct {
			Keys       []keyView `json:"keys"`
			NextCursor string    `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		if len(listed.Keys) != want {
			t.Fatalf("page %d keys=%d want=%d", page, len(listed.Keys), want)
		}
		for _, item := range listed.Keys {
			if seen[item.ID] || len(item.EffectiveReach) != 1 || item.EffectiveReach[0].OrganizationID != "org_beta" {
				t.Fatalf("duplicate or incorrect reach: %+v", item)
			}
			if (len(item.EffectiveReach[0].Projects) == 0) != (item.ID == "key_page_000") {
				t.Fatalf("owner reach changed by another key scope: %+v", item)
			}
			seen[item.ID] = true
		}
		f.provider.mu.Lock()
		lookups := f.provider.membershipLists - before
		f.provider.mu.Unlock()
		if lookups > 3 {
			t.Fatalf("membership lookups=%d for one owner", lookups)
		}
		if serviceCalls > 2 {
			t.Fatalf("service calls=%d for one owner", serviceCalls)
		}
		cursor = listed.NextCursor
		if (cursor != "") != (page == 0) {
			t.Fatalf("page %d cursor=%q", page, cursor)
		}
	}
	if !seen[key.ID] {
		t.Fatal("unused global key missing")
	}
	for _, test := range []struct {
		name        string
		removeBeta  bool
		memberships int
		services    int
	}{
		{name: "account list", memberships: 3, services: 2},
		{name: "account list after membership removal", removeBeta: true, memberships: 2, services: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			serviceCalls = 0
			f.provider.mu.Lock()
			if test.removeBeta {
				delete(f.provider.memberships, "om_user_alice_porg_beta")
			}
			before := f.provider.membershipLists
			f.provider.mu.Unlock()
			response := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/account/api-keys", nil, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("account list=%d %s", response.Code, response.Body.String())
			}
			var listed struct {
				Keys []keyView `json:"keys"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Keys) != 57 {
				t.Fatalf("account keys=%d want=57", len(listed.Keys))
			}
			for _, item := range listed.Keys {
				want := 1
				if item.ID == "key_page_000" {
					want = 0
				}
				if !test.removeBeta && item.ID != "other organization" {
					want++
				}
				if item.Owner != key.Owner || len(item.EffectiveReach) != want {
					t.Fatalf("incorrect account reach: %+v", item)
				}
				for _, reach := range item.EffectiveReach {
					if test.removeBeta && reach.OrganizationID == "org_beta" || (len(reach.Projects) == 0) != (item.ID == "key_page_000") {
						t.Fatalf("stale or scope-contaminated account reach: %+v", item)
					}
				}
			}
			f.provider.mu.Lock()
			lookups := f.provider.membershipLists - before
			f.provider.mu.Unlock()
			if lookups != test.memberships || serviceCalls != test.services {
				t.Fatalf("account membership lookups=%d service calls=%d, want %d and %d", lookups, serviceCalls, test.memberships, test.services)
			}
		})
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
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE event=? AND mutation_json LIKE ?", event, "%"+key.ID+"%").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if event == "key_call" {
			if count != 0 {
				t.Fatalf("successful reads wrote %d target audit rows", count)
			}
		} else if count == 0 {
			t.Fatalf("missing target audit %s", event)
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
	approveEntryFixturePolicy(t, owner, "org_alpha", project)
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
	response := attachmentRequest(t, owner, http.MethodGet, "/api/cloud/organizations/org_alpha/service-keys", nil, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), service.ID) || strings.Contains(response.Body.String(), service.Token) {
		t.Fatalf("service key list=%d %s", response.Code, response.Body.String())
	}
	f.provider.mu.Lock()
	membership := f.provider.memberships["om_user_alice_porg_alpha"]
	delete(f.provider.memberships, "om_user_alice_porg_alpha")
	f.provider.mu.Unlock()
	listed, listFailed := serviceCall("list_projects", map[string]any{"organization_id": "org_alpha"})
	if listFailed || !strings.Contains(string(listed), project) {
		t.Fatalf("service discovery after removal=%s failed=%v", listed, listFailed)
	}
	created, failed := serviceCall(operatortool.FileIssue, map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": "service-after-removal", "title": "Service key survives creator departure", "description": issueContractTestSections})
	if failed || !strings.Contains(string(created), "Service key survives creator departure") {
		t.Fatalf("service write=%s failed=%v", created, failed)
	}
	f.provider.mu.Lock()
	f.provider.memberships[membership.ID] = membership
	f.provider.mu.Unlock()
	response = attachmentRequest(t, owner, http.MethodDelete, "/api/cloud/organizations/org_alpha/service-keys/"+service.ID, nil, map[string]string{"Origin": testPublicURL})
	if response.Code != http.StatusNoContent {
		t.Fatalf("service key revoke=%d %s", response.Code, response.Body.String())
	}
	if result, failed := serviceCall("list_projects", map[string]any{"organization_id": "org_alpha"}); !failed || !strings.Contains(string(result), "key_inactive") {
		t.Fatalf("revoked service key=%s failed=%v", result, failed)
	}
}

func TestKeyTenantCallAssertionLifetimeAfterUsage(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	key := createTestAccessKey(t, owner, "selected", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}, "org_alpha")
	organization, err := f.service.registry.Organization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		delay      time.Duration
		expires    time.Duration
		wantExpiry time.Duration
		wantError  bool
	}{
		{name: "immediate delivery", wantExpiry: 20 * time.Second},
		{name: "usage recording outlasts assertion", delay: 21 * time.Second, wantExpiry: 41 * time.Second},
		{name: "key expiry caps assertion", delay: 21 * time.Second, expires: 30 * time.Second, wantExpiry: 30 * time.Second},
		{name: "key expires during usage recording", delay: 21 * time.Second, expires: 20 * time.Second, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := start
			sent := 0
			if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE access_key_organizations SET last_used_at=NULL WHERE key_id=? AND organization_id=?", key.ID, organization.ID); err != nil {
				t.Fatal(err)
			}
			service := &Service{config: f.service.config, auth: f.service.auth}
			service.config.now = func() time.Time {
				var used sql.NullString
				if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT last_used_at FROM access_key_organizations WHERE key_id=? AND organization_id=?", key.ID, organization.ID).Scan(&used); err != nil && !errors.Is(err, sql.ErrNoRows) {
					t.Fatal(err)
				}
				if used.Valid {
					now = start.Add(test.delay)
				}
				return now
			}
			credential := key
			credential.ExpiresAt = nil
			if test.expires != 0 {
				expires := start.Add(test.expires)
				credential.ExpiresAt = &expires
			}
			service.config.transport = func(Organization) (http.RoundTripper, error) {
				return runtimeTransport(func(request *http.Request) (*http.Response, error) {
					sent++
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					verifier := &cloudassert.Verifier{PublicKeys: f.fixtures["org_alpha"].config.Hosted.SharedEntry.PublicKeys, Issuer: "entry", Audience: "org_alpha", Generation: 1, Now: service.config.now}
					claims, err := verifier.Verify(request.Header.Get(cloudassert.Header), request.Method, request.URL.RequestURI(), body)
					if err != nil {
						return nil, err
					}
					if !claims.IssuedAt.Equal(now) || !claims.ExpiresAt.Equal(start.Add(test.wantExpiry)) || claims.Key.ID != key.ID {
						t.Fatalf("assertion issued=%s expires=%s key=%s", claims.IssuedAt, claims.ExpiresAt, claims.Key.ID)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}), nil
			}
			_, err := service.keyTenantCall(t.Context(), credential, organization, "/internal/v1/keys/call", map[string]string{"name": operatortool.AddComment})
			if test.wantError {
				if !errors.Is(err, cloudassert.ErrInvalid) || sent != 0 {
					t.Fatalf("expired key call error=%v sent=%d", err, sent)
				}
			} else if err != nil || sent != 1 {
				t.Fatalf("key call error=%v sent=%d", err, sent)
			}
		})
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
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed=1 WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	allocation, err := f.service.registry.Organization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	originFor := func(key accessKey) entryauthority.Origin {
		stored, err := f.service.activeAccessKey(t.Context(), apikey.HashToken(key.Token))
		if err != nil {
			t.Fatal(err)
		}
		claims, err := f.service.keyClaims(t.Context(), stored, allocation, http.MethodPost, "/internal/v1/keys/call", nil)
		if err != nil {
			t.Fatal(err)
		}
		return entryauthority.Origin{KeyID: stored.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation}
	}
	origin := originFor(key)
	if _, err := f.service.resolveTenantKey(t.Context(), allocation, origin); err != nil {
		t.Fatalf("current managed authority: %v", err)
	}
	for _, changed := range []entryauthority.Origin{
		{KeyID: origin.KeyID, Subject: origin.Subject, Issuer: origin.Issuer, Generation: origin.Generation},
		{KeyID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Subject: "other_subject", Issuer: origin.Issuer, Generation: origin.Generation},
		{KeyID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Subject: origin.Subject, Issuer: "other_entry", Generation: origin.Generation},
		{KeyID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Subject: origin.Subject, Issuer: origin.Issuer, Generation: origin.Generation + 1},
	} {
		if _, err := f.service.resolveTenantKey(t.Context(), allocation, changed); err == nil {
			t.Fatal("changed origin accepted")
		}
	}
	other, err := f.service.registry.Organization(t.Context(), "org_beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.resolveTenantKey(t.Context(), other, origin); err == nil {
		t.Fatal("independent tenant accepted managed authority")
	}

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
	if _, err := f.service.resolveTenantKey(t.Context(), allocation, origin); err == nil {
		t.Fatal("rotated credential restored async authority")
	}
	origin = originFor(replacement)
	if _, err := f.service.resolveTenantKey(t.Context(), allocation, origin); err != nil {
		t.Fatalf("replacement authority: %v", err)
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
	if _, err := f.service.resolveTenantKey(t.Context(), allocation, origin); err == nil {
		t.Fatal("revoked credential restored async authority")
	}

	response = attachmentRequest(t, client, http.MethodGet, "/api/v2/projects", nil, headers)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "key_inactive") {
		t.Fatalf("revoked key=%d %s", response.Code, response.Body.String())
	}
	expiring := createTestAccessKey(t, owner, "global", nil, "")
	expiredOrigin := originFor(expiring)
	if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE access_keys SET expires_at=? WHERE id=?", formatTime(time.Now().Add(-time.Second)), expiring.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.resolveTenantKey(t.Context(), allocation, expiredOrigin); err == nil {
		t.Fatal("expired key restored async authority")
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
		{http.MethodPost, "service-keys", `{}`}, {http.MethodGet, "service-keys", ""}, {http.MethodDelete, "service-keys/" + other.ID, ""},
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

func TestAccessKeyMCPPreservesMoveItemRequestErrors(t *testing.T) {
	f := newEntryFixture(t)
	const org = "org_alpha"
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed=1 WHERE id=?", org); err != nil {
		t.Fatal(err)
	}
	organization, err := f.service.registry.Organization(t.Context(), org)
	if err != nil {
		t.Fatal(err)
	}
	f.fixtures[org].config.Hosted.SharedEntry.ResolveKey = func(ctx context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
		return f.service.resolveTenantKey(ctx, organization, origin)
	}
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	key := createTestAccessKey(t, owner, "global", nil, "")
	client := newBrowser(t, f.service.Handler())
	call := func(name string, arguments map[string]any) (json.RawMessage, json.RawMessage, bool) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "move-errors", "version": "1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		response := attachmentRequest(t, client, http.MethodPost, "/mcp", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key.Token, "Mcp-Protocol-Version": mcp.ProtocolVersion, "Mcp-Method": "tools/call", "Mcp-Name": name})
		var frame struct {
			Result struct {
				IsError    bool            `json:"isError"`
				Structured json.RawMessage `json:"structuredContent"`
				Content    []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &frame) != nil || len(frame.Result.Content) != 1 {
			t.Fatalf("%s response=%d %s", name, response.Code, response.Body.String())
		}
		return json.RawMessage(frame.Result.Content[0].Text), frame.Result.Structured, frame.Result.IsError
	}
	listed, _, failed := call("list_projects", map[string]any{"organization_id": org})
	var projects struct {
		Data struct {
			Projects []struct {
				ID string `json:"project_id"`
			} `json:"projects"`
		} `json:"data"`
	}
	if failed || json.Unmarshal(listed, &projects) != nil || len(projects.Data.Projects) == 0 {
		t.Fatalf("projects=%s failed=%v", listed, failed)
	}
	project := projects.Data.Projects[0].ID
	approveEntryFixturePolicy(t, owner, org, project)
	created, _, failed := call(operatortool.FileIssue, map[string]any{"organization_id": org, "project_id": project, "request_id": "move-errors-create", "title": "Move error reporting", "description": issueContractTestSections})
	var issue struct {
		Data struct {
			Number   int    `json:"number"`
			Revision string `json:"revision"`
		} `json:"data"`
	}
	if failed || json.Unmarshal(created, &issue) != nil || issue.Data.Number == 0 || issue.Data.Revision == "" {
		t.Fatalf("file issue=%s failed=%v", created, failed)
	}
	for _, test := range []struct {
		name, code, message string
		arguments           map[string]any
	}{
		{"numeric revision", "invalid_request", "expected_revision: must be a string", map[string]any{"target_state": "Done", "expected_revision": 1}},
		{"graph", "transition_not_allowed", "Workflow transition is not allowed", map[string]any{"target_state": "Missing lane", "expected_revision": issue.Data.Revision}},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := map[string]any{"organization_id": org, "project_id": project, "identifier": strconv.Itoa(issue.Data.Number), "request_id": "move-errors-" + strings.ReplaceAll(test.name, " ", "-")}
			for name, value := range test.arguments {
				arguments[name] = value
			}
			text, structured, failed := call(operatortool.MoveItem, arguments)
			var got operatortool.RequestError
			if !failed || len(structured) == 0 || json.Unmarshal(structured, &got) != nil {
				t.Fatalf("move text=%s structured=%s failed=%v", text, structured, failed)
			}
			if got.Code != test.code || got.Message != test.message {
				t.Fatalf("move error=%+v want code=%q message=%q", got, test.code, test.message)
			}
		})
	}
}

func TestAccessKeyMutationAuditResponses(t *testing.T) {
	for _, transport := range []string{"HTTP", "MCP"} {
		for _, failure := range []string{"none", "post-handler audit", "pre-handler audit", "service"} {
			if transport == "HTTP" && failure == "service" {
				continue
			}
			t.Run(transport+"/"+failure, func(t *testing.T) {
				f := newEntryFixture(t)
				const org = "org_alpha"
				if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed=1 WHERE id=?", org); err != nil {
					t.Fatal(err)
				}
				organization, err := f.service.registry.Organization(t.Context(), org)
				if err != nil {
					t.Fatal(err)
				}
				f.fixtures[org].config.Hosted.SharedEntry.ResolveKey = func(ctx context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
					return f.service.resolveTenantKey(ctx, organization, origin)
				}
				owner := newBrowser(t, f.service.Handler())
				owner.login("/organizations", "user_alice:")
				client := newBrowser(t, f.service.Handler())
				db, err := sql.Open("sqlite", storeDSN(f.fixtures[org].path))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var project string
				if err := db.QueryRowContext(t.Context(), "SELECT id FROM projects WHERE profile='native'").Scan(&project); err != nil {
					t.Fatal(err)
				}
				key := createTestAccessKey(t, owner, "selected", []keyOrganization{{OrganizationID: org, ProjectContext: apikey.ProjectContext{Access: "selected", Projects: []string{project}}}}, "")
				call := entryMCPClient(t, client, key.Token, mcp.ProtocolVersion, "/mcp")
				approveEntryFixturePolicy(t, owner, org, project)
				if _, err := db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=0 WHERE id=?", project); err != nil {
					t.Fatal(err)
				}
				var items [2]string
				for i := range items {
					created, failed := call(operatortool.FileIssue, map[string]any{"organization_id": org, "project_id": project, "request_id": fmt.Sprintf("audit-seed-%d", i), "title": fmt.Sprintf("Audit seed %d", i), "description": issueContractTestSections})
					var result struct {
						Data struct {
							ID string `json:"work_item_id"`
						} `json:"data"`
					}
					if failed || json.Unmarshal(created, &result) != nil || result.Data.ID == "" {
						t.Fatalf("seed creation=%s failed=%v", created, failed)
					}
					items[i] = result.Data.ID
				}
				switch failure {
				case "post-handler audit":
					_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_key_audit BEFORE UPDATE ON hosted_audit WHEN NEW.event='key_call' AND OLD.status=200 AND NEW.status=200 BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
				case "pre-handler audit":
					_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_key_audit BEFORE INSERT ON hosted_audit WHEN NEW.event='key_call' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
				case "service":
					f.service.config.transport = func(Organization) (http.RoundTripper, error) {
						return nil, errors.New("backend failure secret sentinel")
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				base := "/api/v2/organizations/" + org + "/projects/" + project + "/work-items"
				if failure != "service" {
					for _, name := range []string{operatortool.WorkList, operatortool.WorkItem} {
						arguments := map[string]any{"organization_id": org, "project_id": project}
						if name == operatortool.WorkItem {
							arguments["reference"] = items[0]
						}
						result, failed := call(name, arguments)
						if failed || !json.Valid(result) {
							t.Fatalf("read %s=%s failed=%v", name, result, failed)
						}
					}
				}
				for _, operation := range []struct {
					name, path, countSQL string
					arguments, body      map[string]any
				}{
					{operatortool.FileIssue, base, "SELECT count(*) FROM issues WHERE title='Audited create'", map[string]any{"title": "Audited create", "description": issueContractTestSections}, map[string]any{"title": "Audited create", "body": issueContractTestSections}},
					{operatortool.AddComment, base + "/" + items[0] + "/comments", "SELECT count(*) FROM native_comments WHERE body='Audited comment'", map[string]any{"identifier": items[0], "body": "Audited comment"}, map[string]any{"body": "Audited comment"}},
					{operatortool.SetDependency, base + "/" + items[0] + "/dependencies", "SELECT count(*) FROM issue_dependencies", map[string]any{"identifier": items[0], "related": items[1], "operation": "add", "expected_revision": "1"}, map[string]any{"related_work_item_id": items[1], "operation": "add", "expected_revision": "1"}},
				} {
					t.Run(operation.name, func(t *testing.T) {
						wantFailure := failure == "pre-handler audit" || failure == "service"
						for range 2 {
							if transport == "MCP" {
								operation.arguments["organization_id"], operation.arguments["project_id"], operation.arguments["request_id"] = org, project, "audit-"+operation.name
								result, failed := call(operation.name, operation.arguments)
								if failed != wantFailure {
									t.Errorf("result=%s failed=%v want failure=%v", result, failed, wantFailure)
								}
								if !wantFailure && !json.Valid(result) {
									t.Errorf("invalid result=%s", result)
								}
								if failure == "service" && (!strings.Contains(string(result), "service_unavailable") || strings.Contains(string(result), "secret sentinel")) {
									t.Errorf("unsafe or incorrect service failure=%s", result)
								}
								if failure == "pre-handler audit" && !strings.Contains(string(result), "audit_unavailable") {
									t.Errorf("incorrect audit refusal=%s", result)
								}
							} else {
								operation.body["idempotency_key"] = "audit-" + operation.name
								response := attachmentRequest(t, client, http.MethodPost, operation.path, jsonBody(t, operation.body), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key.Token})
								wantStatus := http.StatusOK
								if wantFailure {
									wantStatus = http.StatusInternalServerError
								}
								if response.Code != wantStatus || !json.Valid(response.Body.Bytes()) {
									t.Errorf("response=%d %s want status=%d and one JSON document", response.Code, response.Body.String(), wantStatus)
								}
							}
							var count int
							wantCount := 1
							if wantFailure {
								wantCount = 0
							}
							if err := db.QueryRowContext(t.Context(), operation.countSQL).Scan(&count); err != nil || count != wantCount {
								t.Fatalf("mutations=%d want=%d error=%v", count, wantCount, err)
							}
						}
					})
				}
				if failure == "service" {
					return
				}
				other, err := sql.Open("sqlite", storeDSN(f.fixtures["org_beta"].path))
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				var otherProject string
				if err := other.QueryRowContext(t.Context(), "SELECT id FROM projects WHERE profile='native'").Scan(&otherProject); err != nil {
					t.Fatal(err)
				}
				for _, denial := range []string{"cross-project", "invalid key", "revoked key"} {
					t.Run(denial, func(t *testing.T) {
						token, targetProject, wantStatus := key.Token, project, http.StatusUnauthorized
						switch denial {
						case "cross-project":
							targetProject, wantStatus = otherProject, http.StatusForbidden
							if failure == "pre-handler audit" {
								wantStatus = http.StatusInternalServerError
							}
						case "invalid key":
							token += "invalid"
						case "revoked key":
							wantStatus = http.StatusForbidden
							revoked := attachmentRequest(t, owner, http.MethodDelete, "/api/cloud/account/api-keys/"+key.ID, nil, nil)
							if revoked.Code != http.StatusNoContent {
								t.Fatalf("revoke=%d %s", revoked.Code, revoked.Body.String())
							}
						}
						var raw []byte
						if transport == "MCP" {
							deniedCall := entryMCPClient(t, client, token, mcp.ProtocolVersion, "/mcp")
							result, failed := deniedCall(operatortool.AddComment, map[string]any{"organization_id": org, "project_id": targetProject, "identifier": items[0], "request_id": denial, "body": "Forbidden comment"})
							if !failed {
								t.Fatalf("unauthorized call succeeded: %s", result)
							}
							raw = result
						} else {
							response := attachmentRequest(t, client, http.MethodPost, "/api/v2/organizations/"+org+"/projects/"+targetProject+"/work-items/"+items[0]+"/comments", jsonBody(t, map[string]any{"idempotency_key": denial, "body": "Forbidden comment"}), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token})
							if response.Code != wantStatus || !json.Valid(response.Body.Bytes()) {
								t.Fatalf("unauthorized response=%d %s want=%d", response.Code, response.Body.String(), wantStatus)
							}
							raw = response.Body.Bytes()
						}
						for _, secret := range []string{key.Token, "Beta secret project", "Forbidden comment"} {
							if strings.Contains(string(raw), secret) {
								t.Fatal("refusal exposed credential or project content")
							}
						}
						var count int
						if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE body='Forbidden comment'").Scan(&count); err != nil || count != 0 {
							t.Fatalf("unauthorized comments=%d error=%v", count, err)
						}
						if err := other.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments").Scan(&count); err != nil || count != 0 {
							t.Fatalf("other project comments=%d error=%v", count, err)
						}
					})
				}
			})
		}
	}
}
