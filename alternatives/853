package github

import (
	"bytes"
	"context"

	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConnectorUsesDefaultLoggerForClientDiagnostics(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/digitaldrywood/detent/commits/abc/check-runs" {
			t.Fatalf("path = %s, want check-runs path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"check_runs":[]}`))
	}))
	t.Cleanup(server.Close)

	c, err := NewConnector(Config{
		Endpoint:         server.URL,
		APIKey:           "token",
		HTTPClient:       server.Client(),
		RESTDebugLogging: true,
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	if err := c.client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/commits/abc/check-runs", nil, nil); err != nil {
		t.Fatalf("REST() error = %v", err)
	}

	logText := logs.String()
	for _, fragment := range []string{
		"github rest request",
		"github rest response",
		`endpoint_family="check runs"`,
		"request_purpose=hydrate_pull_request_checks",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs missing %q:\n%s", fragment, logText)
		}
	}
}

func TestConnectorInstanceLoginConcurrentAuthenticateAndRead(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()

	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		login := "octocat-" + strconv.FormatInt(count.Add(1), 10)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"` + login + `"},"node":{"__typename":"ProjectV2","id":"PVT_1"}}}`))
	}))
	t.Cleanup(server.Close)

	c, err := NewConnector(Config{
		Endpoint:    server.URL,
		APIKey:      "token",
		ProjectSlug: "PVT_1",
		HTTPClient:  server.Client(),
		GHToken: func(context.Context, string) (string, error) {
			t.Fatal("gh token fallback should not run")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			for range 25 {
				if err := c.Authenticate(context.Background()); err != nil {
					t.Errorf("Authenticate() error = %v", err)
				}
			}
		})
	}
	wg.Go(func() {
		<-start
		for range 200 {
			_ = c.InstanceLogin()
		}
	})

	close(start)
	wg.Wait()
}

func TestConnectorAuthenticateReportsViewerProblems(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()

	tests := []struct {
		name    string
		project string
		body    string
		want    error
	}{
		{name: "viewer without project", body: `{"data":{"viewer":{"login":"example-user"}}}`},
		{name: "viewer missing", body: `{"data":{"viewer":null}}`, want: ErrAuthenticationFailed},
		{name: "viewer login missing", body: `{"data":{"viewer":{"login":""}}}`, want: ErrAuthenticationFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			c, err := NewConnector(Config{
				Endpoint:    server.URL,
				APIKey:      "token",
				ProjectSlug: tt.project,
				HTTPClient:  server.Client(),
				GHToken: func(context.Context, string) (string, error) {
					t.Fatal("gh token fallback should not run")
					return "", nil
				},
			})
			if err != nil {
				t.Fatalf("NewConnector() error = %v", err)
			}

			err = c.Authenticate(context.Background())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tt.want)
			}
		})
	}
}
