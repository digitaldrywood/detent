package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`

// Direct calls must enforce instance administrator authority even without
// discovery. Worker protocol credentials never gain operator tools; an absent
// real approval browser cannot be replaced by an admin bearer token.
func TestHubMCPFleetBoundary(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{GitHubRequestCounts: func() []GitHubRequestCount { return []GitHubRequestCount{} }})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	r.enroll(t)
	other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	other.enroll(t)
	runnerIDs := []string{r.binding.RunnerID, other.binding.RunnerID}
	slices.Sort(runnerIDs)
	contexts := make(chan context.Context, 1)
	// Non-hosted organization is bound by the application route parameter.
	f.service.echo.GET("/api/v2/organizations/:organization/fleet-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	for _, tt := range []struct {
		name, token string
		authorized  bool
	}{{"administrator", testHubAdminToken, true}, {"operator", f.token, false}, {"worker", r.redemption.Credential, false}} {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet-context-test"
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, tt.token, nil)
			var ctx context.Context
			select {
			case ctx = <-contexts:
			default:
			}
			if ctx == nil {
				if tt.name != "worker" || response.Code != http.StatusForbidden {
					t.Fatalf("context status=%d", response.Code)
				}
				return
			}
			ctx = operatortool.BindConnection(ctx, "fleet-boundary-"+tt.name, "direct-test")
			executor := hubFleetExecutor{service: f.service}
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{operatortool.InstanceHealth, operatortool.NativeCapabilities, operatortool.OutboxHealth, operatortool.GetRunnerRouting, operatortool.GetRunnerCapacity, operatortool.GetRunnerUpdate, operatortool.ListRunnerRouting, operatortool.GitHubRequestCounts} {
				args := map[string]any{}
				if name == operatortool.GetRunnerRouting || name == operatortool.GetRunnerCapacity || name == operatortool.GetRunnerUpdate {
					args["runner_id"] = r.binding.RunnerID
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
				allowed := tt.authorized
				if name == operatortool.InstanceHealth || name == operatortool.NativeCapabilities {
					allowed = true
				}
				if (err == nil) != allowed {
					t.Fatalf("%s result=%s error=%v", name, result.Content, err)
				}
			}
			if tt.authorized {
				for _, page := range []struct {
					name      string
					arguments string
					ids       []string
					more      bool
				}{
					{name: "default page", arguments: `{}`, ids: runnerIDs},
					{name: "truncated page", arguments: `{"limit":1}`, ids: runnerIDs[:1], more: true},
					{name: "last page", arguments: `{"limit":1,"offset":1}`, ids: runnerIDs[1:]},
					{name: "empty page", arguments: `{"limit":1,"offset":2}`, ids: []string{}},
				} {
					t.Run(page.name, func(t *testing.T) {
						result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.ListRunnerRouting, Arguments: json.RawMessage(page.arguments)})
						if err != nil {
							t.Fatal(err)
						}
						var response struct {
							Data struct {
								Runners []runnerauth.Runner `json:"runners"`
								HasMore bool                `json:"has_more"`
							} `json:"data"`
						}
						if err := json.Unmarshal(result.Content, &response); err != nil {
							t.Fatal(err)
						}
						ids := []string{}
						for _, runner := range response.Data.Runners {
							ids = append(ids, runner.RunnerID)
						}
						if response.Data.Runners == nil || !slices.Equal(ids, page.ids) || response.Data.HasMore != page.more {
							t.Fatalf("page=%s want runner IDs=%v has_more=%t", result.Content, page.ids, page.more)
						}
					})
				}
			}
			args := json.RawMessage(`{"request_id":"revoke","runner_id":"` + r.binding.RunnerID + `"}`)
			if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.RevokeRunnerIdentity, Arguments: args}); err == nil {
				t.Fatal("bearer credential replaced real human approval")
			}
			var revoked int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens t JOIN runner_identities r ON r.token_id=t.id WHERE r.id=? AND t.revoked_at IS NOT NULL", r.binding.RunnerID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if revoked != 0 {
				t.Fatal("unapproved credential revoked")
			}
		})
	}
}
