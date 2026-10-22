package cloudentry

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const testOAuthVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
const testOAuthChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

func TestOAuthAnonymousRequestsDoNotPersist(t *testing.T) {
	f := newEntryFixture(t)
	b := newBrowser(t, f.service.Handler())
	for _, name := range []string{"First assistant", "Second assistant"} {
		client := registerTestOAuth(t, b, name, "none")
		for range 3 {
			response := b.do(http.MethodGet, "/oauth/authorize?"+testOAuthQuery(client).Encode(), nil, nil)
			if response.StatusCode != http.StatusSeeOther {
				t.Fatalf("authorize=%+v", response)
			}
		}
	}
	for _, test := range []struct{ name, body string }{
		{"sealed client ID", `{"redirect_uris":["https://assistant.example.test/` + strings.Repeat("x", 1900) + `"]}`},
		{"client name", `{"client_name":"` + strings.Repeat("x", 129) + `","redirect_uris":["https://assistant.example.test/callback"]}`},
		{"redirect count", `{"redirect_uris":[` + strings.Repeat(`"https://assistant.example.test/callback",`, 20) + `"https://assistant.example.test/callback"]}`},
		{"request body", `{"redirect_uris":["https://assistant.example.test/callback"],"extra":"` + strings.Repeat("x", 64<<10) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := attachmentRequest(t, b, http.MethodPost, "/oauth/register", strings.NewReader(test.body), map[string]string{"Content-Type": "application/json"})
			if refused.Code != http.StatusBadRequest {
				t.Fatalf("oversized registration=%d", refused.Code)
			}
		})
	}
	for _, table := range []string{"oauth_clients", "oauth_requests", "oauth_grants", "oauth_refresh_tokens"} {
		var count int
		if err := f.service.auth.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("anonymous requests persisted %d rows in %s", count, table)
		}
	}
}

func TestOAuthAnonymousRequestThrottling(t *testing.T) {
	for _, test := range []struct {
		name, method, peer, forwarded string
		status                        int
	}{
		{"registration direct", http.MethodPost, "192.0.2.10:1234", "", http.StatusCreated},
		{"authorization direct", http.MethodGet, "192.0.2.10:1234", "", http.StatusSeeOther},
		{"registration proxy", http.MethodPost, "127.0.0.1:1234", "198.51.100.1, 192.0.2.10", http.StatusCreated},
		{"authorization proxy", http.MethodGet, "[::1]:1234", "198.51.100.1, 192.0.2.10", http.StatusSeeOther},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t)
			now := f.service.config.now()
			f.service.config.now = func() time.Time { return now }
			client := registerTestOAuth(t, newBrowser(t, f.service.Handler()), "Example assistant", "none")
			request := func(method, peer, forwarded string) *httptest.ResponseRecorder {
				t.Helper()
				target := "/oauth/register"
				if method == http.MethodGet {
					target = "/oauth/authorize?" + testOAuthQuery(client).Encode()
				}
				r := httptest.NewRequest(method, testPublicURL+target, strings.NewReader(`{"redirect_uris":["https://assistant.example.test/callback"]}`))
				r.RemoteAddr = peer
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Forwarded-For", forwarded)
				r.Header.Set("X-Real-IP", "203.0.113.1")
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, r)
				return response
			}
			for range 30 {
				if response := request(test.method, test.peer, test.forwarded); response.Code != test.status {
					t.Fatalf("burst request=%d %s", response.Code, response.Body.String())
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				for _, spoof := range []string{"203.0.113.2", "203.0.113.3"} {
					forwarded := spoof
					if test.forwarded != "" {
						forwarded += ", 192.0.2.10"
					}
					response := request(method, test.peer, forwarded)
					var problem struct {
						Error string `json:"error"`
					}
					if response.Code != http.StatusTooManyRequests || json.Unmarshal(response.Body.Bytes(), &problem) != nil || problem.Error != "rate_limited" || response.Header().Get("Retry-After") != "60" || response.Header().Get("X-RateLimit-Limit") != "120" || response.Header().Get("X-RateLimit-Remaining") != "0" {
						t.Fatalf("throttled request=%d %s headers=%v", response.Code, response.Body.String(), response.Header())
					}
				}
			}
			peer, forwarded := "192.0.2.2:4321", ""
			if test.forwarded != "" {
				peer, forwarded = test.peer, "192.0.2.2"
			}
			if response := request(test.method, peer, forwarded); response.Code != test.status {
				t.Fatalf("other caller=%d %s", response.Code, response.Body.String())
			}
			for path, status := range map[string]int{"/.well-known/oauth-authorization-server": http.StatusOK, "/mcp": http.StatusUnauthorized} {
				r := httptest.NewRequest(http.MethodGet, testPublicURL+path, nil)
				r.RemoteAddr = test.peer
				r.Header.Set("X-Forwarded-For", test.forwarded)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, r)
				if response.Code != status {
					t.Fatalf("unthrottled endpoint %s=%d", path, response.Code)
				}
			}
			now = now.Add(time.Minute)
			if response := request(test.method, test.peer, test.forwarded); response.Code != test.status {
				t.Fatalf("refilled quota=%d %s", response.Code, response.Body.String())
			}
			for _, table := range []string{"oauth_clients", "oauth_requests", "oauth_grants", "oauth_refresh_tokens"} {
				var count int
				if err := f.service.auth.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("anonymous requests in %s=%d err=%v", table, count, err)
				}
			}
		})
	}
}

