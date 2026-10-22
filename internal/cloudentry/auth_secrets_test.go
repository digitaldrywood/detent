package cloudentry

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

func entrySecretEnvironment(active int, versions ...int) func(string) string {
	values := map[string]string{}
	for _, version := range versions {
		values[strconv.Itoa(version)] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(version)}, 32))
	}
	raw, _ := json.Marshal(values)
	return func(name string) string {
		if len(versions) == 0 {
			return ""
		}
		if name == "DETENT_HUB_SECRET_KEYS" {
			return string(raw)
		}
		if name == "DETENT_HUB_SECRET_KEY_VERSION" {
			return strconv.Itoa(active)
		}
		return ""
	}
}

func entrySecretKeys(t *testing.T, active int, versions ...int) *hubsecrets.Keyring {
	t.Helper()
	keys, err := hubsecrets.FromEnvironment(entrySecretEnvironment(active, versions...))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func openSecretEntry(t *testing.T, cfg Config) *Service {
	t.Helper()
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	<-s.authResealDone
	return s
}

func addSecretAuthorization(t *testing.T, s *Service, name string, tokens auth.HostedTokens) authorization {
	t.Helper()
	identity := auth.HostedIdentity{Subject: "user_" + name, OrganizationID: "provider_" + name, SessionID: "session_" + name, ExpiresAt: s.config.now().Add(time.Hour)}
	if err := s.auth.createSession(t.Context(), "hash_"+name, "csrf_"+name, auth.Identity{Subject: identity.Subject, Email: name + "@example.test", Hosted: &identity}); err != nil {
		t.Fatal(err)
	}
	session, err := s.auth.session(t.Context(), "hash_"+name)
	if err != nil {
		t.Fatal(err)
	}
	item, _, err := s.auth.authorize(t.Context(), session, "org_"+name, identity, tokens, session.Email)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func secretEntryConfig(t *testing.T, logs *lockedBuffer) Config {
	t.Helper()
	return Config{Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32)), Provider: newFakeProvider(), StateDir: t.TempDir(), Logger: slog.New(slog.NewJSONHandler(logs, nil)), LookupEnv: entrySecretEnvironment(0)}
}

