package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

const mcpInitializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`

func TestRemoteMCPRequiresCurrentReadAuthority(t *testing.T) {
	t.Parallel()

	server, backend := newRemoteMCPTestServer(t, nil)
	readToken, _ := createRemoteMCPKey(t, server, "Global read", []string{"read"}, nil)
	writeToken, _ := createRemoteMCPKey(t, server, "Global write", []string{"write"}, nil)
	adminToken, _ := createRemoteMCPKey(t, server, "Global admin", []string{"admin"}, nil)
	projectToken, _ := createRemoteMCPKey(t, server, "Project read", []string{"read"}, []string{"detent"})
	revokedToken, revokedID := createRemoteMCPKey(t, server, "Revoked read", []string{"read"}, nil)
	if err := backend.RevokeAPIKey(context.Background(), revokedID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}
	expiredToken := createExpiredAPIKey(t, backend)

	tests := []struct {
		name       string
		token      string
		wantStatus int
		wantCode   string
	}{
		{name: "global read", token: readToken, wantStatus: http.StatusOK},
		{name: "missing", wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
		{name: "static API token", token: "detent_admin_token", wantStatus: http.StatusOK},
		{name: "revoked", token: revokedToken, wantStatus: http.StatusUnauthorized, wantCode: "token_revoked"},
		{name: "expired", token: expiredToken, wantStatus: http.StatusUnauthorized, wantCode: "token_expired"},
		{name: "write scope", token: writeToken, wantStatus: http.StatusOK},
		{name: "admin scope", token: adminToken, wantStatus: http.StatusOK},
		{name: "project scoped", token: projectToken, wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := map[string]string{}
			if test.token != "" {
				headers["Authorization"] = "Bearer " + test.token
			}
			recorder := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, headers)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantCode == "" {
				if recorder.Header().Get("Mcp-Session-Id") == "" {
					t.Fatal("successful initialization omitted Mcp-Session-Id")
				}
				return
			}
			var problem struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode auth problem: %v; body = %s", err, recorder.Body.String())
			}
			if problem.Error.Code != test.wantCode {
				t.Fatalf("problem code = %q, want %q", problem.Error.Code, test.wantCode)
			}
			if test.token != "" && strings.Contains(recorder.Body.String(), test.token) {
				t.Fatalf("response leaked bearer token: %s", recorder.Body.String())
			}
		})
	}
}

func TestRemoteMCPBypassesDashboardAuthenticationGates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config globalconfig.Config
	}{
		{
			name: "magic link",
			config: globalconfig.Config{Auth: globalconfig.Auth{
				Mode:          globalconfig.AuthModeMagicLink,
				AllowedEmails: []string{"operator@example.com"},
				LinkTTL:       "15m",
				SessionTTL:    "1h",
			}},
		},
		{
			name: "private dashboard",
			config: globalconfig.Config{DashboardAccess: globalconfig.DashboardAccess{
				Mode:  globalconfig.DashboardAccessModePrivateToken,
				Token: privateDashboardTestToken(9),
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, _ := newRemoteMCPTestServerWithConfig(t, nil, test.config, nil)
			token, _ := createRemoteMCPKey(t, server, "Global read", []string{"read"}, nil)
			authorized := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, map[string]string{
				"Authorization": "Bearer " + token,
			})
			if authorized.Code != http.StatusOK || authorized.Header().Get("Mcp-Session-Id") == "" {
				t.Fatalf("authorized response = %d %s, want initialized MCP session", authorized.Code, authorized.Body.String())
			}
			unauthorized := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, nil)
			if unauthorized.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized response = %d %s, want 401", unauthorized.Code, unauthorized.Body.String())
			}
		})
	}
}

func TestRemoteMCPBearerTokenIsNotLoggedOrEchoed(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	server, backend := newRemoteMCPTestServer(t, logger)
	token, keyID := createRemoteMCPKey(t, server, "Revoked secret", []string{"read"}, nil)
	if err := backend.RevokeAPIKey(context.Background(), keyID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}
	recorder := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("bearer token appeared in response or logs")
	}
}

func TestRemoteMCPUsesExistingRateLimits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	server, _ := newRemoteMCPTestServerWithConfig(t, nil, globalconfig.Config{}, func() time.Time { return now })
	token, _ := createRemoteMCPKey(t, server, "Rate limited read", []string{"read"}, nil)
	var limited bool
	for range 40 {
		recorder := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, map[string]string{
			"Authorization": "Bearer " + token,
		})
		if recorder.Code != http.StatusTooManyRequests {
			continue
		}
		limited = true
		if recorder.Header().Get("X-RateLimit-Limit") == "" || recorder.Header().Get("X-RateLimit-Remaining") == "" || recorder.Header().Get("Retry-After") == "" {
			t.Fatalf("rate limit response omitted headers: %#v", recorder.Header())
		}
		break
	}
	if !limited {
		t.Fatal("MCP rate limit did not engage")
	}
}

func TestRemoteMCPUsesWebServerShutdown(t *testing.T) {
	t.Parallel()

	server, _ := newRemoteMCPTestServer(t, nil)
	token, _ := createRemoteMCPKey(t, server, "Shutdown read", []string{"read"}, nil)
	initialize := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if initialize.Code != http.StatusOK || initialize.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize response = %d %s", initialize.Code, initialize.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	afterShutdown := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, map[string]string{
		"Authorization": "Bearer " + token,
	})
	if afterShutdown.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-shutdown response = %d %s, want 503", afterShutdown.Code, afterShutdown.Body.String())
	}
}

func newRemoteMCPTestServer(t *testing.T, logger *slog.Logger) (*web.Server, store.Store) {
	t.Helper()
	return newRemoteMCPTestServerWithConfig(t, logger, globalconfig.Config{}, nil)
}

func newRemoteMCPTestServerWithConfig(t *testing.T, logger *slog.Logger, config globalconfig.Config, now func() time.Time) (*web.Server, store.Store) {
	t.Helper()
	backend := openWebTestStore(t)
	deps := testDeps(t)
	deps.Store = backend
	deps.MagicLinkSender = &webAuthSender{}
	config.APIToken = "detent_admin_token"
	server, err := web.NewServer(web.Config{
		Logger:        logger,
		ServerAddress: "127.0.0.1:0",
		GlobalConfig:  config,
		Now:           now,
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})
	return server, backend
}

func createRemoteMCPKey(t *testing.T, server *web.Server, name string, scopes []string, projects []string) (string, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":        name,
		"scopes":      scopes,
		"project_ids": projects,
		"expires_in":  "90d",
	})
	if err != nil {
		t.Fatalf("marshal API key request: %v", err)
	}
	recorder := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/keys", string(body), map[string]string{
		"Authorization": "Bearer detent_admin_token",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create API key status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var created struct {
		Token string `json:"token"`
		Key   struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode API key response: %v", err)
	}
	if created.Token == "" || created.Key.ID == "" {
		t.Fatalf("API key response omitted token or ID: %s", recorder.Body.String())
	}
	return created.Token, created.Key.ID
}

func TestRemoteMCPAcceptsXAPIKeyHeader(t *testing.T) {
	t.Parallel()

	server, _ := newRemoteMCPTestServer(t, nil)
	token, _ := createRemoteMCPKey(t, server, "Header read", []string{"read"}, nil)
	recorder := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, map[string]string{
		"X-API-Key": token,
	})
	if recorder.Code != http.StatusOK || recorder.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("response = %d %s, want initialized MCP session", recorder.Code, recorder.Body.String())
	}
}

// A direct call must enforce current grants without discovery, and aggregates
// must not include another project's usage, counts or unscoped event text.
func TestRemoteMCPCurrentProjectAuthority(t *testing.T) {
	t.Parallel()
	backend := openWebTestStore(t)
	deps := testDeps(t)
	deps.Store = backend
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Now(),
		BoardIssues: []telemetry.Issue{{ID: "visible", ProjectID: "allowed", Title: "visible", BlockedBy: []telemetry.BlockedRef{{ID: "confidential", Identifier: "confidential"}}, DependencyNotes: []string{"confidential"}}, {ID: "confidential", ProjectID: "other", Title: "confidential"}},
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "allowed"}, Counts: telemetry.Counts{Queue: 1}, Tokens: telemetry.Tokens{Total: 7}},
			{Project: telemetry.Project{ID: "other"}, Counts: telemetry.Counts{Queue: 99}, Tokens: telemetry.Tokens{Total: 999}},
		},
		Counts: telemetry.Counts{Queue: 100}, Tokens: telemetry.Tokens{Total: 1006},
		Events: []telemetry.ActivityEvent{{Message: "confidential"}},
	}); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token"}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	token, keyID := createRemoteMCPKey(t, server, "Project read", []string{"read"}, []string{"allowed"})
	headers := map[string]string{"Authorization": "Bearer " + token}
	initialized := performJSON(t, server.Handler(), http.MethodPost, "/mcp", mcpInitializeRequest, headers)
	if initialized.Code != http.StatusOK {
		t.Fatal(initialized.Body.String())
	}
	headers["Mcp-Session-Id"] = initialized.Header().Get("Mcp-Session-Id")
	performJSON(t, server.Handler(), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers)
	for _, test := range []struct {
		name, tool, arguments string
		denied                bool
	}{
		{"board", "board_state", `{}`, false},
		{"fleet", "fleet_health", `{}`, false},
		{"usage", "telemetry_usage", `{}`, false},
		{"activity", "recent_activity", `{}`, false},
		{"explanation", "explain_item", `{"project_id":"allowed","reference":"visible"}`, false},
		{"foreign project", "board_state", `{"project_id":"other"}`, true},
		{"foreign explanation", "explain_item", `{"project_id":"other","reference":"confidential"}`, true},
		{"forged context", "board_state", `{"organization_id":"other"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": test.tool, "arguments": json.RawMessage(test.arguments)}})
			if err != nil {
				t.Fatal(err)
			}
			response := performJSON(t, server.Handler(), http.MethodPost, "/mcp", string(body), headers)
			if response.Code != http.StatusOK {
				t.Fatalf("%d: %s", response.Code, response.Body.String())
			}
			var result struct {
				Result struct {
					IsError    bool            `json:"isError"`
					Structured json.RawMessage `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Result.IsError != test.denied || strings.Contains(response.Body.String(), "confidential") {
				t.Fatalf("%s", response.Body.String())
			}
			if test.tool == "telemetry_usage" && !test.denied && !bytes.Contains(result.Result.Structured, []byte(`"total_tokens":7`)) {
				t.Fatalf("%s", response.Body.String())
			}
			// The stdio bridge has the same check even without an MCP session.
			bridge := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+test.tool, test.arguments, map[string]string{"Authorization": "Bearer " + token})
			if strings.Contains(bridge.Body.String(), "confidential") || test.denied && bridge.Code < 400 || !test.denied && bridge.Code != http.StatusOK {
				t.Fatalf("bridge %d %s", bridge.Code, bridge.Body.String())
			}
		})
	}
	if err := backend.RevokeAPIKey(t.Context(), keyID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"board_state","arguments":{}}}`} {
		response := performJSON(t, server.Handler(), http.MethodPost, "/mcp", body, headers)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "confidential") {
			t.Fatalf("revoked: %d %s", response.Code, response.Body.String())
		}
	}
}
