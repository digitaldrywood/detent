package hubserver

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`

// Exercise the real browser authority, form and shared command in both entry
// modes. Catch removed runner grants, stale targets, self-approval, lost original
// principal attribution, and repeat enrollment/credential effects.
func TestHostedMCPFleetControls(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, scenario := range []string{"reads", "enrollment", "revoke enrollment", "revoke identity", "routing", "host", "ordinary", "revoked grants", "stale", "viewer", "YOLO", "different approver", "member runner grants"} {
			t.Run(deployment+"/"+scenario, func(t *testing.T) {
				var f hostedSecurityFixture
				var shared hostedSharedFixture
				if deployment == "shared" {
					shared = newHostedSharedFixture(t)
					f = shared.hostedSecurityFixture
				} else {
					f = newHostedSecurityFixture(t)
				}
				role := "owner"
				if scenario == "member runner grants" {
					role = "member"
				}
				if scenario == "viewer" {
					role = "viewer"
				}
				user := f.user(t, "owner", role, "operator@example.test", "write", "")
				f.grant(t, user, true, true)
				request := func(actor hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
					headers := map[string]string{}
					if path == "/mcp" {
						headers["Mcp-Protocol-Version"] = "2026-07-28"
						headers["Mcp-Method"] = "tools/call"
						if payload, ok := body.(map[string]any); ok {
							if params, ok := payload["params"].(map[string]any); ok {
								headers["Mcp-Name"], _ = params["name"].(string)
							}
						}
					}
					if deployment == "dedicated" {
						if path != "/mcp" {
							return f.request(t, actor, method, path, body)
						}
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
						req.Header.Set("Content-Type", "application/json")
						req.AddCookie(&http.Cookie{Name: hostedCookie, Value: actor.token})
						req.Header.Set("X-CSRF-Token", hostedCSRF(actor.token))
						for key, value := range headers {
							req.Header.Set(key, value)
						}
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, req)
						return response
					}
					input := hostedSharedRequest{headers: headers, user: &actor, method: method, target: "/organizations/org_security" + path, csrf: cloudassert.CSRFToken("shared-"+actor.identity.Subject, "org_security")}
					if form, ok := body.(url.Values); ok {
						input.form = true
						input.body = form.Encode()
					} else if body != nil {
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						input.body = string(raw)
					}
					return shared.serve(t, input)
				}
				call := func(name string, args any) json.RawMessage {
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					var meta map[string]any
					if err := json.Unmarshal([]byte(fleetProtocolMeta), &meta); err != nil {
						t.Fatal(err)
					}
					response := request(user, http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": json.RawMessage(raw), "_meta": meta}})
					var envelope struct {
						Result struct {
							Data json.RawMessage `json:"structuredContent"`
						} `json:"result"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
						t.Fatalf("call %s: %d %s", name, response.Code, response.Body.String())
					}
					return envelope.Result.Data
				}
				_, _ = call, request
				enrollmentRequest := runnerauth.EnrollmentRequest{Binding: runnerauth.NewBinding(), ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 900}
				if scenario == "viewer" {
					if raw := call(operatortool.CreateRunnerEnrollment, map[string]any{"request_id": "denied", "enrollment": enrollmentRequest}); len(raw) != 0 {
						t.Fatalf("viewer created enrollment: %s", raw)
					}
					return
				}
				response := request(user, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", enrollmentRequest)
				requireNativeStatus(t, response, http.StatusCreated)
				var enrollment runnerauth.Enrollment
				decodeHubResponse(t, response, &enrollment)
				runnerID := enrollmentRequest.RunnerID
				if scenario != "revoke enrollment" {
					redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: enrollmentRequest.Binding, Credential: func() string {
						token, err := apikey.GenerateToken()
						if err != nil {
							t.Fatal(err)
						}
						return token
					}(), Hostname: "fake-host", DisplayName: "Fixture runner", Capacity: 2, Version: "test"}
					var redeemed *httptest.ResponseRecorder
					if deployment == "shared" {
						raw, err := json.Marshal(redemption)
						if err != nil {
							t.Fatal(err)
						}
						redeemed = shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: http.MethodPost, target: "/organizations/org_security/api/v2/organizations/org_security/runner-enrollments/redeem", bearer: enrollment.Token, body: string(raw)})
					} else {
						redeemed = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments/redeem", enrollment.Token, redemption)
					}
					requireNativeStatus(t, redeemed, http.StatusCreated)
				}
				if scenario == "reads" {
					for _, name := range []string{operatortool.ListRunnerRouting, operatortool.HostedFleet} {
						raw := call(name, map[string]any{"limit": 1})
						if len(raw) == 0 || !strings.Contains(string(raw), runnerID) {
							t.Fatalf("read %s=%s", name, raw)
						}
					}
					if raw := call(operatortool.GetRunnerRouting, map[string]any{"runner_id": "foreign"}); len(raw) != 0 {
						t.Fatalf("foreign resource=%s", raw)
					}
					return
				}
				var info struct {
					ID string `json:"connection_id"`
				}
				if err := json.Unmarshal(call(operatortool.ConnectionInfo, map[string]any{}), &info); err != nil {
					t.Fatal(err)
				}
				approver := user
				if scenario == "different approver" {
					approver = f.user(t, "approver", "admin", "approver@example.test", "write", "")
					f.grant(t, approver, true, true)
				}
				var cachedPage *httptest.ResponseRecorder
				decision := func(action, kind, mode string) *httptest.ResponseRecorder {
					page := cachedPage
					if page == nil {
						page = request(approver, http.MethodGet, "/chat/approval?connection_id="+info.ID, nil)
					}
					requireNativeStatus(t, page, http.StatusOK)
					fragment := page.Body.String()
					if action != "" {
						for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(fragment, -1) {
							if strings.Contains(form, `name="action_id" value="`+action+`"`) {
								fragment = form
								break
							}
						}
					}
					token := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(fragment)
					if len(token) != 2 {
						t.Fatal("no form token")
					}
					return request(approver, http.MethodPost, "/chat/approval", url.Values{"connection_id": {info.ID}, "action_id": {action}, "decision": {kind}, "mode": {mode}, "form_token": {html.UnescapeString(token[1])}})
				}
				if scenario == "YOLO" {
					requireNativeStatus(t, decision("", "mode", "yolo"), http.StatusSeeOther)
				}
				name := operatortool.UpdateRunnerRouting
				args := map[string]any{"request_id": "fleet-effect", "runner_id": runnerID, "change": map[string]any{"expected_revision": 1, "display_name": "Renamed fixture", "state": "disabled", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project}}}
				switch scenario {
				case "ordinary":
					args["change"].(map[string]any)["state"] = "active"
				case "enrollment":
					name = operatortool.CreateRunnerEnrollment
					args = map[string]any{"request_id": "fleet-effect", "enrollment": runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read}, TTLSeconds: 900}}
				case "revoke enrollment":
					name = operatortool.RevokeRunnerEnrollment
					args = map[string]any{"request_id": "fleet-effect", "enrollment_id": enrollment.ID}
				case "revoke identity":
					name = operatortool.RevokeRunnerIdentity
					args = map[string]any{"request_id": "fleet-effect", "runner_id": runnerID}
				case "host":
					name = operatortool.UpdateRunnerHost
					args = map[string]any{"request_id": "fleet-effect", "machine_id": string(enrollmentRequest.MachineID), "change": map[string]any{"expected_revision": 1, "display_name": "Fixture host", "capacity": 3}}
				}
				var receipt struct {
					ID      string          `json:"action_id"`
					Status  string          `json:"status"`
					Receipt json.RawMessage `json:"receipt"`
				}
				raw := call(name, args)
				if err := json.Unmarshal(raw, &receipt); err != nil {
					t.Fatalf("receipt=%s error=%v", raw, err)
				}
				if scenario == "ordinary" || scenario == "YOLO" {
					if receipt.Status != "succeeded" {
						t.Fatalf("direct=%s", raw)
					}
				} else {
					if receipt.Status != "pending" {
						t.Fatalf("preview=%s", raw)
					}
					if scenario == "revoked grants" {
						cachedPage = request(approver, http.MethodGet, "/chat/approval?connection_id="+info.ID, nil)
						operatorSQL(t, f, "UPDATE hosted_project_grants SET manage_runner=0 WHERE user_id=?", user.identity.Subject)
					}
					if scenario == "stale" {
						operatorSQL(t, f, "UPDATE runner_identities SET revision=revision+1 WHERE id=?", runnerID)
					}
					reply := decision(receipt.ID, "confirm", "")
					if scenario == "revoked grants" {
						if reply.Code != http.StatusForbidden {
							t.Fatalf("revoked decision=%d", reply.Code)
						}
						return
					}
					if scenario == "stale" {
						if reply.Code != http.StatusConflict {
							t.Fatalf("stale decision=%d %s", reply.Code, reply.Body.String())
						}
						return
					}
					requireNativeStatus(t, reply, http.StatusSeeOther)
				}
				raw = call(operatortool.ActionResult, map[string]any{"action_id": receipt.ID})
				if !strings.Contains(string(raw), `"status":"succeeded"`) {
					t.Fatalf("result=%s", raw)
				}
				if scenario == "enrollment" {
					if strings.Contains(string(receipt.Receipt), "token") {
						t.Fatal("token escaped pending preview")
					}
					var result struct {
						Receipt runnerauth.Enrollment `json:"receipt"`
					}
					if json.Unmarshal(raw, &result) != nil || result.Receipt.Token == "" {
						t.Fatalf("missing enrollment receipt=%s", raw)
					}
				}
				again := call(name, args)
				if !strings.Contains(string(again), `"status":"succeeded"`) {
					t.Fatalf("retry=%s", again)
				}
				// Assert the application effect, not just the adapter receipt.
				var effects int
				switch name {
				case operatortool.CreateRunnerEnrollment:
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_enrollments").Scan(&effects); err != nil {
						t.Fatal(err)
					}
					if effects != 2 {
						t.Fatalf("enrollments=%d", effects)
					}
				case operatortool.RevokeRunnerEnrollment:
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_enrollments WHERE id=? AND revoked_at IS NOT NULL", enrollment.ID).Scan(&effects); err != nil {
						t.Fatal(err)
					}
					if effects != 1 {
						t.Fatal("enrollment was not revoked")
					}
				case operatortool.RevokeRunnerIdentity:
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens t JOIN runner_identities r ON r.token_id=t.id WHERE r.id=? AND t.revoked_at IS NOT NULL", runnerID).Scan(&effects); err != nil {
						t.Fatal(err)
					}
					if effects != 1 {
						t.Fatal("runner credential was not revoked")
					}
				case operatortool.UpdateRunnerRouting:
					var revision int
					var state string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT revision,state FROM runner_identities WHERE id=?", runnerID).Scan(&revision, &state); err != nil {
						t.Fatal(err)
					}
					expected := "disabled"
					if scenario == "ordinary" {
						expected = "active"
					}
					if revision != 2 || state != expected {
						t.Fatalf("runner revision/state=%d/%s", revision, state)
					}
				case operatortool.UpdateRunnerHost:
					var capacity, revision int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT capacity,routing_revision FROM machines WHERE id=?", enrollmentRequest.MachineID).Scan(&capacity, &revision); err != nil {
						t.Fatal(err)
					}
					if capacity != 3 || revision != 2 {
						t.Fatalf("host capacity/revision=%d/%d", capacity, revision)
					}
				}
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE operation=?", "mcp "+name).Scan(&effects); err != nil {
					t.Fatal(err)
				}
				if effects != 1 {
					t.Fatalf("effects=%d", effects)
				}
				var actor, original string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actor_id FROM native_commands WHERE operation=?", "mcp "+name).Scan(&actor); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", user.identity.Subject).Scan(&original); err != nil {
					t.Fatal(err)
				}
				if actor != original {
					t.Fatal("command attributed to approving browser instead of original connection")
				}

			})
		}
	}
}

// Direct calls must enforce instance administrator authority even without
// discovery. Worker protocol credentials never gain operator tools; an absent
// real approval browser cannot be replaced by an admin bearer token.
func TestHubMCPFleetBoundary(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{GitHubRequestCounts: func() []GitHubRequestCount { return []GitHubRequestCount{} }})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	r.enroll(t)
	var ctx context.Context
	// Non-hosted organization is bound by the application route parameter.
	f.service.echo.GET("/api/v2/organizations/:organization/fleet-context-test", func(c echo.Context) error { ctx = c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	for _, tt := range []struct {
		name, token string
		authorized  bool
	}{{"administrator", testHubAdminToken, true}, {"operator", f.token, false}, {"worker", r.redemption.Credential, false}} {
		t.Run(tt.name, func(t *testing.T) {
			ctx = nil
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet-context-test"
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, tt.token, nil)
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
			for _, name := range []string{operatortool.GetRunnerRouting, operatortool.ListRunnerRouting, operatortool.GitHubRequestCounts} {
				args := map[string]any{}
				if name == operatortool.GetRunnerRouting {
					args["runner_id"] = r.binding.RunnerID
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
				if (err == nil) != tt.authorized {
					t.Fatalf("%s result=%s error=%v", name, result.Content, err)
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
