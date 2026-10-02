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
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
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
		for _, scenario := range []string{"reads", "enrollment", "revoke enrollment", "revoke identity", "revoke identity heartbeat", "revoke identity revoked runner", "routing", "routing heartbeat", "routing revoked runner", "host", "capacity", "capacity heartbeat", "capacity revoked runner", "capacity reapply", "capacity reapply heartbeat", "capacity revoked grants", "update", "update revoked grants", "update stale", "ordinary", "revoked grants", "revoked original grants", "revoked original session", "cross organization", "stale", "viewer", "YOLO", "different approver", "member runner grants"} {

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

				var updateEvidence *runnerauth.UpdateObservation
				if strings.HasPrefix(scenario, "update") {
					now := f.service.config.now()
					updateEvidence = &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "1.2.4", AvailableObservedAt: now, ObservedAt: now, ReceivedAt: now, Running: runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}}
					updateEvidence.Revision = updateEvidence.BuildRevision()
					encoded, err := json.Marshal(updateEvidence)
					if err != nil {
						t.Fatal(err)
					}
					operatorSQL(t, f, "UPDATE runner_identities SET update_observation_json=?, reported_protocol_major=2 WHERE id=?", string(encoded), runnerID)
				}
				if strings.HasPrefix(scenario, "capacity") {

					evidence := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.service.config.now()}
					encoded, err := json.Marshal(evidence)
					if err != nil {
						t.Fatal(err)
					}
					operatorSQL(t, f, "UPDATE runner_identities SET capacity_configuration_json=? WHERE id=?", string(encoded), runnerID)
					if strings.HasPrefix(scenario, "capacity reapply") {
						operatorSQL(t, f, "UPDATE runner_identities SET capacity_limit=6 WHERE id=?", runnerID)
					}
				}
				if scenario == "routing heartbeat" {
					operatorSQL(t, f, "UPDATE runner_identities SET state='disabled', capacity_limit=6 WHERE id=?", runnerID)
				}
				if strings.HasSuffix(scenario, "heartbeat") && scenario != "capacity reapply heartbeat" {
					operatorSQL(t, f, "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(f.service.config.now().Add(-runnerauth.HeartbeatTimeout)), runnerID)
				}
				if scenario == "reads" {
					if raw := call(operatortool.GetRunnerCapacity, map[string]any{"runner_id": runnerID}); len(raw) == 0 || !strings.Contains(string(raw), "runner_configuration_ceiling") {
						t.Fatalf("capacity read=%s", raw)
					}

					for _, name := range []string{operatortool.InstanceHealth, operatortool.OutboxHealth, operatortool.NativeCapabilities} {
						if raw := call(name, map[string]any{}); len(raw) != 0 {
							t.Fatalf("hosted legacy read %s=%s", name, raw)
						}
					}

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
				if scenario == "different approver" || scenario == "revoked original grants" || scenario == "revoked original session" || scenario == "cross organization" {
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
					csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(fragment)
					if len(csrf) != 2 {
						t.Fatal("no CSRF token")
					}
					return request(approver, http.MethodPost, "/chat/approval", url.Values{"csrf": {html.UnescapeString(csrf[1])}, "connection_id": {info.ID}, "action_id": {action}, "decision": {kind}, "mode": {mode}, "form_token": {html.UnescapeString(token[1])}})
				}
				if scenario == "YOLO" {
					requireNativeStatus(t, decision("", "mode", "yolo"), http.StatusSeeOther)
				}
				name := operatortool.UpdateRunnerRouting
				args := map[string]any{"request_id": "fleet-effect", "runner_id": runnerID, "change": map[string]any{"expected_revision": 1, "display_name": "Renamed fixture", "state": "disabled", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project}}}
				switch scenario {
				case "ordinary":
					args["change"].(map[string]any)["state"] = "active"
				case "routing heartbeat":
					args["change"].(map[string]any)["state"] = "active"
					args["change"].(map[string]any)["capacity_limit"] = 6
					args["change"].(map[string]any)["isolation_tier"] = "sandbox"
				case "capacity reapply", "capacity reapply heartbeat":
					args["change"].(map[string]any)["state"] = "active"
					args["change"].(map[string]any)["capacity_limit"] = 6
				case "enrollment":
					name = operatortool.CreateRunnerEnrollment
					args = map[string]any{"request_id": "fleet-effect", "enrollment": runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read}, TTLSeconds: 900}}
				case "revoke enrollment":
					name = operatortool.RevokeRunnerEnrollment
					args = map[string]any{"request_id": "fleet-effect", "enrollment_id": enrollment.ID}
				case "revoke identity", "revoke identity heartbeat", "revoke identity revoked runner":
					name = operatortool.RevokeRunnerIdentity
					args = map[string]any{"request_id": "fleet-effect", "runner_id": runnerID}

				case "update", "update revoked grants", "update stale":
					name = operatortool.UpdateApply
					args = map[string]any{"request_id": "fleet-effect", "runner_id": runnerID, "change": map[string]any{"expected_revision": 1, "expected_build_revision": updateEvidence.Revision, "service": "detent", "version": "1.2.4", "release": true}}
				case "capacity", "capacity heartbeat", "capacity revoked runner", "capacity revoked grants":

					name = operatortool.UpdateRunnerCapacity
					args = map[string]any{"request_id": "fleet-effect", "runner_id": runnerID, "change": map[string]any{"expected_revision": 1, "expected_config_revision": strings.Repeat("a", 64), "capacity": 6, "backend": "codex"}}
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
					if strings.HasSuffix(scenario, "heartbeat") {
						cachedPage = request(approver, http.MethodGet, "/chat/approval?connection_id="+info.ID, nil)
						before, err := readRunner(t.Context(), f.service.database.db, "org_security", runnerID, f.service.config.now())
						if err != nil {
							t.Fatal(err)
						}
						heartbeat := f.service.config.now()
						wantBefore, wantAfter := "offline", "online"
						if scenario == "capacity reapply heartbeat" {
							heartbeat = heartbeat.Add(-runnerauth.HeartbeatTimeout)
							wantBefore, wantAfter = "online", "offline"
						}
						operatorSQL(t, f, "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(heartbeat), runnerID)
						after, err := readRunner(t.Context(), f.service.database.db, "org_security", runnerID, f.service.config.now())
						if err != nil || before.ConnectionHealth != wantBefore || after.ConnectionHealth != wantAfter || before.Revision != after.Revision {
							t.Fatalf("heartbeat transition: before=%+v after=%+v error=%v", before, after, err)
						}
						if name == operatortool.UpdateRunnerRouting {
							change := args["change"].(map[string]any)
							change["capacity_limit"] = 7
							if raw := call(name, args); len(raw) != 0 {
								t.Fatalf("changed pending arguments=%s", raw)
							}
							change["capacity_limit"] = 6
						}
					}
					if strings.HasSuffix(scenario, "revoked runner") {
						operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=created_at WHERE id=(SELECT token_id FROM runner_identities WHERE id=?)", runnerID)
					}
					if scenario == "revoked original session" || scenario == "cross organization" {
						cachedPage = request(approver, http.MethodGet, "/chat/approval?connection_id="+info.ID, nil)
						sessionHash := apikey.HashToken(user.token)
						if deployment == "shared" {
							sessionHash = cloudassert.AuthorizationBinding("shared-"+user.identity.Subject, "org_security", user.identity.Hosted.SessionID)
						}
						if scenario == "revoked original session" {
							operatorSQL(t, f, "UPDATE hosted_sessions SET revoked_at=created_at WHERE token_hash=?", sessionHash)
						} else {
							operatorSQL(t, f, "UPDATE hosted_sessions SET identity_json=json_set(identity_json,'$.organization_id','org_other') WHERE token_hash=?", sessionHash)
						}
					}
					if scenario == "revoked grants" || scenario == "revoked original grants" || scenario == "capacity revoked grants" || scenario == "update revoked grants" {

						cachedPage = request(approver, http.MethodGet, "/chat/approval?connection_id="+info.ID, nil)
						operatorSQL(t, f, "UPDATE hosted_project_grants SET manage_runner=0 WHERE user_id=?", user.identity.Subject)
					}
					if scenario == "stale" || scenario == "update stale" {
						operatorSQL(t, f, "UPDATE runner_identities SET revision=revision+1 WHERE id=?", runnerID)
					}
					reply := decision(receipt.ID, "confirm", "")
					if scenario == "revoked grants" || scenario == "revoked original grants" || scenario == "capacity revoked grants" || scenario == "update revoked grants" {
						want := http.StatusForbidden
						if scenario == "revoked original grants" {
							want = http.StatusConflict
						}
						if reply.Code != want {
							t.Fatalf("revoked decision=%d", reply.Code)
						}
						if raw := call(name, args); len(raw) != 0 {
							t.Fatalf("revoked direct retry=%s", raw)
						}
						var state, display string
						var capacity int
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT state,display_name,capacity_limit FROM runner_identities WHERE id=?", runnerID).Scan(&state, &display, &capacity); err != nil {
							t.Fatal(err)
						}
						if state != "active" || display != "Fixture runner" || capacity != 2 {
							t.Fatalf("revoked approval changed runner: state=%s display=%s", state, display)
						}
						return
					}
					if scenario == "stale" || scenario == "update stale" || strings.HasSuffix(scenario, "revoked runner") || scenario == "revoked original session" || scenario == "cross organization" {

						if reply.Code != http.StatusConflict {
							t.Fatalf("stale decision=%d %s", reply.Code, reply.Body.String())
						}
						var revision, capacity int
						var state string
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT revision,state,capacity_limit FROM runner_identities WHERE id=?", runnerID).Scan(&revision, &state, &capacity); err != nil {
							t.Fatal(err)
						}
						wantRevision := 1
						if scenario == "stale" || scenario == "update stale" {
							wantRevision++
						}
						if revision != wantRevision || state != "active" || capacity != 2 {
							t.Fatalf("refused approval changed runner: revision=%d state=%s capacity=%d", revision, state, capacity)
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
				if strings.HasSuffix(scenario, "heartbeat") {
					f.service.operatorChat = chatpkg.NewService(nil, nil, hostedOperatorExecutor{f.service}, chatpkg.WithClock(f.service.config.now))
					if raw := call(name, args); !strings.Contains(string(raw), `"status":"succeeded"`) {
						t.Fatalf("durable retry=%s", raw)
					}
					if name == operatortool.UpdateRunnerRouting {
						args["change"].(map[string]any)["capacity_limit"] = 7
						if raw := call(name, args); len(raw) != 0 {
							t.Fatalf("changed durable retry arguments=%s", raw)
						}
						args["change"].(map[string]any)["capacity_limit"] = 6
					}
					operatorSQL(t, f, "UPDATE hosted_project_grants SET manage_runner=0 WHERE user_id=?", user.identity.Subject)
					if raw := call(name, args); len(raw) != 0 {
						t.Fatalf("revoked durable retry=%s", raw)
					}
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
					if scenario == "ordinary" || scenario == "routing heartbeat" || strings.HasPrefix(scenario, "capacity reapply") {
						expected = "active"
					}
					if revision != 2 || state != expected {
						t.Fatalf("runner revision/state=%d/%s", revision, state)
					}
					if scenario == "routing heartbeat" {
						var display, settings, projects string
						var capacity int
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT display_name,capacity_limit,routing_settings_json,(SELECT json_group_array(project_id) FROM token_grants WHERE token_id=r.token_id) FROM runner_identities r WHERE id=?", runnerID).Scan(&display, &capacity, &settings, &projects); err != nil {
							t.Fatal(err)
						}
						var saved runnerSettings
						if err := json.Unmarshal([]byte(settings), &saved); err != nil || display != "Renamed fixture" || capacity != 6 || saved.IsolationTier != "sandbox" || projects != `["`+string(f.project)+`"]` {
							t.Fatalf("routing mutation: display=%s capacity=%d settings=%s projects=%s error=%v", display, capacity, settings, projects, err)
						}
					}
					if scenario == "capacity reapply" {
						var settings string
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT routing_settings_json FROM runner_identities WHERE id=?", runnerID).Scan(&settings); err != nil {
							t.Fatal(err)
						}
						var saved runnerSettings
						if err := json.Unmarshal([]byte(settings), &saved); err != nil || saved.CapacityRequest == nil || saved.CapacityRequest.Capacity != 6 || saved.CapacityRequest.ExpectedConfigRevision != strings.Repeat("a", 64) {
							t.Fatalf("capacity request=%s error=%v", settings, err)
						}
					}
					if scenario == "capacity reapply heartbeat" {
						var settings string
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT routing_settings_json FROM runner_identities WHERE id=?", runnerID).Scan(&settings); err != nil {
							t.Fatal(err)
						}
						var saved runnerSettings
						if err := json.Unmarshal([]byte(settings), &saved); err != nil || saved.CapacityRequest != nil {
							t.Fatalf("stale evidence produced a capacity request=%s error=%v", settings, err)
						}
					}
				case operatortool.UpdateApply:
					var settings string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT routing_settings_json FROM runner_identities WHERE id=?", runnerID).Scan(&settings); err != nil {
						t.Fatal(err)
					}
					var saved runnerSettings
					if err := json.Unmarshal([]byte(settings), &saved); err != nil || saved.UpdateRequest == nil || saved.UpdateRequest.Version != "1.2.4" || saved.UpdateRequest.Service != "detent" {
						t.Fatalf("update delivery=%s error=%v", settings, err)
					}
					if !strings.Contains(string(raw), `"status":"requested"`) {
						t.Fatalf("queue receipt=%s", raw)
					}
				case operatortool.UpdateRunnerCapacity:
					var capacity runnerauth.CapacityView
					if err := json.Unmarshal(raw, &struct {
						Receipt *runnerauth.CapacityView `json:"receipt"`
					}{Receipt: &capacity}); err != nil || capacity.Desired != 6 || capacity.Effective != nil {
						t.Fatalf("capacity receipt=%s error=%v", raw, err)
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
