package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

func TestPooledHTTPClientReusesSequentialConnections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		requests int
	}{
		{name: "two requests", requests: 2},
		{name: "five requests", requests: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
			}))
			t.Cleanup(server.Close)

			httpClient := NewPooledHTTPClient(HTTPTransportConfig{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     time.Minute,
			})
			t.Cleanup(httpClient.CloseIdleConnections)

			client, err := NewClient(ClientConfig{
				Endpoint:    server.URL,
				TokenSource: StaticTokenSource("test-token"),
				HTTPClient:  httpClient,
			})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			var freshConns int
			var reusedConns int
			for i := 0; i < tt.requests; i++ {
				trace := &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) {
						if info.Reused {
							reusedConns++
							return
						}
						freshConns++
					},
				}
				ctx := httptrace.WithClientTrace(context.Background(), trace)
				if err := client.GraphQL(ctx, "query { viewer { login } }", nil, nil); err != nil {
					t.Fatalf("GraphQL() request %d error = %v", i+1, err)
				}
			}

			if freshConns != 1 {
				t.Fatalf("fresh connections = %d, want 1", freshConns)
			}
			if reusedConns != tt.requests-1 {
				t.Fatalf("reused connections = %d, want %d", reusedConns, tt.requests-1)
			}
			if got := httpClient.LiveConnections(); got != 1 {
				t.Fatalf("LiveConnections() = %d, want 1", got)
			}
		})
	}
}

func TestConnectorUsesOnePooledClientForGitHubAppAndGraphQL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	privateKey := testPooledClientPrivateKeyPEM(t)
	var requests int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/app/installations/987/access_tokens":
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(map[string]string{
				"token":      "installation-token",
				"expires_at": now.Add(time.Hour).Format(time.RFC3339),
			}); err != nil {
				t.Fatalf("Encode() token response error = %v", err)
			}
		case "/graphql":
			_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"},"node":{"__typename":"ProjectV2","id":"PVT_1"}}}`))
		default:
			t.Fatalf("unexpected path = %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	connector, err := NewConnector(Config{
		Endpoint:                server.URL + "/graphql",
		GitHubAppID:             "123",
		GitHubAppInstallationID: "987",
		GitHubAppPrivateKey:     privateKey,
		ProjectSlug:             "PVT_1",
		HTTPTransport: HTTPTransportConfig{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     time.Minute,
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	if err := connector.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := atomic.LoadInt64(&requests); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := connector.LiveConnections(); got != 1 {
		t.Fatalf("LiveConnections() = %d, want 1", got)
	}
}

func testPooledClientPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return string(pem.EncodeToMemory(block))
}
