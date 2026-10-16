package cloudentry

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const testOAuthVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
const testOAuthChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

func registerTestOAuth(t *testing.T, b *browser, name, method string) oauthClient {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"client_name": name, "token_endpoint_auth_method": method, "redirect_uris": []string{"https://assistant.example.test/callback"}, "client_uri": "https://assistant.example.test", "application_type": "web", "contacts": []string{"support@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	response := attachmentRequest(t, b, http.MethodPost, "/oauth/register", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
	var client oauthClient
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &client) != nil || client.ID == "" {
		t.Fatalf("register=%d %s", response.Code, response.Body.String())
	}
	return client
}

func testOAuthQuery(client oauthClient) url.Values {
	return url.Values{"client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "response_type": {"code"}, "state": {"original-state"}, "code_challenge": {testOAuthChallenge}, "code_challenge_method": {"S256"}, "resource": {testPublicURL + "/mcp"}, "scope": {oauthScope}}
}

func approveTestOAuth(t *testing.T, b *browser, client oauthClient, org string) url.Values {
	t.Helper()
	started := b.do(http.MethodGet, "/oauth/authorize?"+testOAuthQuery(client).Encode(), nil, nil)
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize=%d %s", started.StatusCode, started.Body)
	}
	path := started.Header.Get("Location")
	consent := b.do(http.MethodGet, path, nil, nil)
	if consent.StatusCode != http.StatusOK || !strings.Contains(consent.Body, client.Name) || !strings.Contains(consent.Body, `value="`+org+`"`) {
		t.Fatalf("consent=%d %s", consent.StatusCode, consent.Body)
	}
	approved := b.do(http.MethodPost, path, url.Values{"csrf": {csrfFrom(t, consent.Body)}, "organization": {org}, "decision": {"allow"}}, nil)
	if approved.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve=%d %s", approved.StatusCode, approved.Body)
	}
	location, err := url.Parse(approved.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("state") != "original-state" || location.Query().Get("iss") != testPublicURL || location.Query().Get("code") == "" {
		t.Fatalf("callback=%s", location)
	}
	return url.Values{"client_id": {client.ID}, "client_secret": {client.Secret}, "grant_type": {"authorization_code"}, "code": {location.Query().Get("code")}, "redirect_uri": {client.RedirectURIs[0]}, "code_verifier": {testOAuthVerifier}, "resource": {testPublicURL + "/mcp"}}
}

func exchangeTestOAuth(t *testing.T, b *browser, form url.Values) oauthTokenResponse {
	t.Helper()
	response := b.do(http.MethodPost, "/oauth/token", form, nil)
	var token oauthTokenResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(response.Body), &token) != nil || token.AccessToken == "" || token.RefreshToken == "" || token.TokenType != "Bearer" || token.ExpiresIn != 3600 || token.Scope != oauthScope {
		t.Fatalf("token=%d %s", response.StatusCode, response.Body)
	}
	return token
}

func TestOAuthDiscoveryAndRegistration(t *testing.T) {
	f := newEntryFixture(t)
	b := newBrowser(t, f.service.Handler())
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		p := b.do(http.MethodGet, path, nil, nil)
		var metadata struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}
		if p.StatusCode != 200 || json.Unmarshal([]byte(p.Body), &metadata) != nil || metadata.Resource != testPublicURL+"/mcp" || !reflect.DeepEqual(metadata.Servers, []string{testPublicURL}) {
			t.Fatalf("resource metadata=%+v", p)
		}
	}
	server := b.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil)
	var metadata map[string]json.RawMessage
	if server.StatusCode != 200 || json.Unmarshal([]byte(server.Body), &metadata) != nil {
		t.Fatalf("metadata=%+v", server)
	}
	for key, value := range map[string]string{"issuer": testPublicURL, "authorization_endpoint": testPublicURL + "/oauth/authorize", "token_endpoint": testPublicURL + "/oauth/token", "registration_endpoint": testPublicURL + "/oauth/register"} {
		var actual string
		if json.Unmarshal(metadata[key], &actual) != nil || actual != value {
			t.Fatalf("metadata %s=%s", key, metadata[key])
		}
	}
	if string(metadata["code_challenge_methods_supported"]) != `["S256"]` {
		t.Fatal("PKCE discovery missing")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		p := b.do(method, "/mcp", nil, nil)
		if p.StatusCode != 401 || p.Header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+testPublicURL+`/.well-known/oauth-protected-resource", scope="detent:mcp"` {
			t.Fatalf("challenge=%+v", p)
		}
	}
	for _, method := range []string{"none", "client_secret_post", "client_secret_basic"} {
		client := registerTestOAuth(t, b, "Assistant "+method, method)
		if (client.Secret == "") != (method == "none") {
			t.Fatal("unexpected client secret")
		}
		var stored string
		if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT secret_hash FROM oauth_clients WHERE id=?", client.ID).Scan(&stored); err != nil || stored == client.Secret {
			t.Fatal("secret not hashed")
		}
	}
	for _, uri := range []string{"http://assistant.example.test/callback", "https://assistant.example.test/callback#fragment", "https://user:pass@assistant.example.test/callback", "javascript:alert(1)"} {
		raw, _ := json.Marshal(oauthClient{RedirectURIs: []string{uri}})
		response := attachmentRequest(t, b, http.MethodPost, "/oauth/register", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
		if response.Code != 400 {
			t.Fatalf("redirect accepted: %s", uri)
		}
	}
}

