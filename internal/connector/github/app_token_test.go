package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstallationTokenSourceMintsAndCachesToken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	privateKey := testPrivateKeyPEM(t)
	var requests int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		if r.URL.Path != "/app/installations/987/access_tokens" {
			t.Fatalf("path = %s, want installation token path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("Authorization = %q, want JWT bearer", got)
		}
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		segments := strings.Split(jwt, ".")
		if len(segments) != 3 {
			t.Fatalf("JWT segments = %d, want 3", len(segments))
		}
		header := decodeJWTSegment(t, segments[0])
		if header["alg"] != "RS256" {
			t.Fatalf("JWT alg = %v, want RS256", header["alg"])
		}
		payload := decodeJWTSegment(t, segments[1])
		if payload["iss"] != "123" {
			t.Fatalf("JWT iss = %v, want 123", payload["iss"])
		}
		if payload["iat"] != float64(now.Unix()-60) {
			t.Fatalf("JWT iat = %v, want %d", payload["iat"], now.Unix()-60)
		}
		if payload["exp"] != float64(now.Unix()+600) {
			t.Fatalf("JWT exp = %v, want %d", payload["exp"], now.Unix()+600)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		response := map[string]string{
			"token":      "installation-token",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	t.Cleanup(server.Close)

	source, err := NewInstallationTokenSource(InstallationTokenConfig{
		Endpoint:       server.URL + "/graphql",
		AppID:          "123",
		InstallationID: "987",
		PrivateKey:     privateKey,
		HTTPClient:     server.Client(),
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewInstallationTokenSource() error = %v", err)
	}

	for range 2 {
		token, err := source.Token(context.Background())
		if err != nil {
			t.Fatalf("Token() error = %v", err)
		}
		if token != "installation-token" {
			t.Fatalf("Token() = %q, want installation-token", token)
		}
	}

	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestInstallationTokenSourceRefreshesNearExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	privateKey := testPrivateKeyPEM(t)
	var requests int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := atomic.AddInt64(&requests, 1)
		expiresAt := now.Add(5 * time.Minute)
		if count > 1 {
			expiresAt = now.Add(time.Hour)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		response := map[string]string{
			"token":      "token-" + strconv.FormatInt(count, 10),
			"expires_at": expiresAt.Format(time.RFC3339),
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	t.Cleanup(server.Close)

	source, err := NewInstallationTokenSource(InstallationTokenConfig{
		Endpoint:       server.URL + "/graphql",
		AppID:          "123",
		InstallationID: "987",
		PrivateKey:     privateKey,
		HTTPClient:     server.Client(),
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewInstallationTokenSource() error = %v", err)
	}

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() first error = %v", err)
	}
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() second error = %v", err)
	}
	if first != "token-1" || second != "token-2" {
		t.Fatalf("tokens = %q, %q, want token-1, token-2", first, second)
	}
	if got := atomic.LoadInt64(&requests); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestInstallationTokenSourceLoadsPrivateKeyPathAndGHESURL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	privateKeyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(privateKeyPath, []byte(testPrivateKeyPEM(t)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	requestPath := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		response := map[string]string{
			"token":      "path-token",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	t.Cleanup(server.Close)

	source, err := NewInstallationTokenSource(InstallationTokenConfig{
		Endpoint:       server.URL + "/api/graphql/",
		AppID:          "123",
		InstallationID: "987",
		PrivateKeyPath: "$PRIVATE_KEY_PATH",
		HTTPClient:     server.Client(),
		Now:            func() time.Time { return now },
		LookupEnv: func(key string) string {
			if key == "PRIVATE_KEY_PATH" {
				return privateKeyPath
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("NewInstallationTokenSource() error = %v", err)
	}

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "path-token" {
		t.Fatalf("Token() = %q, want path-token", token)
	}
	if got := <-requestPath; got != "/api/v3/app/installations/987/access_tokens" {
		t.Fatalf("request path = %q, want GHES REST path", got)
	}
}

func TestInstallationTokenSourceReportsInvalidPrivateKey(t *testing.T) {
	t.Parallel()

	source, err := NewInstallationTokenSource(InstallationTokenConfig{
		AppID:          "123",
		InstallationID: "987",
		PrivateKey:     "not a pem",
	})
	if err != nil {
		t.Fatalf("NewInstallationTokenSource() error = %v", err)
	}

	_, err = source.Token(context.Background())
	if !errors.Is(err, ErrInvalidPrivateKey) {
		t.Fatalf("Token() error = %v, want ErrInvalidPrivateKey", err)
	}
}

func TestTokenResolverUsesConfiguredGitHubApp(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	privateKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		response := map[string]string{
			"token":      "app-token",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	t.Cleanup(server.Close)

	resolver := NewTokenResolver(TokenResolverConfig{
		Endpoint:                server.URL + "/graphql",
		APIKey:                  "$GITHUB_TOKEN",
		GitHubAppID:             "$APP_ID",
		GitHubAppInstallationID: "$INSTALLATION_ID",
		GitHubAppPrivateKey:     "$PRIVATE_KEY",
		HTTPClient:              server.Client(),
		Now:                     func() time.Time { return now },
		LookupEnv: func(key string) string {
			values := map[string]string{
				"APP_ID":          "123",
				"INSTALLATION_ID": "987",
				"PRIVATE_KEY":     privateKey,
				"GITHUB_TOKEN":    "pat-token",
			}
			return values[key]
		},
		GHToken: func(context.Context, string) (string, error) {
			t.Fatal("gh token fallback should not run")
			return "", nil
		},
	})

	token, err := resolver.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "app-token" {
		t.Fatalf("Token() = %q, want app-token", token)
	}
}

func TestTokenResolverUsesGHTokenFallback(t *testing.T) {
	t.Parallel()

	resolver := NewTokenResolver(TokenResolverConfig{
		Endpoint: "https://api.github.com/graphql",
		LookupEnv: func(string) string {
			return ""
		},
		GHToken: func(_ context.Context, endpoint string) (string, error) {
			if endpoint != "https://api.github.com/graphql" {
				t.Fatalf("endpoint = %q, want default GitHub GraphQL endpoint", endpoint)
			}
			return "gh-token", nil
		},
	})

	token, err := resolver.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "gh-token" {
		t.Fatalf("Token() = %q, want gh-token", token)
	}
}

func TestTokenResolverReportsMissingToken(t *testing.T) {
	t.Parallel()

	resolver := NewTokenResolver(TokenResolverConfig{
		LookupEnv: func(string) string {
			return ""
		},
	})

	_, err := resolver.Token(context.Background())
	if !errors.Is(err, ErrMissingToken) {
		t.Fatalf("Token() error = %v, want ErrMissingToken", err)
	}
}

func TestTokenResolverFallsBackToPAT(t *testing.T) {
	t.Parallel()

	resolver := NewTokenResolver(TokenResolverConfig{
		APIKey: "$GITHUB_TOKEN",
		LookupEnv: func(key string) string {
			if key == "GITHUB_TOKEN" {
				return "pat-token"
			}
			return ""
		},
		GHToken: func(context.Context, string) (string, error) {
			t.Fatal("gh token fallback should not run")
			return "", nil
		},
	})

	token, err := resolver.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "pat-token" {
		t.Fatalf("Token() = %q, want pat-token", token)
	}
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}

func decodeJWTSegment(t *testing.T, segment string) map[string]any {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return decoded
}
