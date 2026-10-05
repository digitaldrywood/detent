package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedKeyMCPFixture struct {
	hostedSecurityFixture
	user    hostedSecurityUser
	browser func(string, string, url.Values) *httptest.ResponseRecorder
}

func newHostedKeyMCPFixture(t *testing.T, deployment, role string) (hostedKeyMCPFixture, context.Context) {
	t.Helper()
	var f hostedSecurityFixture
	var shared hostedSharedFixture
	var user hostedSecurityUser
	if deployment == "shared" {
		shared = newHostedSharedFixture(t)
		f = shared.hostedSecurityFixture
		user = shared.member(t, "operator", role, "write")
	} else {
		f = newHostedSecurityFixture(t)
		user = f.user(t, "operator", role, "operator@example.test", "write", "")
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/key-mcp-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "key-connection", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	var response *httptest.ResponseRecorder
	if deployment == "shared" {
		response = shared.serve(t, hostedSharedRequest{user: &user, method: http.MethodPost, target: "/organizations/org_security/key-mcp-test", csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
	} else {
		response = f.request(t, user, http.MethodPost, "/key-mcp-test", nil)
	}
	requireNativeStatus(t, response, http.StatusOK)
	ctx := <-contexts
	if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	browser := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		if deployment == "shared" {
			return shared.serve(t, hostedSharedRequest{user: &user, method: method, target: "/organizations/org_security" + path, body: form.Encode(), form: true, csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
		}
		return f.request(t, user, method, path, form)
	}
	return hostedKeyMCPFixture{f, user, browser}, ctx
}

func TestHostedAPIKeyCurrentAuthority(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			for _, access := range []hostedProjectAccess{hostedProjectsSelected, hostedProjectsAll} {
				t.Run(string(access), func(t *testing.T) {
					for _, scenario := range []string{"read", "write", "admin", "key revoked", "key expired", "key grant removed", "user grant removed", "provider membership removed", "local membership removed", "membership replaced", "provider role downgraded", "local role downgraded", "write grant removed", "foreign organization", "foreign organization route", "foreign project", "unselected project", "issuer principal revoked", "issuer grant removed", "future project", "future project read key", "future project without user grant", "future project without issuer grant", "no projects at creation", "machine", "worker", "never expires read", "never expires write", "never expires admin", "never expires key revoked", "never expires provider membership removed", "never expires local membership removed", "never expires provider role downgraded", "never expires local role downgraded", "never expires user grant removed", "never expires key grant removed"} {
						t.Run(scenario, func(t *testing.T) {
							neverExpires := strings.HasPrefix(scenario, "never expires ")
							scenario := strings.TrimPrefix(scenario, "never expires ")
							if access == hostedProjectsSelected && scenario == "no projects at creation" {
								t.Skip("only all-project keys can be created without projects")
							}
							var f hostedSecurityFixture
							var shared hostedSharedFixture
							if deployment == "shared" {
								shared = newHostedSharedFixture(t)
								f = shared.hostedSecurityFixture
							} else {
								f = newHostedSecurityFixture(t)
							}
							user := f.user(t, "operator", "owner", "operator@example.test", "write", "")
							credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
							if err != nil {
								t.Fatal(err)
							}
							expiry := time.Now().Add(time.Hour)
							keyScope := apikey.ScopeWrite
							if scenario == "read" || scenario == "future project read key" {
								keyScope = apikey.ScopeRead
							}
							scope := apiScopeOperator
							if scenario == "admin" {
								scope = apiScopeAdmin
								keyScope = apikey.ScopeAdmin
							}
							request := tokenRequest{Name: "external-client", Scope: scope, Issuer: &credential, KeyScope: keyScope, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}, ProjectAccess: access}
							if neverExpires {
								request.ExpiresAt = nil
							}
							if access == hostedProjectsAll {
								request.ProjectIDs = nil
							}
							if scenario == "no projects at creation" {
								operatorSQL(t, f, "DELETE FROM hosted_project_grants")
								operatorSQL(t, f, "DELETE FROM token_grants")
								operatorSQL(t, f, "DELETE FROM workflow_states")
								operatorSQL(t, f, "DELETE FROM projects")
							}
							if scenario == "machine" || scenario == "worker" {
								request.Issuer = nil
								request.ProjectIDs = nil
								request.ProjectAccess = ""
								if scenario == "worker" {
									request.Scope = apiScopeWorker
								}
							}
							key, err := f.service.createAPITokenFor(t.Context(), request)
							if err != nil {
								t.Fatal(err)
							}
							mcpHeaders := map[string]string{}
							serve := func(method, path, body string) *httptest.ResponseRecorder {
								if deployment == "shared" {
									return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: method, target: "/organizations/org_security" + path, bearer: key.Token, body: body, headers: mcpHeaders})
								}
								r := httptest.NewRequest(method, path, strings.NewReader(body))
								r.Header.Set("Authorization", "Bearer "+key.Token)
								r.Header.Set("Content-Type", "application/json")
								for name, value := range mcpHeaders {
									r.Header.Set(name, value)
								}
								response := httptest.NewRecorder()
								f.service.Handler().ServeHTTP(response, r)
								return response
							}
							captured := make(chan context.Context, 1)
							f.service.echo.POST("/api/v2/organizations/:organization/key-authority-test", func(c echo.Context) error { captured <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
							initial := serve(http.MethodPost, "/api/v2/organizations/org_security/key-authority-test", `{}`)
							if scenario == "machine" || scenario == "worker" {
								requireNativeStatus(t, initial, http.StatusForbidden)
								requireNativeStatus(t, serve(http.MethodGet, f.base, ""), http.StatusNotFound)
								return
							}
							requireNativeStatus(t, initial, http.StatusOK)
							ctx := <-captured
							executor := hostedOperatorExecutor{f.service}
							definitions, err := executor.ListTools(ctx)
							if err != nil {
								t.Fatal(err)
							}
							for _, d := range definitions {
								if d.Name == operatortool.CredentialList || d.Name == operatortool.CredentialCreate || d.Name == operatortool.CredentialRevoke {
									t.Fatal("bearer credential discovered key administration")
								}
							}
							if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.CredentialList, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("bearer key administration=%v", err)
							}
							mcpEndpoint := "/api/v2/organizations/org_security/mcp"
							initialized := serve(http.MethodPost, mcpEndpoint, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"key-regression","version":"1"}}}`)
							requireNativeStatus(t, initialized, http.StatusOK)
							mcpHeaders["Mcp-Session-Id"] = initialized.Header().Get("Mcp-Session-Id")
							if mcpHeaders["Mcp-Session-Id"] == "" {
								t.Fatal("MCP session missing")
							}
							mcpHeaders["MCP-Protocol-Version"] = "2025-11-25"
							requireNativeStatus(t, serve(http.MethodPost, mcpEndpoint, `{"jsonrpc":"2.0","method":"notifications/initialized"}`), http.StatusAccepted)

							project := string(f.project)
							readDenied, writeDenied := false, keyScope == apikey.ScopeRead
							switch scenario {
							case "key revoked":
								operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
								readDenied = true
							case "key expired":
								operatorSQL(t, f, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(time.Now().Add(-time.Minute)), key.ID)
								readDenied = true
							case "key grant removed":
								operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=?", key.ID)
								readDenied = access == hostedProjectsSelected
							case "user grant removed":
								operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", user.identity.Subject)
								readDenied = true
							case "provider membership removed":
								f.provider.mu.Lock()
								delete(f.provider.members, credential.HostedMembership)
								f.provider.mu.Unlock()
								readDenied = true
							case "local membership removed":
								operatorSQL(t, f, "UPDATE hosted_members SET active=0 WHERE user_id=?", user.identity.Subject)
								readDenied = true
							case "membership replaced":
								operatorSQL(t, f, "UPDATE hosted_members SET membership_id='replacement' WHERE user_id=?", user.identity.Subject)
								readDenied = true
							case "provider role downgraded":
								f.provider.mu.Lock()
								m := f.provider.members[credential.HostedMembership]
								m.Role.Slug = "viewer"
								f.provider.members[credential.HostedMembership] = m
								f.provider.mu.Unlock()
								writeDenied = true
							case "local role downgraded":
								operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", user.identity.Subject)
								writeDenied = true
							case "write grant removed":
								operatorSQL(t, f, "UPDATE hosted_project_grants SET can_write=0 WHERE user_id=?", user.identity.Subject)
								writeDenied = true
							case "foreign organization":
								operatorSQL(t, f, "UPDATE api_tokens SET hosted_organization_id=NULL WHERE id=?", key.ID)
								readDenied = true
							case "foreign organization route":
								readDenied = true
							case "foreign project":
								project = "prj_foreign"
								readDenied = true
							case "unselected project", "future project", "future project read key", "future project without user grant", "future project without issuer grant":
								project = "prj_other"
								operatorSQL(t, f, "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT ?,organization_id,'other',profile,states_json,created_at FROM projects WHERE id=?", project, f.project)
								operatorSQL(t, f, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT ?,source_name,detent_state,terminal,dispatchable,created_at,updated_at FROM workflow_states WHERE project_id=?", project, f.project)
								operatorSQL(t, f, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write) VALUES (?,'org_security',?,1)", user.identity.Subject, project)
								operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,'org_security',?)", credential.ID, project)
								readDenied = access == hostedProjectsSelected
								if scenario == "future project without user grant" {
									operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", user.identity.Subject, project)
									readDenied = true
								}
								if scenario == "future project without issuer grant" {
									operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=? AND project_id=?", credential.ID, project)
									readDenied = true
								}
							case "no projects at creation":
								if err := seedHostedSecurityProject(t.Context(), f.service.database.db, "org_security"); err != nil {
									t.Fatal(err)
								}
								f.grant(t, user, true, false)
							case "issuer grant removed":
								operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=?", credential.ID)
								readDenied = true
							case "issuer principal revoked":
								operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), credential.ID)
								readDenied = true
							}
							organization := "org_security"
							if scenario == "foreign organization route" {
								organization = "org_foreign"
							}
							for _, required := range []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite, apikey.ScopeAdmin} {
								_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: required, ProjectID: project, OrganizationID: organization})
								denied := readDenied || required != apikey.ScopeRead && (writeDenied || required == apikey.ScopeAdmin && keyScope != apikey.ScopeAdmin)
								if (err != nil) != denied {
									t.Fatalf("scope %s denied=%t err=%v", required, denied, err)
								}
							}
							if current, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", ""); err == nil && scenario != "foreign organization route" {
								projects, every, err := f.service.readableConversationProjects(t.Context(), nativeScope{organization: "org_security", credential: current})
								if err != nil || every || slices.Contains(projects, tracker.ProjectID(project)) == readDenied {
									t.Fatalf("project projection denied=%t projects=%v every=%t err=%v", readDenied, projects, every, err)
								}
							}
							apiBase := strings.Replace(strings.Replace(f.base, string(f.project), project, 1), "org_security", organization, 1)
							read := serve(http.MethodGet, apiBase, "")
							if (read.Code != http.StatusOK) != readDenied {
								t.Fatalf("API read status=%d want denied=%t", read.Code, readDenied)
							}
							write := serve(http.MethodPost, apiBase+"/work-items", `{"title":"key-write","state":"Todo","idempotency_key":"key-write"}`)
							if readDenied || writeDenied {
								if write.Code < 400 {
									t.Fatalf("API write unexpectedly succeeded: %d", write.Code)
								}
							} else {
								requireNativeStatus(t, write, http.StatusOK)
							}
							_, err = (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkList, Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)})
							if readDenied && scenario != "foreign organization route" && !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("MCP read after authority change: %v", err)
							}
							if !readDenied && err != nil {
								t.Fatal(err)
							}
							mcpRead := serve(http.MethodPost, strings.Replace(mcpEndpoint, "org_security", organization, 1), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"work_list","arguments":{"project_id":"`+project+`","limit":20}}}`)
							var rpc struct {
								Error  json.RawMessage `json:"error"`
								Result struct {
									IsError bool `json:"isError"`
								} `json:"result"`
							}
							if mcpRead.Code == http.StatusOK {
								decodeHubResponse(t, mcpRead, &rpc)
							}
							mcpDenied := mcpRead.Code >= 400 || len(rpc.Error) > 0 && string(rpc.Error) != "null" || rpc.Result.IsError
							if mcpDenied != readDenied {
								t.Fatalf("MCP session read status=%d denied=%t want=%t body=%s", mcpRead.Code, mcpDenied, readDenied, mcpRead.Body.String())
							}

						})
					}
				})
			}
		})
	}
}