func TestEntrySecretRestarts(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			t.Parallel()
			var logs lockedBuffer
			cfg := secretEntryConfig(t, &logs)
			if !legacy {
				cfg.LookupEnv = entrySecretEnvironment(1, 1)
			}
			s := openSecretEntry(t, cfg)
			want := auth.HostedTokens{AccessToken: "known-provider-access", RefreshToken: "known-provider-refresh"}
			items := []authorization{addSecretAuthorization(t, s, "one", want), addSecretAuthorization(t, s, "two", want)}
			if legacy {
				if strings.Count(logs.String(), "DETENT_HUB_SECRET_KEYS") != 1 || !strings.Contains(logs.String(), "DETENT_HUB_SECRET_KEY_VERSION") {
					t.Fatal("missing single startup warning")
				}
				for _, item := range items {
					if strings.Contains(item.sealedAccess, ".") {
						t.Fatal("legacy format changed")
					}
					raw, err := base64.RawStdEncoding.DecodeString(item.sealedAccess)
					if err != nil {
						t.Fatal(err)
					}
					plain, err := s.auth.seal.Open(nil, raw[:s.auth.seal.NonceSize()], raw[s.auth.seal.NonceSize():], []byte(item.Binding))
					if err != nil || string(plain) != want.AccessToken {
						t.Fatal("legacy sealing changed")
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				cfg.LookupEnv = entrySecretEnvironment(1, 1)
				s = openSecretEntry(t, cfg)
			}
			var v1 []hubsecrets.Envelope
			for _, item := range items {
				stored, err := s.auth.authorizationByBinding(t.Context(), item.Binding)
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.auth.tokens(stored)
				if err != nil || got != want {
					t.Fatalf("v1 tokens=%v err=%v", got, err)
				}
				envelope, err := decodeEntrySecret(stored.sealedAccess)
				if err != nil || envelope.Version != 1 {
					t.Fatalf("v1 envelope=%d err=%v", envelope.Version, err)
				}
				v1 = append(v1, envelope)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.SigningKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
			s = openSecretEntry(t, cfg)
			for _, item := range items {
				session, err := s.auth.session(t.Context(), "hash_"+strings.TrimPrefix(item.Identity.Subject, "user_"))
				if err != nil {
					t.Fatal("session lost on signing-key rotation", err)
				}
				stored, err := s.auth.authorization(t.Context(), session, item.Organization)
				if err != nil {
					t.Fatal("authorization lost on signing-key rotation", err)
				}
				got, err := s.auth.tokens(stored)
				if err != nil || got != want {
					t.Fatal("provider tokens lost on signing-key rotation", err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.LookupEnv = entrySecretEnvironment(2, 1, 2)
			s = openSecretEntry(t, cfg)
			for i, item := range items {
				stored, err := s.auth.authorizationByBinding(t.Context(), item.Binding)
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.auth.tokens(stored)
				if err != nil || got != want {
					t.Fatal("rotation changed tokens", err)
				}
				envelope, err := decodeEntrySecret(stored.sealedAccess)
				if err != nil || envelope.Version != 2 || !bytes.Equal(envelope.Ciphertext, v1[i].Ciphertext) || !bytes.Equal(envelope.Nonce, v1[i].Nonce) {
					t.Fatal("rotation did not rewrap", err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.LookupEnv = entrySecretEnvironment(2, 2)
			s = openSecretEntry(t, cfg)
			for _, item := range items {
				stored, err := s.auth.authorizationByBinding(t.Context(), item.Binding)
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.auth.tokens(stored)
				if err != nil || got != want {
					t.Fatal("retiring old key lost tokens", err)
				}
			}
		})
	}
}

type gatedEntryLegacySeal struct {
	cipher.AEAD
	binding  string
	entered  chan struct{}
	resume   chan struct{}
	once     sync.Once
	canceled <-chan struct{}
}

func (g *gatedEntryLegacySeal) Open(dst, nonce, ciphertext, aad []byte) ([]byte, error) {
	if string(aad) == g.binding {
		g.once.Do(func() {
			close(g.entered)
			select {
			case <-g.resume:
			case <-g.canceled:
			}
		})
	}
	return g.AEAD.Open(dst, nonce, ciphertext, aad)
}

func TestEntrySecretResealCASAndSkips(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	s := openSecretEntry(t, secretEntryConfig(t, &logs))
	for _, owner := range []string{"first-user", "second-user"} {
		if err := s.saveGitHubConnection(t.Context(), owner, &oauth2.Token{AccessToken: "github-" + owner}); err != nil {
			t.Fatal(err)
		}
	}
	old := auth.HostedTokens{AccessToken: "old-access", RefreshToken: "old-refresh"}
	items := map[string]authorization{}
	for _, name := range []string{"refresh", "revoked", "empty", "partial", "broken", "healthy", "active"} {
		items[name] = addSecretAuthorization(t, s, name, old)
	}
	for _, row := range []struct {
		query string
		args  []any
	}{
		{"UPDATE authorizations SET revoked_at=? WHERE binding=?", []any{formatTime(s.config.now()), items["revoked"].Binding}},
		{"UPDATE authorizations SET access_token='',refresh_token='' WHERE binding=?", []any{items["empty"].Binding}},
		{"UPDATE authorizations SET refresh_token='' WHERE binding=?", []any{items["partial"].Binding}},
		{"UPDATE authorizations SET access_token=? WHERE binding=?", []any{"broken-secret-material", items["broken"].Binding}},
	} {
		if _, err := s.auth.store.db.ExecContext(t.Context(), row.query, row.args...); err != nil {
			t.Fatal(err)
		}
	}
	s.auth.keys = entrySecretKeys(t, 2, 1, 2)
	if err := s.auth.storeTokens(t.Context(), items["active"].Binding, old); err != nil {
		t.Fatal(err)
	}
	before := map[string][2]string{}
	for name, item := range items {
		var pair [2]string
		if err := s.auth.store.reader.QueryRowContext(t.Context(), "SELECT access_token,refresh_token FROM authorizations WHERE binding=?", item.Binding).Scan(&pair[0], &pair[1]); err != nil {
			t.Fatal(err)
		}
		before[name] = pair
	}
	gate := &gatedEntryLegacySeal{AEAD: s.auth.seal, binding: items["refresh"].Binding, entered: make(chan struct{}), resume: make(chan struct{}), canceled: t.Context().Done()}
	s.auth.seal = gate
	done := make(chan error, 1)
	go func() { done <- s.resealAuthorizations(t.Context()) }()
	select {
	case <-gate.entered:
	case err := <-done:
		t.Fatal("reseal pass exited before refresh race", err)
	}
	newer := auth.HostedTokens{AccessToken: "new-access", RefreshToken: "new-refresh"}
	if err := s.auth.storeTokens(t.Context(), items["refresh"].Binding, newer); err != nil {
		t.Fatal(err)
	}
	close(gate.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"first-user", "second-user"} {
		var sealed string
		if err := s.auth.store.reader.QueryRowContext(t.Context(), `SELECT access_token FROM github_user_connections WHERE owner_subject=?`, owner).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sealed, "hk1.2.") {
			t.Fatal("GitHub connection did not rotate")
		}
		plain, err := s.auth.openToken("github-user-access", owner, sealed)
		if err != nil || plain != "github-"+owner {
			t.Fatal("rotated GitHub connection unusable", err)
		}
		if _, err := s.auth.openToken("github-user-access", "other-user", sealed); err == nil {
			t.Fatal("GitHub connection lost owner binding")
		}
	}
	for name, item := range items {
		var pair [2]string
		if err := s.auth.store.reader.QueryRowContext(t.Context(), "SELECT access_token,refresh_token FROM authorizations WHERE binding=?", item.Binding).Scan(&pair[0], &pair[1]); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "revoked", "empty", "broken", "active":
			if pair != before[name] {
				t.Fatalf("%s was rewritten", name)
			}
		case "partial":
			if !strings.HasPrefix(pair[0], "hk1.2.") || pair[1] != "" {
				t.Fatal("partial empty value not preserved")
			}
		default:
			want := old
			if name == "refresh" {
				want = newer
			}
			got, err := s.auth.tokens(authorization{Binding: item.Binding, sealedAccess: pair[0], sealedRefresh: pair[1]})
			if err != nil || got != want {
				t.Fatalf("%s tokens changed: %v", name, err)
			}
		}
	}
	cfg := s.config
	cfg.LookupEnv = entrySecretEnvironment(2, 2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openSecretEntry(t, cfg)
	broken, err := s.auth.authorizationByBinding(t.Context(), items["broken"].Binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.auth.tokens(broken); !errors.Is(err, errNoSession) {
		t.Fatal("unreadable row did not sign out", err)
	}
	healthy, err := s.auth.authorizationByBinding(t.Context(), items["healthy"].Binding)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.auth.tokens(healthy); err != nil || got != old {
		t.Fatal("bad row stopped startup migration", err)
	}
	message := logs.String()
	if strings.Count(message, "reseal authorization failed") != 2 || !strings.Contains(message, "binding_hash") {
		t.Fatal("bad-row failure was not logged once per pass")
	}
	for _, secret := range []string{"broken-secret-material", old.AccessToken, old.RefreshToken, items["broken"].Binding, before["broken"][1]} {
		if strings.Contains(message, secret) {
			t.Fatal("secret appeared in logs")
		}
	}
}

func TestEntryOAuthKeyringState(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	cfg := secretEntryConfig(t, &logs)
	cfg.LookupEnv = entrySecretEnvironment(1, 1)
	s := openSecretEntry(t, cfg)
	b := newBrowser(t, s.Handler())
	client := registerTestOAuth(t, b, "Example assistant", "client_secret_post")
	if !strings.HasPrefix(client.ID, "oauth_client_hk1.1.") || len(client.ID) > 2048 {
		t.Fatal("invalid versioned client ID")
	}
	registration, err := s.oauthRegistration(t.Context(), client.ID)
	if err != nil || registration.Name != client.Name {
		t.Fatal("client did not round trip", err)
	}
	response := b.do(http.MethodGet, "/oauth/authorize?"+testOAuthQuery(client).Encode(), nil, nil)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize=%d", response.StatusCode)
	}
	var cookie *http.Cookie
	for _, candidate := range (&http.Response{Header: response.Header}).Cookies() {
		if candidate.Name == s.cookieName("oauth") {
			cookie = candidate
		}
	}
	if cookie == nil || !strings.HasPrefix(cookie.Value, "hk1.1.") || len(cookie.Value) > 3800 {
		t.Fatal("missing bounded versioned consent cookie")
	}
	var consent oauthRequest
	if err := s.openOAuth("oauth-consent", cookie.Value, &consent); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, purpose, value string }{
		{"tampered", "oauth-consent", cookie.Value + "A"},
		{"wrong purpose", "oauth-client", cookie.Value},
		{"client as consent", "oauth-consent", strings.TrimPrefix(client.ID, "oauth_client_")},
		{"malformed prefix", "oauth-consent", "hk1.bad"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var target oauthRequest
			if err := s.openOAuth(test.purpose, test.value, &target); !errors.Is(err, errNoSession) {
				t.Fatalf("bad state accepted: %v", err)
			}
		})
	}
	for _, id := range []string{client.ID + "A", "oauth_client_" + cookie.Value} {
		query := testOAuthQuery(client)
		query.Set("client_id", id)
		response := b.do(http.MethodGet, "/oauth/authorize?"+query.Encode(), nil, nil)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(response.Body, "unauthorized_client") {
			t.Fatal("invalid keyring client returned wrong OAuth error")
		}
	}
	other := &Service{auth: s.auth, config: s.config}
	other.config.PublicURL = "https://other.example.test"
	if err := other.openOAuth("oauth-consent", cookie.Value, &oauthRequest{}); !errors.Is(err, errNoSession) {
		t.Fatal("origin AAD not enforced")
	}
	consent.ExpiresAt = s.config.now().Add(-time.Second)
	expired, err := s.sealOAuth("oauth-consent", consent)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{expired, cookie.Value + "A", strings.TrimPrefix(client.ID, "oauth_client_")} {
		request := httptest.NewRequest(http.MethodGet, "/oauth/consent/"+consent.ID, nil)
		request.AddCookie(&http.Cookie{Name: s.cookieName("oauth"), Value: value})
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request") {
			t.Fatal("invalid keyring consent returned wrong OAuth error")
		}
	}
	access, err := s.auth.sealToken("provider-access", "binding", "access")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.auth.openToken("provider-refresh", "binding", access); !errors.Is(err, errNoSession) {
		t.Fatal("provider purpose AAD not enforced")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SigningKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	s = openSecretEntry(t, cfg)
	got, err := s.oauthRegistration(t.Context(), client.ID)
	if err != nil || !reflect.DeepEqual(got, registration) {
		t.Fatal("pending registration lost on signing rotation", err)
	}
	var reopened oauthRequest
	if err := s.openOAuth("oauth-consent", cookie.Value, &reopened); err != nil || reopened.ID != consent.ID {
		t.Fatal("consent lost on signing rotation", err)
	}
}

func TestEntryRejectsMalformedKeyring(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	cfg := secretEntryConfig(t, &logs)
	cfg.LookupEnv = func(string) string { return "invalid-key-material" }
	if _, err := Open(context.Background(), cfg); err == nil || strings.Contains(err.Error(), "invalid-key-material") {
		t.Fatal("malformed keyring not safely rejected", err)
	}
}