func registerTestOAuth(t *testing.T, b *browser, name, method string) oauthClient {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"client_name": name, "token_endpoint_auth_method": method, "redirect_uris": []string{"https://assistant.example.test/callback"}, "client_uri": "https://assistant.example.test", "client_secret_expires_at": 123, "application_type": "web", "contacts": []string{"support@example.test"}})
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
		if (client.SecretExpiresAt == nil) != (method == "none") || client.SecretExpiresAt != nil && *client.SecretExpiresAt != 0 {
			t.Fatalf("unexpected client secret expiry: %+v", client.SecretExpiresAt)
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
		var stored string
		if err := f.service.auth.store.reader.QueryRowContext(t.Context(), "SELECT secret_hash FROM oauth_clients WHERE id=?", c.ID).Scan(&stored); err != nil || stored == c.Secret {
			t.Fatal("consented client secret not hashed")
		}
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
		if _, err := f.service.auth.store.db.ExecContext(t.Context(), `CREATE TRIGGER fail_spent_refresh_insert BEFORE INSERT ON oauth_spent_refresh_tokens BEGIN SELECT RAISE(ABORT,'injected insert failure'); END`); err != nil {
			t.Fatal(err)
		}
		if response := owner.do(http.MethodPost, "/oauth/token", refresh, nil); response.StatusCode < 400 {
			t.Fatal("rotation succeeded without retaining spent token")
		}
		if err := f.service.auth.store.reader.QueryRowContext(t.Context(), "SELECT token_hash FROM oauth_refresh_tokens WHERE grant_id=?", key.ID).Scan(&stored); err != nil || stored != apikey.HashToken(token.RefreshToken) {
			t.Fatalf("failed rotation changed current token: %v", err)
		}
		if _, err := f.service.auth.store.db.ExecContext(t.Context(), "DROP TRIGGER fail_spent_refresh_insert"); err != nil {
			t.Fatal(err)
		}
		rotated := exchangeTestOAuth(t, owner, refresh)
		if token.RefreshToken == rotated.RefreshToken || token.AccessToken == rotated.AccessToken {
			t.Fatal("tokens did not rotate")
		}
		if _, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(rotated.AccessToken)); err != nil {
			t.Fatal(err)
		}
		rotating := url.Values{"client_id": {c.ID}, "client_secret": {c.Secret}, "grant_type": {"refresh_token"}, "resource": {testPublicURL + "/mcp"}}
		for range 32 {
			rotating.Set("refresh_token", rotated.RefreshToken)
			rotated = exchangeTestOAuth(t, owner, rotating)
		}
		var retained int
		if err := f.service.auth.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM oauth_refresh_tokens WHERE grant_id=?", key.ID).Scan(&retained); err != nil || retained != 1 {
			t.Fatalf("refresh history retained=%d want=1 err=%v", retained, err)
		}
		foreign := client
		if c.ID == client.ID {
			foreign = other
		}
		for _, invalid := range []url.Values{
			{"client_id": {c.ID}, "client_secret": {c.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {"A" + rotated.RefreshToken}, "resource": {testPublicURL + "/mcp"}},
			{"client_id": {foreign.ID}, "client_secret": {foreign.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {rotated.RefreshToken}, "resource": {testPublicURL + "/mcp"}},
			{"client_id": {c.ID}, "client_secret": {c.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {rotated.RefreshToken}, "resource": {"https://other.example.test/mcp"}},
			{"client_id": {foreign.ID}, "client_secret": {foreign.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {testPublicURL + "/mcp"}},
			{"client_id": {c.ID}, "client_secret": {c.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {"https://other.example.test/mcp"}},
		} {
			if response := owner.do(http.MethodPost, "/oauth/token", invalid, nil); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid refresh=%+v", response)
			}
			if _, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(rotated.AccessToken)); err != nil {
				t.Fatalf("invalid refresh revoked grant: %v", err)
			}
		}
		if replay := owner.do(http.MethodPost, "/oauth/token", refresh, nil); replay.StatusCode != 400 {
			t.Fatal("refresh replay succeeded")
		}
		if _, err := f.service.activeMCPKey(t.Context(), apikey.HashToken(rotated.AccessToken)); err == nil {
			t.Fatal("refresh replay did not revoke current access token")
		}
		rotating.Set("refresh_token", rotated.RefreshToken)
		if response := owner.do(http.MethodPost, "/oauth/token", rotating, nil); response.StatusCode != http.StatusBadRequest {
			t.Fatal("refresh replay did not revoke current refresh token")
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
	for _, test := range []struct{ key, value string }{{"client_id", client.ID + "x"}, {"redirect_uri", "https://attacker.example.test/callback"}, {"code_challenge_method", "plain"}, {"code_challenge", "invalid"}, {"resource", "https://other.example.test/mcp"}, {"response_type", "token"}, {"scope", "admin"}} {
		t.Run(test.key, func(t *testing.T) {
			values, _ := url.ParseQuery(q.Encode())
			values.Set(test.key, test.value)
			if p := b.do(http.MethodGet, "/oauth/authorize?"+values.Encode(), nil, nil); p.StatusCode != 400 || p.Header.Get("Location") != "" {
				t.Fatalf("invalid authorize=%+v", p)
			}
		})
	}
	for _, scenario := range []string{"tampered cookie", "expired request", "oversized cookie", "wrong request"} {
		t.Run(scenario, func(t *testing.T) {
			browser := newBrowser(t, f.service.Handler())
			start := browser.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
			path := start.Header.Get("Location")
			base, err := url.Parse(browser.origin)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "tampered cookie", "oversized cookie":
				for _, cookie := range browser.jar.Cookies(base) {
					if cookie.Name != f.service.cookieName("oauth") {
						continue
					}
					cookie.Value += "x"
					if scenario == "oversized cookie" {
						cookie.Value = strings.Repeat("x", 3801)
					}
					browser.jar.SetCookies(base, []*http.Cookie{cookie})
				}
			case "expired request":
				now := f.service.config.now
				f.service.config.now = func() time.Time { return now().Add(11 * time.Minute) }
				defer func() { f.service.config.now = now }()
			case "wrong request":
				path += "x"
			}
			if response := browser.do(http.MethodGet, path, nil, nil); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid pending request=%+v", response)
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
	start = b.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
	path = start.Header.Get("Location")
	consent = b.do(http.MethodGet, path, nil, nil)
	form = url.Values{"csrf": {csrfFrom(t, consent.Body)}, "organization": {"org_alpha"}, "decision": {"allow"}}
	if response := b.do(http.MethodPost, path, form, nil); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("consent=%+v", response)
	}
	base, err := url.Parse(b.origin)
	if err != nil {
		t.Fatal(err)
	}
	b.jar.SetCookies(base, (&http.Response{Header: start.Header}).Cookies())
	if response := b.do(http.MethodPost, path, form, nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("consent replay=%+v", response)
	}
	var grants int
	if err := f.service.auth.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM oauth_grants").Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("consent replay left grants=%d err=%v", grants, err)
	}
}

func connectTestOAuth(t *testing.T, b *browser, endpoint, name, redirect string) (oauthClient, oauthTokenResponse) {
	t.Helper()
	origin, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	path := func(endpoint string) string {
		t.Helper()
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != origin.Scheme || u.Host != origin.Host {
			t.Fatalf("invalid discovered endpoint: %q", endpoint)
		}
		return u.RequestURI()
	}
	challenge := b.do(http.MethodPost, path(endpoint), nil, nil)
	_, metadata, found := strings.Cut(challenge.Header.Get("WWW-Authenticate"), `resource_metadata="`)
	metadata, _, closed := strings.Cut(metadata, `"`)
	if challenge.StatusCode != http.StatusUnauthorized || !found || !closed {
		t.Fatalf("challenge=%+v", challenge)
	}
	var resource struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	response := b.do(http.MethodGet, path(metadata), nil, nil)
	if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(response.Body), &resource) != nil || resource.Resource != endpoint || len(resource.Servers) != 1 {
		t.Fatalf("resource metadata=%+v", response)
	}
	issuer, err := url.Parse(resource.Servers[0])
	if err != nil {
		t.Fatal(err)
	}
	issuer.Path = "/.well-known/oauth-authorization-server" + issuer.Path
	var server struct {
		Issuer        string `json:"issuer"`
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
		Registration  string `json:"registration_endpoint"`
	}
	response = b.do(http.MethodGet, path(issuer.String()), nil, nil)
	if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(response.Body), &server) != nil || server.Issuer != resource.Servers[0] {
		t.Fatalf("server metadata=%+v", response)
	}
	raw, err := json.Marshal(oauthClient{Name: name, RedirectURIs: []string{redirect}, AuthMethod: "none", GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}})
	if err != nil {
		t.Fatal(err)
	}
	registered := attachmentRequest(t, b, http.MethodPost, path(server.Registration), bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
	var client oauthClient
	if registered.Code != http.StatusCreated || json.Unmarshal(registered.Body.Bytes(), &client) != nil || client.ID == "" {
		t.Fatalf("registration=%d %s", registered.Code, registered.Body.String())
	}
	query := testOAuthQuery(client)
	query.Set("resource", resource.Resource)
	started := b.do(http.MethodGet, path(server.Authorization)+"?"+query.Encode(), nil, nil)
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorization=%+v", started)
	}
	consentPath := started.Header.Get("Location")
	consent := b.do(http.MethodGet, consentPath, nil, nil)
	if consent.StatusCode == http.StatusSeeOther {
		b.login(consentPath, "user_alice:porg_alpha")
		consent = b.do(http.MethodGet, consentPath, nil, nil)
	}
	if consent.StatusCode != http.StatusOK || !strings.Contains(consent.Body, name) {
		t.Fatalf("consent=%+v", consent)
	}
	approved := b.do(http.MethodPost, consentPath, url.Values{"csrf": {csrfFrom(t, consent.Body)}, "organization": {"org_alpha"}, "decision": {"allow"}}, nil)
	callback, err := url.Parse(approved.Header.Get("Location"))
	if approved.StatusCode != http.StatusSeeOther || err != nil || callback.Query().Get("state") != query.Get("state") || callback.Query().Get("iss") != server.Issuer || callback.Query().Get("code") == "" {
		t.Fatalf("callback=%+v", approved)
	}
	code := callback.Query().Get("code")
	callback.RawQuery = ""
	if callback.String() != redirect {
		t.Fatalf("callback destination=%s", callback)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {testOAuthVerifier}, "resource": {resource.Resource}}
	response = b.do(http.MethodPost, path(server.Token), form, nil)
	var token oauthTokenResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(response.Body), &token) != nil || token.AccessToken == "" || token.RefreshToken == "" {
		t.Fatalf("token=%+v", response)
	}
	return client, token
}

