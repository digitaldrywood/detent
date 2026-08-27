package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/web"
)

const (
	openAPIPath         = "/api/v1/openapi.yaml"
	openAPIMediaType    = "application/vnd.oai.openapi+yaml;version=3.0"
	openAPIRuntimeToken = "runtime-token-must-not-appear"
)

func TestServerOpenAPIEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode web.Mode
		deps func(*testing.T) web.Dependencies
	}{
		{name: "running", mode: web.ModeRunning, deps: testDeps},
		{name: "onboarding", mode: web.ModeOnboarding, deps: func(*testing.T) web.Dependencies { return web.Dependencies{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newOpenAPITestServer(t, tt.mode, tt.deps(t))
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)

			first := requestOpenAPI(t, httpServer.URL)
			second := requestOpenAPI(t, httpServer.URL)
			if string(first) != string(second) {
				t.Fatal("OpenAPI response changed between identical requests")
			}
			for _, privateValue := range []string{
				openAPIRuntimeToken,
				"private-dashboard.example.invalid",
				"private-hostname.example.invalid",
				"request-host.example.invalid",
				"request-header-must-not-appear",
			} {
				if strings.Contains(string(first), privateValue) {
					t.Fatalf("OpenAPI response contains runtime value %q", privateValue)
				}
			}
			document := parseOpenAPI(t, first)
			if got, want := document.Info.Version, "1.0.0"; got != want {
				t.Fatalf("OpenAPI info.version = %q, want %q", got, want)
			}
			if len(document.Servers) != 1 || document.Servers[0].URL != "/" {
				t.Fatalf("OpenAPI servers = %#v, want one relative root URL", document.Servers)
			}
		})
	}
}

func TestServerOpenAPIIssueExplanationSchemaVersion(t *testing.T) {
	t.Parallel()

	server := newOpenAPITestServer(t, web.ModeRunning, testDeps(t))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	document := parseOpenAPI(t, requestOpenAPI(t, httpServer.URL))

	path := document.Paths.Find("/api/v1/projects/{project_id}/issues/explanation")
	if path == nil || path.Get == nil {
		t.Fatal("OpenAPI issue explanation operation is missing")
	}
	operation := path.Get
	parameter := operation.Parameters.GetByInAndName("query", "schema")
	if parameter == nil || parameter.Schema == nil || parameter.Schema.Value == nil {
		t.Fatal("OpenAPI issue explanation schema parameter is missing")
	}
	assertOpenAPISchemaVersion(t, parameter.Schema.Value.Enum)
	explanation, ok := document.Components.Schemas["IssueExplanation"]
	if !ok || explanation == nil || explanation.Value == nil {
		t.Fatal("OpenAPI IssueExplanation schema is missing")
	}
	responseSchema, ok := explanation.Value.Properties["schema"]
	if !ok || responseSchema == nil || responseSchema.Value == nil {
		t.Fatal("OpenAPI IssueExplanation schema version property is missing")
	}
	assertOpenAPISchemaVersion(t, responseSchema.Value.Enum)
}

func TestServerOpenAPIIssueProgressCredit(t *testing.T) {
	t.Parallel()

	server := newOpenAPITestServer(t, web.ModeRunning, testDeps(t))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	document := parseOpenAPI(t, requestOpenAPI(t, httpServer.URL))

	path := document.Paths.Find("/api/v1/projects/{project_id}/issues/progress-credit")
	if path == nil || path.Post == nil {
		t.Fatal("OpenAPI issue progress credit operation is missing")
	}
	response := path.Post.Responses.Value("200")
	if response == nil || response.Value == nil {
		t.Fatal("OpenAPI issue progress credit response is missing")
	}
	schema := response.Value.Content.Get("application/json").Schema
	if schema == nil || schema.Ref != "#/components/schemas/IssueProgressCredit" {
		t.Fatalf("OpenAPI issue progress credit schema = %#v", schema)
	}
}

func assertOpenAPISchemaVersion(t *testing.T, values []any) {
	t.Helper()
	want := strconv.Itoa(explain.SchemaVersion)
	if len(values) != 1 || fmt.Sprint(values[0]) != want {
		t.Fatalf("OpenAPI issue explanation schema versions = %v, want [%s]", values, want)
	}
}

func TestServerOpenAPIDoesNotWeakenAPIAuthentication(t *testing.T) {
	t.Parallel()

	server := newOpenAPITestServer(t, web.ModeRunning, testDeps(t))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+"/api/v1/state", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/state error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/state status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /api/v1/state Content-Type = %q, want application/json", response.Header.Get("Content-Type"))
	}
}

