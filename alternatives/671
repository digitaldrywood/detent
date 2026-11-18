package harness

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudentry/oauth"
)

const OAuthVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

const OAuthChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

func RegisterOAuth(t *testing.T, b *Browser, name, method string) oauth.Client {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"client_name": name, "token_endpoint_auth_method": method, "redirect_uris": []string{"https://assistant.example.test/callback"}, "client_uri": "https://assistant.example.test", "client_secret_expires_at": 123, "application_type": "web", "contacts": []string{"support@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	response := Request(t, b, http.MethodPost, "/oauth/register", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
	var client oauth.Client
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &client) != nil || client.ID == "" {
		t.Fatalf("register=%d %s", response.Code, response.Body.String())
	}
	return client
}

func OAuthQuery(client oauth.Client) url.Values {
	return url.Values{"client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "response_type": {"code"}, "state": {"original-state"}, "code_challenge": {OAuthChallenge}, "code_challenge_method": {"S256"}, "resource": {PublicURL + "/mcp"}, "scope": {oauth.Scope}}
}

func ApproveOAuth(t *testing.T, b *Browser, client oauth.Client, org string) url.Values {
	t.Helper()
	started := b.Do(http.MethodGet, "/oauth/authorize?"+OAuthQuery(client).Encode(), nil, nil)
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize=%d %s", started.StatusCode, started.Body)
	}
	path := started.Header.Get("Location")
	consent := b.Do(http.MethodGet, path, nil, nil)
	if consent.StatusCode != http.StatusOK || !strings.Contains(consent.Body, client.Name) || !strings.Contains(consent.Body, `value="`+org+`"`) {
		t.Fatalf("consent=%d %s", consent.StatusCode, consent.Body)
	}
	approved := b.Do(http.MethodPost, path, url.Values{"csrf": {CSRF(t, consent.Body)}, "organization": {org}, "decision": {"allow"}}, nil)
	if approved.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve=%d %s", approved.StatusCode, approved.Body)
	}
	location, err := url.Parse(approved.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("state") != "original-state" || location.Query().Get("iss") != PublicURL || location.Query().Get("code") == "" {
		t.Fatalf("callback=%s", location)
	}
	return url.Values{"client_id": {client.ID}, "client_secret": {client.Secret}, "grant_type": {"authorization_code"}, "code": {location.Query().Get("code")}, "redirect_uri": {client.RedirectURIs[0]}, "code_verifier": {OAuthVerifier}, "resource": {PublicURL + "/mcp"}}
}

func ExchangeOAuth(t *testing.T, b *Browser, form url.Values) oauth.TokenResponse {
	t.Helper()
	response := b.Do(http.MethodPost, "/oauth/token", form, nil)
	var token oauth.TokenResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(response.Body), &token) != nil || token.AccessToken == "" || token.RefreshToken == "" || token.TokenType != "Bearer" || token.ExpiresIn != 3600 || token.Scope != oauth.Scope {
		t.Fatalf("token=%d %s", response.StatusCode, response.Body)
	}
	return token
}
