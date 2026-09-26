package cloudassert_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func testKey(t *testing.T, seed byte) ed25519.PrivateKey {
	t.Helper()
	value := make([]byte, ed25519.SeedSize)
	for i := range value {
		value[i] = seed
	}
	key, err := cloudassert.ParsePrivateKey(base64.StdEncoding.EncodeToString(value))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func browserClaims(now time.Time, id string) cloudassert.Claims {
	return cloudassert.Claims{
		Issuer: "entry", Audience: "org_a", Generation: 2, Kind: cloudassert.KindBrowser,
		Subject: "user_a", Email: "a@example.test", ProviderOrganization: "org_provider_a", ProviderSession: "session_a",
		SessionCreatedAt: now.Add(-time.Minute), SessionExpiresAt: now.Add(time.Hour),
		Binding: strings.Repeat("b", 64), CSRF: strings.Repeat("c", 64),
		Method: "POST", Path: "/organizations/org_a/projects", BodyDigest: cloudassert.BodyDigest([]byte("name=x")),
		IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id,
	}
}

func TestVerify(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	key := testKey(t, 1)
	other := testKey(t, 2)
	tests := []struct {
		name   string
		key    ed25519.PrivateKey
		mutate func(*cloudassert.Claims)
		method string
		path   string
		body   string
		header func(string) string
		ok     bool
	}{
		{name: "valid browser", key: key, ok: true},
		{name: "wrong key", key: other},
		{name: "wrong audience", key: key, mutate: func(c *cloudassert.Claims) { c.Audience = "org_b" }},
		{name: "wrong issuer", key: key, mutate: func(c *cloudassert.Claims) { c.Issuer = "other" }},
		{name: "stale generation", key: key, mutate: func(c *cloudassert.Claims) { c.Generation = 1 }},
		{name: "method substitution", key: key, method: "GET"},
		{name: "path substitution", key: key, path: "/organizations/org_a/projects/other"},
		{name: "body substitution", key: key, body: "name=y"},
		{name: "expired", key: key, mutate: func(c *cloudassert.Claims) {
			c.IssuedAt, c.ExpiresAt = now.Add(-40*time.Second), now.Add(-10*time.Second)
		}},
		{name: "future issued", key: key, mutate: func(c *cloudassert.Claims) {
			c.IssuedAt, c.ExpiresAt = now.Add(time.Minute), now.Add(70*time.Second)
		}},
		{name: "browser missing binding", key: key, mutate: func(c *cloudassert.Claims) { c.Binding = "" }},
		{name: "machine with identity", key: key, mutate: func(c *cloudassert.Claims) { c.Kind = cloudassert.KindMachine }},
		{name: "machine without identity", key: key, ok: true, mutate: func(c *cloudassert.Claims) {
			*c = cloudassert.Claims{Issuer: c.Issuer, Audience: c.Audience, Generation: c.Generation, Kind: cloudassert.KindMachine, Method: c.Method, Path: c.Path, BodyDigest: c.BodyDigest, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt, ID: c.ID}
		}},
		{name: "service with organization identity", key: key, mutate: func(c *cloudassert.Claims) { c.Kind = cloudassert.KindService }},
		{name: "unknown kind", key: key, mutate: func(c *cloudassert.Claims) { c.Kind = "admin" }},
		{name: "tampered payload", key: key, header: func(value string) string { return "x" + value }},
		{name: "missing signature", key: key, header: func(value string) string { return strings.Split(value, ".")[0] }},
		{name: "empty", key: key, header: func(string) string { return "" }},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := browserClaims(now, "id-"+string(rune('a'+i)))
			if test.mutate != nil {
				test.mutate(&claims)
			}
			value, err := signUnchecked(test.key, claims)
			if err != nil {
				t.Fatal(err)
			}
			if test.header != nil {
				value = test.header(value)
			}
			method, path, body := "POST", "/organizations/org_a/projects", "name=x"
			if test.method != "" {
				method = test.method
			}
			if test.path != "" {
				path = test.path
			}
			if test.body != "" {
				body = test.body
			}
			verifier := &cloudassert.Verifier{PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Issuer: "entry", Audience: "org_a", Generation: 2, Now: func() time.Time { return now }}
			_, err = verifier.Verify(value, method, path, []byte(body))
			if (err == nil) != test.ok {
				t.Fatalf("Verify error = %v, want ok %v", err, test.ok)
			}
		})
	}
}