func TestOAuthReporterAuthorityAndRevocation(t *testing.T) {
	for _, test := range []struct{ name, redirect string }{
		{"ChatGPT", "https://chatgpt.com/connector_platform_oauth_redirect"},
		{"ChatGPT callback ID", "https://chatgpt.com/connector/oauth/example-callback"},
		{"Claude", "https://claude.ai/api/mcp/auth_callback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t)
			f.provider.member("user_alice", "porg_alpha", "reporter")
			transport, _ := entryTenantTransport(t, f.service.Handler())
			origin, err := url.Parse(testPublicURL)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(origin)
			proxy.Transport = transport
			owner := newBrowser(t, proxy)
			owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			personal := createTestAccessKey(t, owner, "selected", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}, "")
			connector := newBrowser(t, proxy)
			client, token := connectTestOAuth(t, connector, testPublicURL+"/mcp", test.name, test.redirect)
			anonymous := newBrowser(t, proxy)
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
			var catalog struct {
				Tools []struct{ Name string } `json:"tools"`
			}
			if err := json.Unmarshal(oauthTools, &catalog); err != nil {
				t.Fatal(err)
			}
			var fileIssue string
			for _, tool := range catalog.Tools {
				if tool.Name == operatortool.FileIssue {
					fileIssue = tool.Name
				}
			}
			if fileIssue == "" {
				t.Fatal("reporter cannot discover issue filing")
			}
			project := attachmentProject(t, owner, "org_alpha")
			args := map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": "oauth-report", "type": "bug", "title": "Example report", "description": "## Steps to reproduce\nOpen the example page.\n## Expected\nThe page loads.\n## Actual\nAn error appears.\n## Where\nExample page.\n## Environment\nExample browser.", "state": "Todo"}
			raw, failed := oauthCall(fileIssue, args)
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
			foreign := newBrowser(t, proxy)
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
			if response := anonymous.do(http.MethodPost, "/mcp", nil, map[string]string{"Authorization": "Bearer " + token.AccessToken}); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked HTTP request=%+v", response)
			}
			if raw, failed := foreignCall("tools/list", nil); failed {
				t.Fatalf("other owner's session revoked=%s", raw)
			}
			refresh := url.Values{"client_id": {client.ID}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
			if p := owner.do(http.MethodPost, "/oauth/token", refresh, nil); p.StatusCode != 400 {
				t.Fatal("revoked refresh accepted")
			}
		})
	}
}

func TestOAuthCurrentAuthority(t *testing.T) {
	for _, scenario := range []string{"membership removed", "project grant removed", "role downgraded", "policy blocked", "policy approval", "refresh expired", "expired consumed refresh", "browser revoke"} {
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
			case "expired consumed refresh":
				now := f.service.config.now()
				f.service.config.now = func() time.Time { return now.Add(29 * 24 * time.Hour) }
				refresh := url.Values{"client_id": {client.ID}, "grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
				rotated := exchangeTestOAuth(t, b, refresh)
				f.service.config.now = func() time.Time { return now.Add(30 * 24 * time.Hour) }
				if response := b.do(http.MethodPost, "/oauth/token", refresh, nil); response.StatusCode != http.StatusBadRequest {
					t.Fatalf("expired consumed refresh=%+v", response)
				}
				refresh.Set("refresh_token", rotated.RefreshToken)
				exchangeTestOAuth(t, b, refresh)
				return
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
