package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresProjectIconReplay(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	var logs bytes.Buffer
	service.config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	service.config.Icons = &memoryProjectIcons{objects: map[string][]byte{}}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	service.echo.GET(nativeBase+"/local-projects", func(c echo.Context) error {
		projects, err := service.localAppProjects(c.Request().Context(), nativeRequestScope(c).credential)
		if err != nil {
			return service.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, projects)
	}, service.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	service.registerNativeRoutes(service.echo)
	service.registerOperatorTools(service.echo)
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedStartupOrganization(t, repository, org)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0) ON CONFLICT(organization_id,name) DO UPDATE SET value=0`, org); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin_" + string(org), Name: "Example admin", Hash: apikey.HashToken(string(org)), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: tracker.ProjectID("project_" + string(org)), Name: "Example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			base := "/api/v2/organizations/" + org + "/projects/project_" + org
			service.localOrganization = org
			for i, icon := range []projecticon.Icon{{Kind: "emoji", Emoji: "🚀"}, {Kind: "automatic"}, {Kind: "image"}, {Kind: "automatic"}} {
				request := map[string]any{"idempotency_key": fmt.Sprintf("icon-%d", i), "expected_revision": fmt.Sprint(i + 1), "icon": icon}
				if icon.Kind == "image" {
					request["content_base64"] = imageBytes.Bytes()
				}
				response := performHubAPIRequest(t, service, http.MethodPut, base+"/icon", org, request)
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
				}
				var saved ProjectIntegration
				decodeHubResponse(t, response, &saved)
				if saved.Revision != tracker.Revision(i+2) || (icon.Kind == "automatic" && saved.Icon != nil) || (icon.Kind != "automatic" && (saved.Icon == nil || saved.Icon.Kind != icon.Kind || saved.Icon.Emoji != icon.Emoji)) {
					t.Fatalf("saved=%+v", saved)
				}
				read := performHubAPIRequest(t, service, http.MethodGet, base+"/integration", org, nil)
				requireNativeStatus(t, read, http.StatusOK)
				var current ProjectIntegration
				decodeHubResponse(t, read, &current)
				if current.Revision != saved.Revision || !reflect.DeepEqual(current.Icon, saved.Icon) {
					t.Fatalf("persisted=%+v saved=%+v", current, saved)
				}
				call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "get_project_integration", "arguments": map[string]any{"project_id": "project_" + org}, "_meta": json.RawMessage(fleetProtocolMeta)}}
				mcp := performHubWorkCall(t, service, "/api/v2/organizations/"+org+"/mcp", org, "get_project_integration", call)
				requireNativeStatus(t, mcp, http.StatusOK)
				var envelope struct {
					Result struct {
						IsError bool `json:"isError"`
						Data    struct {
							Integration ProjectIntegration `json:"data"`
						} `json:"structuredContent"`
					} `json:"result"`
				}
				decodeHubResponse(t, mcp, &envelope)
				if envelope.Result.IsError || envelope.Result.Data.Integration.Revision != saved.Revision || !reflect.DeepEqual(envelope.Result.Data.Integration.Icon, saved.Icon) {
					t.Fatalf("MCP integration=%s", mcp.Body.String())
				}
				local := performHubAPIRequest(t, service, http.MethodGet, base+"/local-projects", org, nil)
				requireNativeStatus(t, local, http.StatusOK)
				var projects []appBootstrapProject
				decodeHubResponse(t, local, &projects)
				if len(projects) != 1 || projects[0].ID != "project_"+org || !reflect.DeepEqual(projects[0].Icon, saved.Icon) {
					t.Fatalf("local projects=%+v", projects)
				}
				foreignReader := "org_first"
				if org == foreignReader {
					foreignReader = "org_second"
				}
				requireNativeStatus(t, performHubWorkCall(t, service, "/api/v2/organizations/"+org+"/mcp", foreignReader, "get_project_integration", call), http.StatusUnauthorized)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, base+"/local-projects", foreignReader, nil), http.StatusUnauthorized)
				freshnessCall := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "repository_freshness", "arguments": map[string]any{"project_id": "project_" + org}, "_meta": json.RawMessage(fleetProtocolMeta)}}
				freshness := performHubWorkCall(t, service, "/api/v2/organizations/"+org+"/mcp", org, "repository_freshness", freshnessCall)
				requireNativeStatus(t, freshness, http.StatusOK)
				var health struct {
					Result struct {
						IsError bool `json:"isError"`
						Data    struct {
							Organization string                      `json:"organization_id"`
							Health       repositoryFreshnessResponse `json:"data"`
						} `json:"structuredContent"`
					} `json:"result"`
				}
				decodeHubResponse(t, freshness, &health)
				if health.Result.IsError || health.Result.Data.Organization != org || health.Result.Data.Health.Repositories == nil || len(health.Result.Data.Health.Repositories) != 0 {
					t.Fatalf("freshness=%s", freshness.Body.String())
				}
				replay := performHubAPIRequest(t, service, http.MethodPut, base+"/icon", org, request)
				requireNativeStatus(t, replay, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatal("replay did not return original result")
				}
				foreign := "org_first"
				if org == foreign {
					foreign = "org_second"
				}
				denied := performHubAPIRequest(t, service, http.MethodPut, base+"/icon", foreign, request)
				if denied.Code != http.StatusUnauthorized && denied.Code != http.StatusForbidden && denied.Code != http.StatusNotFound {
					t.Fatalf("cross-organization status=%d body=%s", denied.Code, denied.Body.String())
				}
			}
			arguments := map[string]any{"organization_id": org, "project_id": "project_" + org, "icon": projecticon.Icon{Kind: "image"}, "content_base64": append(append([]byte{}, imageBytes.Bytes()...), []byte("discarded-metadata")...), "expected_revision": "5", "request_id": "mcp-icon"}
			call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "set_project_icon", "arguments": arguments, "_meta": json.RawMessage(fleetProtocolMeta)}}
			var original json.RawMessage
			for i := 0; i < 2; i++ {
				response := performHubWorkCall(t, service, "/api/v2/organizations/"+org+"/mcp", org, "set_project_icon", call)
				requireNativeStatus(t, response, http.StatusOK)
				var envelope struct {
					Result struct {
						IsError bool            `json:"isError"`
						Data    json.RawMessage `json:"structuredContent"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &envelope)
				if envelope.Result.IsError || len(envelope.Result.Data) == 0 {
					t.Fatalf("MCP=%s logs=%s", response.Body.String(), logs.String())
				}
				if i == 0 {
					original = envelope.Result.Data
				} else if !bytes.Equal(original, envelope.Result.Data) {
					t.Fatalf("MCP replay changed: %s != %s", original, envelope.Result.Data)
				}
			}
			logoBase := "/api/v2/organizations/" + org
			for i, remove := range []bool{false, true} {
				request := map[string]any{"idempotency_key": fmt.Sprintf("logo-%d", i), "expected_revision": fmt.Sprint(i + 1), "remove": remove}
				if !remove {
					request["content_base64"] = imageBytes.Bytes()
				}
				response := performHubAPIRequest(t, service, http.MethodPut, logoBase+"/logo", org, request)
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
				}
				var logo organizationLogo
				decodeHubResponse(t, response, &logo)
				if logo.Revision != tracker.Revision(i+2) || (logo.ImageKey == "") != remove {
					t.Fatalf("logo=%+v", logo)
				}
				replay := performHubAPIRequest(t, service, http.MethodPut, logoBase+"/logo", org, request)
				requireNativeStatus(t, replay, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatal("logo replay changed")
				}
				if !remove {
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, logoBase+"/attachment-metadata/"+logo.ImageKey, org, nil), http.StatusOK)
				}
			}

		})
	}
}
