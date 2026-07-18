package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/auth"
)

const (
	testOIDCClientID     = "detent-test"
	testOIDCClientSecret = "detent-secret"
	testOIDCRedirectURL  = "https://detent.example.com/auth/oidc/callback"
)

func TestOIDCProviderAuthorizationAndTokenValidation(t *testing.T) {
	t.Parallel()

	issuer := newFakeOIDCIssuer(t)
	provider, err := auth.NewIdentityProvider(t.Context(), auth.IdentityProviderOIDC, auth.OIDCConfig{
		IssuerURL:    issuer.URL(),
		ClientID:     testOIDCClientID,
		ClientSecret: testOIDCClientSecret,
		RedirectURL:  testOIDCRedirectURL,
		Scopes:       []string{"profile", "groups", "email"},
	})
	if err != nil {
		t.Fatalf("NewIdentityProvider() error = %v", err)
	}

	verifier := strings.Repeat("v", 43)
	authorizationURL, err := url.Parse(provider.AuthorizationURL("expected-state", "expected-nonce", verifier))
	if err != nil {
		t.Fatalf("Parse(AuthorizationURL()) error = %v", err)
	}
	query := authorizationURL.Query()
	if authorizationURL.Path != "/authorize" || query.Get("state") != "expected-state" || query.Get("nonce") != "expected-nonce" {
		t.Fatalf("AuthorizationURL() = %q", authorizationURL)
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(verifier) {
		t.Fatalf("AuthorizationURL() PKCE = method %q challenge %q", query.Get("code_challenge_method"), query.Get("code_challenge"))
	}
	for _, scope := range []string{"openid", "email", "profile", "groups"} {
		if !strings.Contains(" "+query.Get("scope")+" ", " "+scope+" ") {
			t.Fatalf("AuthorizationURL() scope = %q, want %q", query.Get("scope"), scope)
		}
	}

	tests := []struct {
		name string
		code string
		want error
	}{
		{name: "valid token", code: "valid"},
		{name: "code exchange failure", code: "exchange-error", want: auth.ErrOIDCExchange},
		{name: "missing id token", code: "missing-id-token", want: auth.ErrOIDCInvalidToken},
		{name: "wrong issuer", code: "wrong-issuer", want: auth.ErrOIDCInvalidToken},
		{name: "wrong audience", code: "wrong-audience", want: auth.ErrOIDCInvalidToken},
		{name: "invalid signature", code: "invalid-signature", want: auth.ErrOIDCInvalidToken},
		{name: "expired token", code: "expired", want: auth.ErrOIDCInvalidToken},
		{name: "missing issued at", code: "missing-issued-at", want: auth.ErrOIDCInvalidToken},
		{name: "future issued at", code: "future-issued-at", want: auth.ErrOIDCInvalidToken},
		{name: "multiple audiences without authorized party", code: "missing-azp", want: auth.ErrOIDCInvalidToken},
		{name: "wrong authorized party", code: "wrong-azp", want: auth.ErrOIDCInvalidToken},
		{name: "altered nonce", code: "wrong-nonce", want: auth.ErrOIDCInvalidNonce},
		{name: "missing email", code: "missing-email", want: auth.ErrOIDCMissingEmail},
		{name: "unverified email", code: "unverified-email", want: auth.ErrOIDCUnverifiedEmail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity, err := provider.Exchange(t.Context(), tt.code, verifier, "expected-nonce")
			if !errors.Is(err, tt.want) {
				t.Fatalf("Exchange() error = %v, want %v", err, tt.want)
			}
			if tt.want == nil && (identity.Subject != "subject-1" || identity.Email != "operator@example.com" || !identity.EmailVerified) {
				t.Fatalf("Exchange() identity = %#v", identity)
			}
		})
	}
	if issuer.lastCodeVerifier != verifier {
		t.Fatalf("token endpoint code_verifier = %q, want %q", issuer.lastCodeVerifier, verifier)
	}
}

func TestOIDCProviderRejectsDiscoveryIssuerMismatch(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL + "/different",
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"jwks_uri":               server.URL + "/jwks",
		}); err != nil {
			t.Errorf("Encode(discovery) error = %v", err)
		}
	}))
	t.Cleanup(server.Close)
	_, err := auth.NewIdentityProvider(context.Background(), auth.IdentityProviderOIDC, auth.OIDCConfig{
		IssuerURL:    server.URL,
		ClientID:     testOIDCClientID,
		ClientSecret: testOIDCClientSecret,
		RedirectURL:  testOIDCRedirectURL,
	})
	if err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("NewIdentityProvider() error = %v, want issuer mismatch", err)
	}
}