func TestOAuthCodeExchangeAndRotation(t *testing.T) {
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations", "user_alice:")
	client := registerTestOAuth(t, owner, "ChatGPT", "none")
	other := registerTestOAuth(t, owner, "Claude", "client_secret_post")
	for _, test := range []struct{ name, key, value string }{
		{"bad verifier", "code_verifier", strings.Repeat("x", 43)}, {"missing verifier", "code_verifier", ""}, {"wrong redirect", "redirect_uri", "https://other.example.test/callback"}, {"wrong resource", "resource", "https://other.example.test/mcp"}, {"wrong client", "client_id", other.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			form := approveTestOAuth(t, owner, client, "org_alpha")
			valid := form.Encode()
			form.Set(test.key, test.value)
			refused := owner.do(http.MethodPost, "/oauth/token", form, nil)
			if refused.StatusCode < 400 {
				t.Fatal("invalid exchange succeeded")
			}
			form, err := url.ParseQuery(valid)
			if err != nil {
				t.Fatal(err)
			}
			exchangeTestOAuth(t, owner, form)
		})
	}
	for _, c := range []oauthClient{client, other} {
		form := approveTestOAuth(t, owner, c, "org_beta")
		token := exchangeTestOAuth(t, owner, form)
		if response := owner.do(http.MethodPost, "/oauth/token", form, nil); response.StatusCode != 400 {
			t.Fatal("code replay succeeded")
		}
		key, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(token.AccessToken))
		if err != nil || key.Owner != "user_alice" || key.Organizations[0].OrganizationID != "org_beta" {
			t.Fatalf("actor=%+v err=%v", key, err)
		}
		if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE oauth_grants SET access_expires_at='2000-01-01T00:00:00Z' WHERE id=?", key.ID); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/mcp", "/organizations/org_beta/mcp"} {
			p := owner.do(http.MethodPost, path, nil, map[string]string{"Authorization": "Bearer " + token.AccessToken})
			if p.StatusCode != 401 || p.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("expired %s=%d", path, p.StatusCode)
			}
		}
		refresh := url.Values{"client_id": {c.ID}, "client_secret": {c.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
		rotated := exchangeTestOAuth(t, owner, refresh)
		if token.RefreshToken == rotated.RefreshToken || token.AccessToken == rotated.AccessToken {
			t.Fatal("tokens did not rotate")
		}
		if _, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(rotated.AccessToken)); err != nil {
			t.Fatal(err)
		}
		if replay := owner.do(http.MethodPost, "/oauth/token", refresh, nil); replay.StatusCode != 400 {
			t.Fatal("refresh replay succeeded")
		}
		if _, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(rotated.AccessToken)); err == nil {
			t.Fatal("refresh replay did not revoke grant")
		}
	}
	basic := registerTestOAuth(t, owner, "Basic assistant", "client_secret_basic")
	form := approveTestOAuth(t, owner, basic, "org_alpha")
	form.Del("client_secret")
	for _, test := range []struct {
		secret string
		status int
	}{{"wrong", 401}, {basic.Secret, 200}} {
		header := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(basic.ID+":"+test.secret))}
		p := owner.do(http.MethodPost, "/oauth/token", form, header)
		if p.StatusCode != test.status {
			t.Fatalf("basic authentication=%d expected=%d", p.StatusCode, test.status)
		}
	}

}