func TestHostedAPIKeyManagement(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			if deployment == "shared" {
				shared = newHostedSharedFixture(t)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t)
			}
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
			endpoint := "/api/v2/organizations/org_security/api-keys"
			request := func(user *hostedSecurityUser, method, path, body string, csrf bool, bearer string) *httptest.ResponseRecorder {
				if deployment == "shared" {
					r := hostedSharedRequest{user: user, method: method, target: "/organizations/org_security" + path, body: body, bearer: bearer}
					if csrf && user != nil {
						r.csrf = cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")
					}
					if bearer != "" {
						r.kind = cloudassert.KindMachine
						r.user = nil
					}
					return shared.serve(t, r)
				}
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if user != nil {
					r.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
					if csrf {
						r.Header.Set("X-CSRF-Token", hostedCSRF(user.token))
					}
				}
				if bearer != "" {
					r.Header.Set("Authorization", "Bearer "+bearer)
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, r)
				return response
			}
			for _, test := range []struct {
				name, scope, selection string
				user                   *hostedSecurityUser
				csrf                   bool
				status                 int
				access                 hostedProjectAccess
				days                   int
				neverExpires           bool
			}{
				{"no CSRF", "read", `,"project_ids":["` + string(f.project) + `"]`, &owner, false, http.StatusForbidden, "", 30, false},
				{"viewer elevation", "admin", "", &viewer, true, http.StatusUnprocessableEntity, "", 30, false},
				{"viewer write", "write", "", &viewer, true, http.StatusUnprocessableEntity, "", 30, false},
				{"foreign project", "read", `,"project_ids":["foreign"]`, &owner, true, http.StatusNotFound, "", 30, false},
				{"invalid project", "read", `,"project_ids":[""]`, &owner, true, http.StatusNotFound, "", 30, false},
				{"selected without projects", "read", `,"project_access":"selected"`, &owner, true, http.StatusUnprocessableEntity, "", 30, false},
				{"unknown access", "read", `,"project_access":"unknown"`, &owner, true, http.StatusUnprocessableEntity, "", 30, false},
				{"ambiguous all", "read", `,"project_access":"all","project_ids":["` + string(f.project) + `"]`, &owner, true, http.StatusUnprocessableEntity, "", 30, false},
				{"owner legacy", "read", `,"project_ids":["` + string(f.project) + `"]`, &owner, true, http.StatusCreated, hostedProjectsSelected, 30, false},
				{"viewer selected", "read", `,"project_access":"selected","project_ids":["` + string(f.project) + `"]`, &viewer, true, http.StatusCreated, hostedProjectsSelected, 30, false},
				{"default all", "read", "", &owner, true, http.StatusCreated, hostedProjectsAll, 30, false},
				{"empty default all", "read", `,"project_ids":[]`, &owner, true, http.StatusCreated, hostedProjectsAll, 30, false},
				{"viewer all", "read", `,"project_access":"all"`, &viewer, true, http.StatusCreated, hostedProjectsAll, 30, false},
				{"never expires", "read", "", &owner, true, http.StatusCreated, hostedProjectsAll, 0, true},
				{"viewer never expires", "read", `,"project_access":"selected","project_ids":["` + string(f.project) + `"]`, &viewer, true, http.StatusCreated, hostedProjectsSelected, 0, true},
				{"zero expiry", "read", "", &owner, true, http.StatusUnprocessableEntity, "", 0, false},
				{"conflicting expiry", "read", "", &owner, true, http.StatusUnprocessableEntity, "", 30, true},
				{"negative expiry", "read", "", &owner, true, http.StatusUnprocessableEntity, "", -1, false},
				{"excessive expiry", "read", "", &owner, true, http.StatusUnprocessableEntity, "", 91, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := fmt.Sprintf(`{"name":%q,"scope":%q,"expires_days":%d,"never_expires":%t%s}`, test.name, test.scope, test.days, test.neverExpires, test.selection)
					response := request(test.user, http.MethodPost, endpoint, body, test.csrf, "")
					requireNativeStatus(t, response, test.status)
					if test.status != http.StatusCreated {
						if test.name == "selected without projects" && !strings.Contains(response.Body.String(), "at least one project") {
							t.Fatal("missing clear selected-project error")
						}
						return
					}
					if response.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("key response must not be cached")
					}
					var key tokenResponse
					decodeHubResponse(t, response, &key)
					if (key.ExpiresAt == nil) != test.neverExpires {
						t.Fatal("unexpected key expiry")
					}
					var expiry sql.NullString
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT expires_at FROM api_tokens WHERE id=?", key.ID).Scan(&expiry); err != nil {
						t.Fatal(err)
					}
					if expiry.Valid == test.neverExpires {
						t.Fatal("unexpected stored key expiry")
					}
					wantProjects := 0
					if test.access == hostedProjectsSelected {
						wantProjects = 1
					}
					if key.ProjectAccess != test.access || len(key.Projects) != wantProjects {
						t.Fatal("create metadata missing")
					}
					listed := request(test.user, http.MethodGet, endpoint, "", false, "")
					requireNativeStatus(t, listed, http.StatusOK)
					if strings.Contains(listed.Body.String(), key.Token) || strings.Contains(listed.Body.String(), apikey.HashToken(key.Token)) {
						t.Fatal("key listing exposed secret material")
					}
					var result struct {
						Keys []hostedAPIKey `json:"keys"`
					}
					decodeHubResponse(t, listed, &result)
					found := false
					for _, metadata := range result.Keys {
						if metadata.ID == key.ID {
							found = true
							if (metadata.Expiry == nil) != test.neverExpires {
								t.Fatal("unexpected listed key expiry")
							}
							if len(metadata.Projects) != wantProjects || metadata.Scope != apikey.ScopeRead || metadata.ProjectAccess != test.access {
								t.Fatal("key metadata or grants missing")
							}
							createdAt, err := time.Parse(time.RFC3339Nano, metadata.CreatedAt)
							if err != nil || !createdAt.Equal(key.CreatedAt) || metadata.RevokedAt != nil {
								t.Fatal("active key timestamps missing or revoked")
							}
						}
					}
					if !found {
						t.Fatal("key not listed")
					}
					requireNativeStatus(t, request(nil, http.MethodGet, "/api/v2/organizations/org_security/projects/"+string(f.project), "", false, key.Token), http.StatusOK)
					requireNativeStatus(t, request(nil, http.MethodPost, "/api/v2/organizations/org_security/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"created-key","version":"1"}}}`, false, key.Token), http.StatusOK)
					requireNativeStatus(t, request(nil, http.MethodGet, endpoint, "", false, key.Token), http.StatusForbidden)
					if test.user != &viewer {
						requireNativeStatus(t, request(&viewer, http.MethodDelete, endpoint+"/"+key.ID, "", true, ""), http.StatusNotFound)
					}
					requireNativeStatus(t, request(test.user, http.MethodDelete, endpoint+"/"+key.ID, "", true, ""), http.StatusNoContent)
					listed = request(test.user, http.MethodGet, endpoint, "", false, "")
					requireNativeStatus(t, listed, http.StatusOK)
					decodeHubResponse(t, listed, &result)
					found = false
					for _, metadata := range result.Keys {
						if metadata.ID != key.ID {
							continue
						}
						found = true
						if !metadata.Revoked || metadata.RevokedAt == nil {
							t.Fatal("revoked key timestamp missing")
						}
						if _, err := time.Parse(time.RFC3339Nano, *metadata.RevokedAt); err != nil {
							t.Fatal("invalid revoked key timestamp")
						}
					}
					if !found {
						t.Fatal("revoked key missing from history")
					}
					if _, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", ""); err == nil {
						t.Fatal("revoked key authenticated")
					}
				})
			}
		})
	}
}