func TestServerOpenAPIBypassesDashboardAuthentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T) (web.Config, web.Dependencies)
	}{
		{
			name: "magic link",
			setup: func(t *testing.T) (web.Config, web.Dependencies) {
				deps := testDeps(t)
				deps.Store = openWebTestStore(t)
				deps.MagicLinkSender = &webAuthSender{}
				cfg := openAPITestConfig()
				cfg.GlobalConfig.Auth = globalconfig.Auth{
					Mode:          globalconfig.AuthModeMagicLink,
					AllowedEmails: []string{"operator@example.com"},
					LinkTTL:       "15m",
					SessionTTL:    "1h",
				}
				return cfg, deps
			},
		},
		{
			name: "OIDC",
			setup: func(t *testing.T) (web.Config, web.Dependencies) {
				deps := testDeps(t)
				deps.Store = openWebTestStore(t)
				deps.IdentityProvider = &webOIDCProvider{}
				cfg := openAPITestConfig()
				cfg.GlobalConfig.Auth = globalconfig.Auth{
					Mode:          globalconfig.AuthModeOIDC,
					AllowedEmails: []string{"operator@example.com"},
					SessionTTL:    "1h",
					OIDC: globalconfig.OIDC{
						IssuerURL:    "https://issuer.example.invalid",
						ClientID:     "test-client",
						ClientSecret: "test-client-secret",
					},
				}
				return cfg, deps
			},
		},
		{
			name: "private dashboard token",
			setup: func(t *testing.T) (web.Config, web.Dependencies) {
				cfg := openAPITestConfig()
				cfg.GlobalConfig.DashboardAccess = globalconfig.DashboardAccess{
					Mode:  globalconfig.DashboardAccessModePrivateToken,
					Token: privateDashboardTestToken(12),
				}
				return cfg, testDeps(t)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, deps := tt.setup(t)
			server := newConfiguredOpenAPITestServer(t, cfg, deps)
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			parseOpenAPI(t, requestOpenAPI(t, httpServer.URL))
		})
	}
}

func TestServerOpenAPIRouteParity(t *testing.T) {
	t.Parallel()

	server := newOpenAPITestServer(t, web.ModeRunning, testDeps(t))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	document := parseOpenAPI(t, requestOpenAPI(t, httpServer.URL))

	registered := map[string]bool{}
	for _, route := range server.Echo().Routes() {
		if !strings.HasPrefix(route.Path, "/api/v1/") {
			continue
		}
		key := routeKey(route.Method, route.Path)
		registered[key] = true
	}

	documented := map[string][]string{}
	for path, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			echoPath, ok := operation.Extensions["x-detent-echo-path"].(string)
			if !ok || strings.TrimSpace(echoPath) == "" {
				t.Errorf("%s %s has no x-detent-echo-path", method, path)
				continue
			}
			key := routeKey(method, echoPath)
			documented[key] = append(documented[key], method+" "+path)
			if !registered[key] {
				t.Errorf("documented operation %s %s maps to unregistered route %s", method, path, key)
			}
			assertOpenAPIAccess(t, method, path, operation)
		}
	}

	excluded := map[string]string{
		routeKey(http.MethodPost, "/api/v1/projects/:project_id/budget/override"):   "HTMX project budget panel",
		routeKey(http.MethodDelete, "/api/v1/projects/:project_id/budget/override"): "HTMX project budget panel",
		routeKey(http.MethodGet, "/api/v1/keys/:id/rotate"):                         "HTML rotate dialog",
		routeKey(http.MethodGet, "/api/v1/projects/:project_id/runs/:attempt/stop"): "HTML stop-run dialog",
		routeKey(http.MethodGet, "/api/v1/ai-debug"):                                "private plain-text clipboard prompt",
		routeKey(http.MethodGet, "/api/v1/board/card"):                              "HTMX board card",
		routeKey(http.MethodGet, "/api/v1/board/card/core"):                         "HTMX board card core",
		routeKey(http.MethodGet, "/api/v1/board/receipt"):                           "HTMX board receipt",
		routeKey(http.MethodGet, "/api/v1/board/conversation"):                      "HTMX board conversation",
		routeKey(http.MethodGet, "/api/v1/board/activity"):                          "HTMX board activity",
		routeKey(http.MethodGet, "/api/v1/board/activity/events"):                   "SSE board activity stream",
		routeKey(http.MethodGet, "/api/v1/board/session"):                           "HTML live session",
		routeKey(http.MethodGet, "/api/v1/board/session/events"):                    "SSE live session stream",
		routeKey(http.MethodGet, "/api/v1/board/session/history"):                   "HTMX live session history",
		routeKey(http.MethodGet, "/api/v1/chat"):                                    "HTMX chat panel",
		routeKey(http.MethodPost, "/api/v1/chat/messages"):                          "HTMX chat conversation",
		routeKey(http.MethodPost, "/api/v1/chat/actions/:action_id/confirm"):        "HTMX chat conversation",
		routeKey(http.MethodPost, "/api/v1/chat/actions/:action_id/reject"):         "HTMX chat conversation",
		routeKey(http.MethodGet, "/api/v1/kanban/move"):                             "HTML move dialog",
		routeKey(http.MethodPost, "/api/v1/kanban/move"):                            "HTMX board snapshot",
		routeKey(http.MethodPost, "/api/v1/kanban/remove"):                          "HTMX board snapshot",
		routeKey(http.MethodGet, "/api/v1/kanban/comment"):                          "HTML comment dialog",
	}

	for key, reason := range excluded {
		if !registered[key] {
			t.Errorf("stale OpenAPI exclusion %s (%s)", key, reason)
		}
		if documented[key] != nil {
			t.Errorf("route %s is both documented and excluded (%s)", key, reason)
		}
	}
	for key := range registered {
		if documented[key] == nil && excluded[key] == "" {
			t.Errorf("registered JSON namespace route %s is neither documented nor explicitly excluded", key)
		}
	}

}