func signUnchecked(key ed25519.PrivateKey, claims cloudassert.Claims) (string, error) {
	if claims.ExpiresAt.Sub(claims.IssuedAt) <= cloudassert.MaxLifetime && claims.ExpiresAt.After(claims.IssuedAt) {
		return cloudassert.Sign(key, claims)
	}
	return "", nil
}

func TestVerifyRejectsReplayAndAcceptsRotatedKeys(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	oldKey, newKey := testKey(t, 3), testKey(t, 4)
	verifier := &cloudassert.Verifier{PublicKeys: []ed25519.PublicKey{oldKey.Public().(ed25519.PublicKey), newKey.Public().(ed25519.PublicKey)}, Issuer: "entry", Audience: "org_a", Generation: 2, Now: func() time.Time { return now }}
	for _, key := range []ed25519.PrivateKey{oldKey, newKey} {
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		value, err := cloudassert.Sign(key, browserClaims(now, id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(value, "POST", "/organizations/org_a/projects", []byte("name=x")); err != nil {
			t.Fatalf("first use rejected: %v", err)
		}
		if _, err := verifier.Verify(value, "POST", "/organizations/org_a/projects", []byte("name=x")); err == nil {
			t.Fatal("replayed assertion accepted")
		}
	}
}

func TestSignRejectsLongLifetime(t *testing.T) {
	now := time.Now()
	claims := browserClaims(now, "long")
	claims.ExpiresAt = now.Add(time.Minute)
	if _, err := cloudassert.Sign(testKey(t, 5), claims); err == nil {
		t.Fatal("Sign accepted a lifetime beyond the limit")
	}
}

func TestKeyParsingAndBindings(t *testing.T) {
	key := testKey(t, 6)
	public, err := cloudassert.ParsePublicKey(cloudassert.EncodePublicKey(key.Public().(ed25519.PublicKey)))
	if err != nil || !public.Equal(key.Public()) {
		t.Fatalf("public key round trip = %v, %v", public, err)
	}
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := cloudassert.ParsePrivateKey(value); err == nil {
			t.Fatalf("ParsePrivateKey(%q) accepted", value)
		}
		if _, err := cloudassert.ParsePublicKey(value); err == nil {
			t.Fatalf("ParsePublicKey(%q) accepted", value)
		}
	}
	if cloudassert.CSRFToken("s", "org_a") == cloudassert.CSRFToken("s", "org_b") || cloudassert.AuthorizationBinding("s", "org_a", "p1") == cloudassert.AuthorizationBinding("s", "org_a", "p2") {
		t.Fatal("bindings do not separate organizations or provider sessions")
	}
}

func TestCanonicalPath(t *testing.T) {
	for _, test := range []struct {
		target string
		ok     bool
	}{
		{"/organizations/org_a/projects/p", true},
		{"/organizations/org_a/projects/p?cursor=abc", true},
		{"/organizations/org_a/projects%2Fp", false},
		{"/organizations/org_a/%70rojects", false},
		{"/organizations/org_a/./projects", false},
		{"/organizations/org_a/../org_b", false},
		{"/organizations//org_a", false},
		{"/organizations/org_a/projects/p?cursor=a&cursor=b", false},
		{"/organizations/org_a/projects/p?cursor=%zz", false},
	} {
		t.Run(test.target, func(t *testing.T) {
			u, err := url.ParseRequestURI(test.target)
			if err != nil {
				if test.ok {
					t.Fatal(err)
				}
				return
			}
			if got := cloudassert.CanonicalPath(u); got != test.ok {
				t.Fatalf("canonical = %v, want %v", got, test.ok)
			}
		})
	}
}