func TestOAuthConsentBinding(t *testing.T) {
	f := newEntryFixture(t)
	b := newBrowser(t, f.service.Handler())
	client := registerTestOAuth(t, b, "Assistant", "none")
	q := testOAuthQuery(client)
	for _, test := range []struct{ key, value string }{{"redirect_uri", "https://attacker.example.test/callback"}, {"code_challenge_method", "plain"}, {"code_challenge", "invalid"}, {"resource", "https://other.example.test/mcp"}, {"response_type", "token"}, {"scope", "admin"}} {
		t.Run(test.key, func(t *testing.T) {
			values, _ := url.ParseQuery(q.Encode())
			values.Set(test.key, test.value)
			if p := b.do(http.MethodGet, "/oauth/authorize?"+values.Encode(), nil, nil); p.StatusCode != 400 || p.Header.Get("Location") != "" {
				t.Fatalf("invalid authorize=%+v", p)
			}
		})
	}
	start := b.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
	path := start.Header.Get("Location")
	foreign := newBrowser(t, f.service.Handler())
	foreign.login("/organizations", "user_bob:")
	if p := foreign.do(http.MethodGet, path, nil, nil); p.StatusCode != 400 {
		t.Fatal("consent accepted foreign browser")
	}
	login := b.do(http.MethodGet, path, nil, nil)
	if login.StatusCode != 303 || !strings.HasPrefix(login.Header.Get("Location"), "/auth/oidc/start?") {
		t.Fatal("missing WorkOS redirect")
	}
	destination := b.login(login.Header.Get("Location"), "user_alice:")
	if destination != path {
		t.Fatalf("login lost request: %s", destination)
	}
	consent := b.do(http.MethodGet, path, nil, nil)
	if !strings.Contains(consent.Body, "Alpha") || !strings.Contains(consent.Body, "Beta") {
		t.Fatal("consent omitted an available workspace")
	}
	form := url.Values{"csrf": {csrfFrom(t, consent.Body)}, "organization": {"org_alpha"}, "decision": {"allow"}}
	if p := b.do(http.MethodPost, path, form, map[string]string{"Origin": "https://attacker.example.test"}); p.StatusCode != 403 {
		t.Fatal("cross-origin consent accepted")
	}
	bad := url.Values{"csrf": {"bad"}, "organization": {"org_alpha"}, "decision": {"allow"}}
	if p := b.do(http.MethodPost, path, bad, nil); p.StatusCode != 403 {
		t.Fatal("CSRF accepted")
	}
	form.Set("organization", "org_missing")
	if p := b.do(http.MethodPost, path, form, nil); p.StatusCode != 403 {
		t.Fatal("foreign workspace accepted")
	}
	form.Set("decision", "deny")
	denied := b.do(http.MethodPost, path, form, nil)
	target, err := url.Parse(denied.Header.Get("Location"))
	if err != nil || denied.StatusCode != 303 || target.Query().Get("error") != "access_denied" || target.Query().Get("state") != "original-state" || target.Query().Get("iss") != testPublicURL {
		t.Fatalf("deny=%+v", denied)
	}
}