func TestServerOpenAPIRoutePrecedence(t *testing.T) {
	t.Parallel()

	server := newOpenAPITestServer(t, web.ModeRunning, testDeps(t))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+"/api/v1/projects/missing/state", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+openAPIRuntimeToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET project wildcard error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"code":"snapshot_unavailable"`) {
		t.Fatalf("project wildcard response = %d %s, want project handler ahead of issue catch-all", response.StatusCode, body)
	}
}

func TestServerOpenAPIOnboardingRouteDifferences(t *testing.T) {
	t.Parallel()

	server := newOpenAPITestServer(t, web.ModeOnboarding, web.Dependencies{})
	var apiRoutes []string
	for _, route := range server.Echo().Routes() {
		if strings.HasPrefix(route.Path, "/api/v1/") {
			apiRoutes = append(apiRoutes, routeKey(route.Method, route.Path))
		}
	}
	if !slices.Equal(apiRoutes, []string{routeKey(http.MethodGet, openAPIPath)}) {
		t.Fatalf("onboarding API routes = %v, want only public OpenAPI route", apiRoutes)
	}
}

func newOpenAPITestServer(t *testing.T, mode web.Mode, deps web.Dependencies) *web.Server {
	t.Helper()
	cfg := openAPITestConfig()
	cfg.Mode = mode
	return newConfiguredOpenAPITestServer(t, cfg, deps)
}

func openAPITestConfig() web.Config {
	return web.Config{
		ServerAddress: "0.0.0.0:8443",
		DashboardURL:  "https://private-dashboard.example.invalid/operator",
		LookupEnv: func(key string) string {
			if key == "DETENT_API_TOKEN" {
				return openAPIRuntimeToken
			}
			return ""
		},
		Hostname: func() (string, error) {
			return "private-hostname.example.invalid", nil
		},
	}
}

func newConfiguredOpenAPITestServer(t *testing.T, cfg web.Config, deps web.Dependencies) *web.Server {
	t.Helper()
	server, err := web.NewServer(cfg, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})
	return server
}

func requestOpenAPI(t *testing.T, baseURL string) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+openAPIPath, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Host = "request-host.example.invalid"
	request.Header.Set("X-Runtime-Header", "request-header-must-not-appear")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s error = %v", openAPIPath, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d: %s", openAPIPath, response.StatusCode, http.StatusOK, body)
	}
	if got := response.Header.Get("Content-Type"); got != openAPIMediaType {
		t.Fatalf("GET %s Content-Type = %q, want %q", openAPIPath, got, openAPIMediaType)
	}
	if got, want := response.Header.Get("Cache-Control"), "public, max-age=300"; got != want {
		t.Fatalf("GET %s Cache-Control = %q, want %q", openAPIPath, got, want)
	}
	if got, want := response.Header.Get("X-Detent-OpenAPI-Version"), "1.0.0"; got != want {
		t.Fatalf("GET %s version = %q, want %q", openAPIPath, got, want)
	}
	return body
}

func parseOpenAPI(t *testing.T, body []byte) *openapi3.T {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromData(body)
	if err != nil {
		t.Fatalf("OpenAPI parser error = %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("OpenAPI validation error = %v", err)
	}
	return document
}

func assertOpenAPIAccess(t *testing.T, method string, path string, operation *openapi3.Operation) {
	t.Helper()
	access, ok := operation.Extensions["x-detent-access"].(string)
	if !ok {
		t.Errorf("%s %s has no x-detent-access classification", method, path)
		return
	}
	if operation.Security == nil {
		t.Errorf("%s %s does not declare security", method, path)
	}
	scope, _ := operation.Extensions["x-detent-scope"].(string)
	switch access {
	case "public":
		if scope != "" {
			t.Errorf("%s %s public operation declares scope %q", method, path, scope)
		}
	case "read", "write", "admin":
		if scope != access {
			t.Errorf("%s %s access %q has scope %q", method, path, access, scope)
		}
	case "project-scoped":
		if _, ok := operation.Extensions["x-detent-project-parameter"].(string); !ok {
			t.Errorf("%s %s lacks project-scope parameter", method, path)
		}
		if scope != "" && scope != "write" {
			t.Errorf("%s %s project operation has scope %q", method, path, scope)
		}
	default:
		t.Errorf("%s %s has unknown access classification %q", method, path, access)
	}
}

func routeKey(method string, path string) string {
	return strings.ToUpper(method) + " " + path
}
