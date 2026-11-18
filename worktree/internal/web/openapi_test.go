package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

	server := newConfiguredOpenAPITestServer(t, openAPITestConfig())
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
}

func TestServerOpenAPIIssueExplanationSchemaVersion(t *testing.T) {
	t.Parallel()

	server := newConfiguredOpenAPITestServer(t, openAPITestConfig())
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
	for _, name := range []string{"dependencies", "dependency_notes"} {
		property := explanation.Value.Properties[name]
		if property == nil || property.Value == nil || property.Value.Items == nil {
			t.Fatalf("missing array schema for %s", name)
		}
	}
	dependency := document.Components.Schemas["IssueDependency"]
	for _, name := range []string{"identifier", "id", "state", "tracker_state", "source", "human_owned", "human_completion_ready"} {
		if dependency == nil || dependency.Value == nil || dependency.Value.Properties[name] == nil {
			t.Fatalf("missing dependency property %s", name)
		}
	}
	reasons, ok := explanation.Value.Properties["reasons"]
	if !ok || reasons == nil || reasons.Value == nil || reasons.Value.Items == nil || reasons.Value.Items.Value == nil {
		t.Fatal("OpenAPI IssueExplanation reasons property is missing")
	}
	if got := reasons.Value.Items.Ref; got != "#/components/schemas/IssueExplanationReason" {
		t.Fatalf("OpenAPI IssueExplanation reasons ref = %q", got)
	}
}

func TestServerOpenAPIOmitsRetiredIssueProgressCredit(t *testing.T) {
	t.Parallel()

	server := newConfiguredOpenAPITestServer(t, openAPITestConfig())
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	document := parseOpenAPI(t, requestOpenAPI(t, httpServer.URL))

	if document.Paths.Find("/api/v1/projects/{project_id}/issues/progress-credit") != nil {
		t.Fatal("OpenAPI includes the retired issue progress credit operation")
	}
	if _, ok := document.Components.Schemas["IssueProgressCredit"]; ok {
		t.Fatal("OpenAPI includes the retired issue progress credit schema")
	}
}

func assertOpenAPISchemaVersion(t *testing.T, values []any) {
	t.Helper()
	want := strconv.Itoa(explain.SchemaVersion)
	if len(values) != 1 || fmt.Sprint(values[0]) != want {
		t.Fatalf("OpenAPI issue explanation schema versions = %v, want [%s]", values, want)
	}
}

func TestServerOpenAPIBypassesPrivateDashboardToken(t *testing.T) {
	t.Parallel()

	cfg := openAPITestConfig()
	cfg.GlobalConfig.DashboardAccess = globalconfig.DashboardAccess{
		Mode:  globalconfig.DashboardAccessModePrivateToken,
		Token: "private-dashboard-token",
	}
	server := newConfiguredOpenAPITestServer(t, cfg)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	parseOpenAPI(t, requestOpenAPI(t, httpServer.URL))
}

func openAPITestConfig() web.Config {
	return web.Config{
		Mode:          web.ModeOnboarding,
		ServerAddress: "0.0.0.0:8443",
		DashboardURL:  "https://private-dashboard.example.invalid/operator",
		LookupEnv: func(key string) string {
			if key == "DETENT_API_TOKEN" {
				return openAPIRuntimeToken
			}
			return ""
		},
	}
}

func newConfiguredOpenAPITestServer(t *testing.T, cfg web.Config) *web.Server {
	t.Helper()
	server, err := web.NewServer(cfg, web.Dependencies{})
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
