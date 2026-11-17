package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDevRuntimeAuthConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		email        string
		smtp         string
		from         string
		issuer       string
		clientID     string
		clientSecret string
		wantMode     string
		wantErr      bool
	}{
		{name: "disabled"},
		{name: "magic link", email: "operator@example.com", smtp: "127.0.0.1:2525", from: "detent@example.com", wantMode: "magic_link"},
		{name: "oidc", email: "operator@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", clientSecret: "secret", wantMode: "oidc"},
		{name: "mixed providers", email: "operator@example.com", smtp: "127.0.0.1:2525", from: "detent@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", clientSecret: "secret", wantErr: true},
		{name: "incomplete oidc", email: "operator@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := devRuntimeAuthConfig(tt.email, tt.smtp, tt.from, tt.issuer, tt.clientID, tt.clientSecret)
			if (err != nil) != tt.wantErr {
				t.Fatalf("devRuntimeAuthConfig() error = %v, want error %t", err, tt.wantErr)
			}
			if cfg.Mode != tt.wantMode {
				t.Fatalf("devRuntimeAuthConfig().Mode = %q, want %q", cfg.Mode, tt.wantMode)
			}
		})
	}
}

func waitForDashboardHeader(t *testing.T, rawURL string, done <-chan error, scenario string) string {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastErr error
	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before dashboard response: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("X-Detent-Demo-Scenario", scenario)
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			lastErr = errors.New(resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out fetching %s with scenario %s: %v", rawURL, scenario, lastErr)
	return ""
}

func postRuntimeRefresh(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before refresh: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/refresh", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting runtime refresh to %s", url)
}

func postRuntimeKanbanForm(t *testing.T, rawURL string, done <-chan error, form url.Values) string {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before Kanban action: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			t.Fatalf("Kanban action status = %d, want 200; body = %s", resp.StatusCode, string(body))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting Kanban action to %s", rawURL)
	return ""
}

func newRuntimeHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{}, Timeout: time.Second}
}
