package cloudentry

import (
	"crypto/ed25519"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
)

func TestOAuthSigningKeyRotation(t *testing.T) {
	f := newEntryFixture(t)
	s := f.service
	<-s.oauthBackfillDone
	b := newBrowser(t, s.Handler())
	b.login("/organizations", "user_alice:")
	unconsented := registerTestOAuth(t, b, "Pending assistant", "none")
	type issued struct {
		client  oauthClient
		token   oauthTokenResponse
		code    url.Values
		rotated oauthTokenResponse
	}
	var clients []issued
	for _, format := range []string{"legacy", "opaque"} {
		client := registerTestOAuth(t, b, format+" assistant", "client_secret_post")
		token := exchangeTestOAuth(t, b, approveTestOAuth(t, b, client, "org_alpha"))
		var grant, stored string
		if err := s.auth.store.reader.QueryRowContext(t.Context(), "SELECT grant_id,token_hash FROM oauth_refresh_tokens WHERE token_hash=?", apikey.HashToken(token.RefreshToken)).Scan(&grant, &stored); err != nil {
			t.Fatal(err)
		}
		if stored == token.RefreshToken || !strings.HasPrefix(token.RefreshToken, "oauth_refresh_") || strings.ContainsAny(strings.TrimPrefix(token.RefreshToken, "oauth_refresh_"), ".+/=") {
			t.Fatal("new refresh token is not opaque and hashed")
		}
		if format == "legacy" {
			legacy, err := s.sealOAuth("oauth-refresh", struct {
				Grant     string    `json:"g"`
				ExpiresAt time.Time `json:"e"`
			}{grant, s.config.now().Add(oauthRefreshLifetime)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.auth.store.db.ExecContext(t.Context(), "UPDATE oauth_refresh_tokens SET token_hash=? WHERE grant_id=?", apikey.HashToken(legacy), grant); err != nil {
				t.Fatal(err)
			}
			token.RefreshToken = legacy
		}
		clients = append(clients, issued{client: client, token: token, code: approveTestOAuth(t, b, client, "org_beta")})
		if format == "legacy" {
			if _, err := s.auth.store.db.ExecContext(t.Context(), "DELETE FROM oauth_client_redirect_uris WHERE client_id=?", client.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	config := s.config
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	underA, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	<-underA.oauthBackfillDone
	for _, item := range clients {
		var count int
		if err := underA.auth.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM oauth_client_redirect_uris WHERE client_id=?", item.client.ID).Scan(&count); err != nil || count != len(item.client.RedirectURIs) {
			t.Fatalf("redirects=%d err=%v", count, err)
		}
	}
	if err := underA.Close(); err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 99
	config.SigningKey = ed25519.NewKeyFromSeed(seed)
	underB, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = underB.Close() })
	b = newBrowser(t, underB.Handler())
	for i, item := range clients {
		t.Run(item.client.Name, func(t *testing.T) {
			if response := b.do(http.MethodGet, "/oauth/authorize?"+testOAuthQuery(item.client).Encode(), nil, nil); response.StatusCode != http.StatusSeeOther {
				t.Fatalf("authorized client after key rotation=%d", response.StatusCode)
			}
			if _, err := underB.activeMCPKey(t.Context(), apikey.HashToken(item.token.AccessToken)); err != nil {
				t.Fatalf("existing access token after key rotation: %v", err)
			}
			refresh := url.Values{"client_id": {item.client.ID}, "client_secret": {item.client.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {item.token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
			wrongSecret := refresh.Get("client_secret")
			refresh.Set("client_secret", "wrong")
			if response := b.do(http.MethodPost, "/oauth/token", refresh, nil); response.StatusCode != http.StatusUnauthorized {
				t.Fatal("stored client secret verification bypassed")
			}
			refresh.Set("client_secret", wrongSecret)
			rotated := exchangeTestOAuth(t, b, refresh)
			if rotated.RefreshToken == item.token.RefreshToken || !strings.HasPrefix(rotated.RefreshToken, "oauth_refresh_") {
				t.Fatal("refresh did not rotate to an opaque token")
			}
			refresh.Set("refresh_token", rotated.RefreshToken)
			clients[i].rotated = exchangeTestOAuth(t, b, refresh)
			var spentHash string
			if err := underB.auth.store.reader.QueryRowContext(t.Context(), "SELECT token_hash FROM oauth_spent_refresh_tokens WHERE token_hash=?", apikey.HashToken(item.token.RefreshToken)).Scan(&spentHash); err != nil || spentHash != apikey.HashToken(item.token.RefreshToken) {
				t.Fatalf("spent refresh hash not retained: %v", err)
			}
			exchangeTestOAuth(t, b, item.code)
		})
	}
	if response := b.do(http.MethodGet, "/oauth/authorize?"+testOAuthQuery(unconsented).Encode(), nil, nil); response.StatusCode != http.StatusBadRequest || !strings.Contains(response.Body, "unauthorized_client") {
		t.Fatalf("unconsented authorize=%d", response.StatusCode)
	}
	if response := b.do(http.MethodPost, "/oauth/token", url.Values{"client_id": {unconsented.ID}, "grant_type": {"refresh_token"}, "refresh_token": {"unknown"}, "resource": {testPublicURL + "/mcp"}}, nil); response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Body, "invalid_client") {
		t.Fatalf("unconsented token=%d", response.StatusCode)
	}
	registered := registerTestOAuth(t, b, "Replacement assistant", "none")
	if response := b.do(http.MethodGet, "/oauth/authorize?"+testOAuthQuery(registered).Encode(), nil, nil); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("replacement authorize=%d", response.StatusCode)
	}
	if err := underB.Close(); err != nil {
		t.Fatal(err)
	}
	seed[0] = 100
	config.SigningKey = ed25519.NewKeyFromSeed(seed)
	underC, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = underC.Close() })
	b = newBrowser(t, underC.Handler())
	for _, item := range clients {
		t.Run(item.client.Name+" replay after restart", func(t *testing.T) {
			if _, err := underC.activeMCPKey(t.Context(), apikey.HashToken(item.rotated.AccessToken)); err != nil {
				t.Fatalf("current access token after restart: %v", err)
			}
			refresh := url.Values{"client_id": {item.client.ID}, "client_secret": {item.client.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {item.token.RefreshToken}, "resource": {testPublicURL + "/mcp"}}
			if response := b.do(http.MethodPost, "/oauth/token", refresh, nil); response.StatusCode != http.StatusBadRequest || !strings.Contains(response.Body, "invalid_grant") {
				t.Fatalf("spent refresh replay=%d", response.StatusCode)
			}
			if _, err := underC.activeMCPKey(t.Context(), apikey.HashToken(item.rotated.AccessToken)); err == nil {
				t.Fatal("spent refresh replay did not revoke current access token")
			}
			refresh.Set("refresh_token", item.rotated.RefreshToken)
			if response := b.do(http.MethodPost, "/oauth/token", refresh, nil); response.StatusCode != http.StatusBadRequest || !strings.Contains(response.Body, "invalid_grant") {
				t.Fatal("spent refresh replay did not revoke current refresh token")
			}
		})
	}
}