type fakeOIDCIssuer struct {
	t                *testing.T
	server           *httptest.Server
	key              *rsa.PrivateKey
	invalidKey       *rsa.PrivateKey
	lastCodeVerifier string
}

func newFakeOIDCIssuer(t *testing.T) *fakeOIDCIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	invalidKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey(invalid) error = %v", err)
	}
	issuer := &fakeOIDCIssuer{t: t, key: key, invalidKey: invalidKey}
	issuer.server = httptest.NewServer(http.HandlerFunc(issuer.serveHTTP))
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (f *fakeOIDCIssuer) URL() string {
	return f.server.URL
}

func (f *fakeOIDCIssuer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		f.writeJSON(w, map[string]any{
			"issuer":                                f.URL(),
			"authorization_endpoint":                f.URL() + "/authorize",
			"token_endpoint":                        f.URL() + "/token",
			"jwks_uri":                              f.URL() + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	case "/jwks":
		f.writeJSON(w, map[string]any{"keys": []any{rsaJWK(&f.key.PublicKey, "primary")}})
	case "/token":
		f.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOIDCIssuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("ParseForm() error = %v", err)
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	clientID, clientSecret, basic := r.BasicAuth()
	if !basic {
		clientID = r.Form.Get("client_id")
		clientSecret = r.Form.Get("client_secret")
	}
	if clientID != testOIDCClientID || clientSecret != testOIDCClientSecret {
		http.Error(w, "invalid client", http.StatusUnauthorized)
		return
	}
	f.lastCodeVerifier = r.Form.Get("code_verifier")
	code := r.Form.Get("code")
	if code == "exchange-error" {
		w.WriteHeader(http.StatusBadRequest)
		f.writeJSON(w, map[string]any{"error": "invalid_grant"})
		return
	}
	response := map[string]any{
		"access_token": "provider-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if code != "missing-id-token" {
		response["id_token"] = f.idToken(code)
	}
	f.writeJSON(w, response)
}

func (f *fakeOIDCIssuer) idToken(code string) string {
	now := time.Now()
	issuer := f.URL()
	audience := testOIDCClientID
	expiresAt := now.Add(time.Hour)
	nonce := "expected-nonce"
	emailVerified := true
	claims := map[string]any{
		"iss":            issuer,
		"sub":            "subject-1",
		"aud":            audience,
		"iat":            now.Unix(),
		"exp":            expiresAt.Unix(),
		"nonce":          nonce,
		"email":          "Operator@Example.com",
		"email_verified": emailVerified,
	}
	key := f.key
	switch code {
	case "wrong-issuer":
		claims["iss"] = issuer + "/wrong"
	case "wrong-audience":
		claims["aud"] = "another-client"
	case "invalid-signature":
		key = f.invalidKey
	case "expired":
		claims["exp"] = now.Add(-time.Minute).Unix()
	case "missing-issued-at":
		delete(claims, "iat")
	case "future-issued-at":
		claims["iat"] = now.Add(10 * time.Minute).Unix()
	case "missing-azp":
		claims["aud"] = []string{testOIDCClientID, "another-client"}
	case "wrong-azp":
		claims["aud"] = []string{testOIDCClientID, "another-client"}
		claims["azp"] = "another-client"
	case "wrong-nonce":
		claims["nonce"] = "altered-nonce"
	case "missing-email":
		delete(claims, "email")
	case "unverified-email":
		claims["email_verified"] = false
	}
	return signTestJWT(f.t, key, claims)
}

func (f *fakeOIDCIssuer) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		f.t.Errorf("Encode() error = %v", err)
	}
}

func rsaJWK(key *rsa.PublicKey, id string) map[string]string {
	return map[string]string{
		"kty": "RSA",
		"kid": id,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "primary", "typ": "JWT"})
	if err != nil {
		t.Fatalf("Marshal(header) error = %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("Marshal(claims) error = %v", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("SignPKCS1v15() error = %v", err)
	}
	return fmt.Sprintf("%s.%s", unsigned, base64.RawURLEncoding.EncodeToString(signature))
}
