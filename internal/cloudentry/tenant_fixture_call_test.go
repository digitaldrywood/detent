package cloudentry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func signedTenantFixtureCall(t *testing.T, ctx context.Context, handler http.Handler, key ed25519.PrivateKey, claims cloudassert.Claims, body []byte, csrf string) (int, []byte) {
	t.Helper()
	assertion, err := cloudassert.Sign(key, claims)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, claims.Method, "http://tenant"+claims.Path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set(cloudassert.Header, assertion)
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

func TestTenantFixtureCallPreservesContextAndAuthorization(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "active"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			defer cancel()
			if canceled {
				cancel()
			}
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
			now := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
			body := []byte(`{"name":"Example"}`)
			claims := cloudassert.Claims{Issuer: "entry", Audience: "org_example", Generation: 1, Kind: cloudassert.KindBrowser, Subject: "user_example", Email: "example@example.test", ProviderOrganization: "provider_example", ProviderSession: "session_example", SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), Role: "owner", AccessExpiresAt: now.Add(time.Hour), Binding: strings.Repeat("a", 64), CSRF: strings.Repeat("b", 64), Method: http.MethodPost, Path: "/api/v2/organizations/org_example/projects", BodyDigest: cloudassert.BodyDigest(body), IssuedAt: now, ExpiresAt: now.Add(cloudassert.MaxLifetime), ID: "fixture-example"}
			called := false
			handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				called = true
				got, found := request.Context().Deadline()
				if !found || !got.Equal(deadline) {
					t.Errorf("fixture request deadline = %v/%t, want outer deadline %v", got, found, deadline)
				}
				if errors.Is(request.Context().Err(), context.Canceled) != canceled {
					t.Errorf("fixture context error = %v, canceled=%t", request.Context().Err(), canceled)
				}
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				verifier := cloudassert.Verifier{PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Issuer: claims.Issuer, Audience: claims.Audience, Generation: claims.Generation, Now: func() time.Time { return now }}
				verified, err := verifier.Verify(request.Header.Get(cloudassert.Header), request.Method, request.URL.Path, raw)
				if err != nil || verified.Subject != claims.Subject || !bytes.Equal(raw, body) {
					t.Errorf("fixture authorization/body = %v/%v", verified, err)
				}
				if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("X-CSRF-Token") != claims.CSRF {
					t.Error("fixture request headers lost JSON or CSRF authorization")
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("created"))
			})
			status, response := signedTenantFixtureCall(t, ctx, handler, key, claims, body, claims.CSRF)
			if !called || status != http.StatusCreated || string(response) != "created" {
				t.Errorf("fixture response called=%t status=%d body=%q", called, status, response)
			}
		})
	}
}