func TestOAuthReporterAuthorityAndRevocation(t *testing.T) {
	f := newEntryFixture(t)
	f.provider.member("user_alice", "porg_alpha", "reporter")
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	personal := createTestAccessKey(t, owner, "selected", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}, "")
	client := registerTestOAuth(t, owner, "Assistant", "none")
	token := exchangeTestOAuth(t, owner, approveTestOAuth(t, owner, client, "org_alpha"))
	anonymous := newBrowser(t, f.service.Handler())
	oauthCall := entryMCPClient(t, anonymous, token.AccessToken, "2025-11-25", "/mcp")
	personalCall := entryMCPClient(t, anonymous, personal.Token, "2025-11-25", "/mcp")
	oauthTools, failed := oauthCall("tools/list", nil)
	if failed {
		t.Fatalf("OAuth tools=%s", oauthTools)
	}
	personalTools, failed := personalCall("tools/list", nil)
	if failed || !bytes.Equal(oauthTools, personalTools) {
		t.Fatalf("reporter catalogs differ: OAuth=%s key=%s", oauthTools, personalTools)
	}
	project := attachmentProject(t, owner, "org_alpha")
	args := map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": "oauth-report", "type": "bug", "title": "Example report", "description": "## Steps to reproduce\nOpen the example page.\n## Expected\nThe page loads.\n## Actual\nAn error appears.\n## Where\nExample page.\n## Environment\nExample browser.", "state": "Todo"}
	raw, failed := oauthCall(operatortool.FileIssue, args)
	var envelope struct {
		Data struct {
			Actor struct {
				Kind        string `json:"kind"`
				PrincipalID string `json:"principal_id"`
				ViaKey      string `json:"via_key"`
			} `json:"actor"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", storeDSN(f.fixtures["org_alpha"].path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var principal string
	if err := db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", "user_alice").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if failed || envelope.Data.Actor.Kind != "human" || envelope.Data.Actor.PrincipalID != principal || !strings.HasPrefix(envelope.Data.Actor.ViaKey, "oauth_grant_") || envelope.Data.State != "Triage" {
		t.Fatalf("file issue=%s failed=%v", raw, failed)
	}
	for _, call := range []func(string, any) (json.RawMessage, bool){oauthCall, personalCall} {
		if raw, failed := call("list_projects", map[string]any{"organization_id": "org_beta"}); !failed {
			t.Fatalf("foreign workspace=%s", raw)
		}
		if raw, failed := call(operatortool.MembershipList, map[string]any{"organization_id": "org_alpha"}); !failed || !strings.Contains(string(raw), "role_denied") {
			t.Fatalf("reporter administration=%s", raw)
		}
	}
	listed := owner.do(http.MethodGet, "/api/cloud/account/oauth-grants", nil, nil)
	var grants struct {
		Grants []oauthGrant `json:"grants"`
	}
	if listed.StatusCode != 200 || json.Unmarshal([]byte(listed.Body), &grants) != nil || len(grants.Grants) != 1 || grants.Grants[0].LastUsedAt == nil || strings.Contains(listed.Body, token.AccessToken) || strings.Contains(listed.Body, token.RefreshToken) {
		t.Fatalf("grants=%+v", listed)
	}
	foreign := newBrowser(t, f.service.Handler())
	foreign.login("/organizations/org_beta/organization", "user_bob:porg_beta")
	beta, err := sql.Open("sqlite", storeDSN(f.fixtures["org_beta"].path))
	if err != nil {
		t.Fatal(err)
	}
	defer beta.Close()
	otherPrincipal := "hosted_" + apikey.HashToken("user_bob")[:32]
	if _, err := beta.ExecContext(t.Context(), `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) SELECT ?,?,?, '','operator',1,created_at,updated_at FROM hosted_members WHERE user_id=?`, otherPrincipal, otherPrincipal, apikey.HashToken("user_bob_principal"), "user_alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := beta.ExecContext(t.Context(), `INSERT INTO hosted_members(user_id,email,membership_id,role,active,principal_id,created_at,updated_at) SELECT ?,?,?,?,1,?,created_at,updated_at FROM hosted_members WHERE user_id=?`, "user_bob", "bob@example.test", "om_user_bob_porg_beta", "member", otherPrincipal, "user_alice"); err != nil {
		t.Fatal(err)
	}
	foreignToken := exchangeTestOAuth(t, foreign, approveTestOAuth(t, foreign, client, "org_beta"))
	foreignCall := entryMCPClient(t, anonymous, foreignToken.AccessToken, "2025-11-25", "/mcp")
	if raw, failed := foreignCall(operatortool.OAuthGrantList, map[string]any{}); failed || !strings.Contains(string(raw), "org_beta") || strings.Contains(string(raw), grants.Grants[0].ID) {
		t.Fatalf("foreign MCP grants=%s", raw)
	}
	if p := foreign.do(http.MethodDelete, "/api/cloud/account/oauth-grants/"+grants.Grants[0].ID, nil, nil); p.StatusCode < 400 {
		t.Fatal("foreign owner revoked grant")
	}
	if raw, failed := oauthCall(operatortool.OAuthGrantList, map[string]any{}); failed || !strings.Contains(string(raw), client.ID) {
		t.Fatalf("MCP grants=%s", raw)
	}
	if raw, failed := personalCall(operatortool.OAuthGrantRevoke, map[string]any{"grant_id": grants.Grants[0].ID}); failed {
		t.Fatalf("MCP revoke=%s", raw)
	}
	if raw, failed := oauthCall("tools/list", nil); !failed || !strings.Contains(string(raw), "invalid_token") {
		t.Fatalf("revoked session=%s", raw)
	}
	if raw, failed := foreignCall("tools/list", nil); failed {
		t.Fatalf("other owner's session revoked=%s", raw)
	}
	refresh := url.Values{"client_id": {client.ID}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
	if p := owner.do(http.MethodPost, "/oauth/token", refresh, nil); p.StatusCode != 400 {
		t.Fatal("revoked refresh accepted")
	}
}

func TestOAuthCurrentAuthority(t *testing.T) {
	for _, scenario := range []string{"membership removed", "project grant removed", "role downgraded", "policy blocked", "policy approval", "refresh expired", "browser revoke"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEntryFixture(t)
			b := newBrowser(t, f.service.Handler())
			b.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			client := registerTestOAuth(t, b, "Assistant", "none")
			token := exchangeTestOAuth(t, b, approveTestOAuth(t, b, client, "org_alpha"))
			key, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(token.AccessToken))
			if err != nil {
				t.Fatal(err)
			}
			call := entryMCPClient(t, newBrowser(t, f.service.Handler()), token.AccessToken, "2025-11-25", "/mcp")
			project := attachmentProject(t, b, "org_alpha")
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := f.service.auth.store.db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "membership removed":
				f.provider.mu.Lock()
				delete(f.provider.memberships, "om_user_alice_porg_alpha")
				f.provider.mu.Unlock()
			case "project grant removed":
				tenant := f.fixtures["org_alpha"]
				db, err := sql.Open("sqlite", storeDSN(tenant.path))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err = db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id='user_alice' AND project_id=?", project); err != nil {
					t.Fatal(err)
				}
			case "role downgraded":
				f.provider.member("user_alice", "porg_alpha", "viewer")
			case "policy blocked":
				exec("INSERT INTO organization_key_policy(organization_id,personal_keys) VALUES(?,?)", "org_alpha", "blocked")
			case "policy approval":
				exec("INSERT INTO organization_key_policy(organization_id,personal_keys) VALUES(?,?)", "org_alpha", "approval")
			case "refresh expired":
				exec("UPDATE oauth_refresh_tokens SET expires_at='2000-01-01T00:00:00Z' WHERE grant_id=?", key.ID)
			case "browser revoke":
				denied := b.do(http.MethodDelete, "/api/cloud/account/oauth-grants/"+key.ID, nil, map[string]string{"Origin": "https://other.example.test"})
				if denied.StatusCode != 403 {
					t.Fatal("cross-origin revoke accepted")
				}
				revoked := b.do(http.MethodDelete, "/api/cloud/account/oauth-grants/"+key.ID, nil, nil)
				if revoked.StatusCode != 204 {
					t.Fatalf("revoke=%+v", revoked)
				}
			}
			if scenario == "refresh expired" {
				refresh := url.Values{"client_id": {client.ID}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
				if p := b.do(http.MethodPost, "/oauth/token", refresh, nil); p.StatusCode != 400 {
					t.Fatal("expired refresh accepted")
				}
				return
			}
			if scenario == "role downgraded" {
				args := map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": "downgraded", "title": "No write permission"}
				if raw, failed := call(operatortool.FileIssue, args); !failed {
					t.Fatalf("downgraded write=%s", raw)
				}
				return
			}
			if raw, failed := call(operatortool.WorkList, map[string]any{"organization_id": "org_alpha", "project_id": project}); !failed {
				t.Fatalf("authority removal=%s", raw)
			}
			if scenario == "membership removed" || scenario == "browser revoke" {
				p := b.do(http.MethodPost, "/mcp", nil, map[string]string{"Authorization": "Bearer " + token.AccessToken})
				if p.StatusCode != 401 {
					t.Fatalf("inactive HTTP=%d", p.StatusCode)
				}
			}
		})
	}
}
