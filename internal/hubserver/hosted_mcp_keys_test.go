package hubserver

import (
	"context"
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
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedKeyMCPFixture struct {
	hostedSecurityFixture
	user    hostedSecurityUser
	browser func(string, string, url.Values) *httptest.ResponseRecorder
	issuer  func(*testing.T) hubapi.APICredential
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
	issuer := func(t *testing.T) hubapi.APICredential {
		t.Helper()
		if deployment == "shared" {
			return shared.sessionCredential(t, user)
		}
		credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
		if err != nil {
			t.Fatal(err)
		}
		return credential
	}
	return hostedKeyMCPFixture{f, user, browser, issuer}, ctx
}

func TestHostedAPIKeyCurrentAuthority(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			t.Parallel()
			for _, access := range []hubapi.HostedProjectAccess{hubapi.HostedProjectsSelected, hubapi.HostedProjectsAll} {
				t.Run(string(access), func(t *testing.T) {
					t.Parallel()
					for _, scenario := range []string{"read", "write", "admin", "key revoked", "key expired", "key grant removed", "user grant removed", "provider membership removed", "local membership removed", "membership replaced", "provider role downgraded", "local role downgraded", "write grant removed", "foreign organization", "foreign organization route", "foreign project", "unselected project", "issuer principal revoked", "issuer grant removed", "future project", "future project read key", "future project without user grant", "future project without issuer grant", "no projects at creation", "machine", "worker", "never expires read", "never expires write", "never expires admin", "never expires key revoked", "never expires provider membership removed", "never expires local membership removed", "never expires provider role downgraded", "never expires local role downgraded", "never expires user grant removed", "never expires key grant removed"} {
						t.Run(scenario, func(t *testing.T) {
							t.Parallel()
							neverExpires := strings.HasPrefix(scenario, "never expires ")
							scenario := strings.TrimPrefix(scenario, "never expires ")
							var f hostedSecurityFixture
							var shared hostedSharedFixture
							if deployment == "shared" {
								shared = newHostedSharedFixture(t)
								f = shared.hostedSecurityFixture
							} else {
								f = newHostedSecurityFixture(t)
							}
							writeAccess := func(apply func(tenantstore.Writer) error) {
								t.Helper()
								if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, apply); err != nil {
									t.Fatal(err)
								}
							}
							createProject := func(id tracker.ProjectID, principal string) {
								t.Helper()
								writeAccess(func(w tenantstore.Writer) error {
									template, err := w.ProjectRecord(t.Context(), f.project)
									if err != nil {
										return err
									}
									if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.service.accessOrganization(), ID: id, Name: "Other", Profile: template.Core.Profile, CreatedAt: template.Core.CreatedAt}, States: template.States}); err != nil {
										return err
									}
									return w.InitializeProjectAccess(t.Context(), id, principal)
								})
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
							scope := hubapi.APIScopeOperator
							if scenario == "admin" {
								scope = hubapi.APIScopeAdmin
								keyScope = apikey.ScopeAdmin
							}
							request := hubapi.TokenRequest{Name: "external-client", Scope: scope, Issuer: &credential, KeyScope: keyScope, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}, ProjectAccess: access}
							if neverExpires {
								request.ExpiresAt = nil
							}
							if access == hubapi.HostedProjectsAll {
								request.ProjectIDs = nil
							}
							if scenario == "no projects at creation" {
								writeAccess(func(w tenantstore.Writer) error {
									row, err := w.ProjectRecord(t.Context(), f.project)
									if err != nil {
										return err
									}
									if err := w.(accessstore.ProjectAccessCleaner).DeleteAccessProjectGrants(t.Context(), f.project); err != nil {
										return err
									}
									return w.RetireProject(t.Context(), f.project, row.Settings.Revision, f.service.config.now())
								})
							}
							if scenario == "machine" || scenario == "worker" {
								request.Issuer = nil
								request.ProjectIDs = nil
								request.ProjectAccess = ""
								if scenario == "worker" {
									request.Scope = hubapi.APIScopeWorker
								}
							}
							key, err := f.service.createAPITokenFor(t.Context(), request)
							if access == hubapi.HostedProjectsSelected && scenario == "no projects at creation" {
								if !hubapi.IsNativeNotFound(err) {
									t.Fatalf("selected key without an active project=%v", err)
								}
								return
							}
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
								if scenario == "worker" {
									current, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", "")
									if err != nil {
										t.Fatal(err)
									}
									if actor := (hubscope.NativeScope{Credential: current}).Actor(); actor.Kind != "runner" || actor.PrincipalID != key.ID {
										t.Fatalf("hosted worker actor=%+v", actor)
									}
								}
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
							organization := "org_security"
							readDenied, writeDenied := false, keyScope == apikey.ScopeRead
							switch scenario {
							case "key revoked":
								writeAccess(func(w tenantstore.Writer) error {
									return w.RevokeAccessCredential(t.Context(), key.ID, f.service.config.now())
								})
								readDenied = true
							case "key expired":
								writeAccess(func(w tenantstore.Writer) error {
									row, err := w.AccessCredential(t.Context(), key.ID)
									if err != nil {
										return err
									}
									return w.RenewAccessCredential(t.Context(), row.ID, row.Hash, row.Fingerprint, f.service.config.now().Add(-time.Minute), f.service.config.now())
								})
								readDenied = true
							case "key grant removed":
								writeAccess(func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), key.ID, f.project) })
								readDenied = access == hubapi.HostedProjectsSelected
							case "user grant removed":
								writeAccess(func(w tenantstore.Writer) error {
									return w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, f.project)
								})
								readDenied = true
							case "provider membership removed":
								f.provider.mu.Lock()
								delete(f.provider.members, credential.HostedMembership)
								f.provider.mu.Unlock()
								readDenied = true
							case "local membership removed":
								writeAccess(func(w tenantstore.Writer) error {
									return w.RevokeAccessMember(t.Context(), user.identity.Subject, f.service.config.now())
								})
								readDenied = true
							case "membership replaced":
								writeAccess(func(w tenantstore.Writer) error {
									member, err := w.AccessMember(t.Context(), user.identity.Subject)
									if err != nil {
										return err
									}
									member.MembershipID = "replacement"
									member.UpdatedAt = f.service.config.now()
									return w.UpdateAccessMember(t.Context(), member)
								})
								readDenied = true
							case "provider role downgraded":
								f.provider.mu.Lock()
								m := f.provider.members[credential.HostedMembership]
								m.Role.Slug = "viewer"
								f.provider.members[credential.HostedMembership] = m
								f.provider.mu.Unlock()
								writeDenied = true
							case "local role downgraded":
								writeAccess(func(w tenantstore.Writer) error {
									member, err := w.AccessMember(t.Context(), user.identity.Subject)
									if err != nil {
										return err
									}
									member.Role = "viewer"
									member.UpdatedAt = f.service.config.now()
									return w.UpdateAccessMember(t.Context(), member)
								})
								writeDenied = true
							case "write grant removed":
								writeAccess(func(w tenantstore.Writer) error {
									grant, err := w.AccessProjectGrant(t.Context(), user.identity.Subject, f.project)
									if err != nil {
										return err
									}
									grant.CanWrite = false
									return w.SetAccessProjectGrant(t.Context(), grant)
								})
								writeDenied = true
							case "foreign organization", "foreign organization route":
								foreign, err := f.service.createNativeOrganizationFor(t.Context(), "Foreign")
								if err != nil {
									t.Fatal(err)
								}
								organization = string(foreign.ID)
								if err := f.service.config.TenantRepository.WriteTenant(t.Context(), foreign.ID, corestore.Serving, func(w tenantstore.Writer) error {
									_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: foreign.ID, ID: f.project, Name: "Foreign", Profile: "native"}, States: nativeFixtureStates()})
									return err
								}); err != nil {
									t.Fatal(err)
								}
								if err := f.service.config.TenantRepository.ReadTenant(t.Context(), foreign.ID, corestore.Serving, func(r tenantstore.Reader) error {
									_, err := r.AuthorizeAccess(t.Context(), accessstore.Authority{CredentialID: key.ID, Hash: apikey.HashToken(key.Token), ProjectID: f.project}, f.service.config.now())
									return err
								}); !errors.Is(err, accessstore.ErrDenied) {
									t.Fatalf("foreign tenant credential authority=%v", err)
								}
								readDenied = true
							case "foreign project":
								project = "prj_foreign"
								readDenied = true
							case "unselected project", "future project", "future project read key", "future project without user grant", "future project without issuer grant":
								project = "prj_other"
								createProject(tracker.ProjectID(project), credential.ID)
								seedNativeFixturePolicy(t, f.service, "org_security/"+project)
								readDenied = access == hubapi.HostedProjectsSelected
								if scenario == "future project without user grant" {
									writeAccess(func(w tenantstore.Writer) error {
										return w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, tracker.ProjectID(project))
									})
									readDenied = true
								}
								if scenario == "future project without issuer grant" {
									writeAccess(func(w tenantstore.Writer) error {
										return w.DeleteAccessTokenGrant(t.Context(), credential.ID, tracker.ProjectID(project))
									})
									readDenied = true
								}
							case "no projects at creation":
								project = "prj_after_empty"
								writeAccess(func(w tenantstore.Writer) error {
									if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.service.accessOrganization(), ID: tracker.ProjectID(project), Name: "After empty", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}}); err != nil {
										return err
									}
									return w.InitializeProjectAccess(t.Context(), tracker.ProjectID(project), credential.ID)
								})
								seedNativeFixturePolicy(t, f.service, "org_security/"+project)
								f.base = strings.Replace(f.base, string(f.project), project, 1)
								f.project = tracker.ProjectID(project)
								f.grant(t, user, true, false)
							case "issuer grant removed":
								writeAccess(func(w tenantstore.Writer) error {
									return w.DeleteAccessTokenGrant(t.Context(), credential.ID, f.project)
								})
								readDenied = true
							case "issuer principal revoked":
								writeAccess(func(w tenantstore.Writer) error {
									return w.RevokeAccessCredential(t.Context(), credential.ID, f.service.config.now())
								})
								readDenied = true
							}
							for _, required := range []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite, apikey.ScopeAdmin} {
								_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: required, ProjectID: project, OrganizationID: organization})
								denied := readDenied || required != apikey.ScopeRead && (writeDenied || required == apikey.ScopeAdmin && keyScope != apikey.ScopeAdmin)
								if (err != nil) != denied {
									t.Fatalf("scope %s denied=%t err=%v", required, denied, err)
								}
							}
							ownReadDenied := readDenied && organization == "org_security"
							current, _, currentErr := f.service.authenticateAPIToken(t.Context(), key.Token, "", "")
							if organization != "org_security" && currentErr != nil {
								t.Fatalf("own tenant credential changed: %v", currentErr)
							}
							if currentErr == nil {
								projects, every, err := f.service.readableConversationProjects(t.Context(), hubscope.NativeScope{Organization: "org_security", Credential: current})
								if err != nil || every || slices.Contains(projects, tracker.ProjectID(project)) == ownReadDenied {
									t.Fatalf("project projection denied=%t projects=%v every=%t err=%v", ownReadDenied, projects, every, err)
								}
							}
							apiBase := strings.Replace(strings.Replace(f.base, string(f.project), project, 1), "org_security", organization, 1)
							read := serve(http.MethodGet, apiBase, "")
							if (read.Code != http.StatusOK) != readDenied {
								t.Fatalf("API read status=%d want denied=%t", read.Code, readDenied)
							}
							write := serve(http.MethodPost, apiBase+"/work-items", "{\"body\":\"\\n## Acceptance criteria\\nComplete the requested work.\\n## Must not break\\nExisting behavior.\\n## How we know it worked\\nRun the project checks.\",\"idempotency_key\":\"key-write\",\"state\":\"Todo\",\"title\":\"key-write\"}")
							if readDenied || writeDenied {
								if write.Code < 400 {
									t.Fatalf("API write unexpectedly succeeded: %d", write.Code)
								}
							} else {
								requireNativeStatus(t, write, http.StatusOK)
							}
							for _, readTool := range []string{operatortool.WorkList, operatortool.WorkBulkExport} {
								_, err = (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: readTool, Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)})
								if ownReadDenied && !errors.Is(err, operatortool.ErrAccessDenied) {
									t.Fatalf("MCP read after authority change: %v", err)
								}
								if !ownReadDenied && err != nil {
									t.Fatal(err)
								}
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
							if scenario == "admin" {
								var expected tracker.ChangeReviewPolicy
								if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
									revision, err := r.ProjectPolicy(t.Context(), f.project)
									if err != nil {
										return err
									}
									descriptor, err := eventstore.NewContent(f.service.runDetailStore()).ProjectPolicy(t.Context(), revision)
									expected = defaultChangeReviewPolicy(descriptor)
									return err
								}); err != nil {
									t.Fatal(err)
								}
								response := serve(http.MethodPost, mcpEndpoint, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+operatortool.GetChangeReviewPolicy+`","arguments":{"project_id":"`+project+`"}}}`)
								requireNativeStatus(t, response, http.StatusOK)
								var reviewPolicy struct {
									Result struct {
										IsError bool `json:"isError"`
										Content struct {
											OrganizationID string `json:"organization_id"`
											ProjectID      string `json:"project_id"`
											Policy         struct {
												ReviewPolicyID string                    `json:"review_policy_id"`
												PolicyID       string                    `json:"policy_id"`
												RequireReview  bool                      `json:"require_review"`
												RequiredChecks []tracker.ChangeCheckSpec `json:"required_checks"`
											} `json:"policy"`
										} `json:"structuredContent"`
									} `json:"result"`
								}
								decodeHubResponse(t, response, &reviewPolicy)
								got := reviewPolicy.Result.Content
								if reviewPolicy.Result.IsError || got.OrganizationID != organization || got.ProjectID != project || got.Policy.ReviewPolicyID != expected.ID || got.Policy.PolicyID != expected.PolicyID || got.Policy.RequireReview != expected.RequireReview || got.Policy.RequiredChecks == nil || len(got.Policy.RequiredChecks) != 0 {
									t.Fatalf("admin key %s=%s want default policy %+v", operatortool.GetChangeReviewPolicy, response.Body.String(), expected)
								}
								for _, missing := range []struct{ name, message string }{
									{"get_cutover_receipt", "No cutover receipt exists for this project"},
								} {
									response := serve(http.MethodPost, mcpEndpoint, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+missing.name+`","arguments":{"project_id":"`+project+`"}}}`)
									requireNativeStatus(t, response, http.StatusOK)
									var result struct {
										Result struct {
											IsError bool                      `json:"isError"`
											Refusal operatortool.RequestError `json:"structuredContent"`
										} `json:"result"`
									}
									decodeHubResponse(t, response, &result)
									if !result.Result.IsError || result.Result.Refusal.Code != "not_found" || result.Result.Refusal.Message != missing.message {
										t.Fatalf("admin key %s=%s", missing.name, response.Body.String())
									}
								}
							}

						})
					}
				})
			}
		})
	}
}

func TestHostedAPIKeyManagement(t *testing.T) {
	t.Parallel()
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
				access                 hubapi.HostedProjectAccess
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
				{"owner legacy", "read", `,"project_ids":["` + string(f.project) + `"]`, &owner, true, http.StatusCreated, hubapi.HostedProjectsSelected, 30, false},
				{"viewer selected", "read", `,"project_access":"selected","project_ids":["` + string(f.project) + `"]`, &viewer, true, http.StatusCreated, hubapi.HostedProjectsSelected, 30, false},
				{"default all", "read", "", &owner, true, http.StatusCreated, hubapi.HostedProjectsAll, 30, false},
				{"empty default all", "read", `,"project_ids":[]`, &owner, true, http.StatusCreated, hubapi.HostedProjectsAll, 30, false},
				{"viewer all", "read", `,"project_access":"all"`, &viewer, true, http.StatusCreated, hubapi.HostedProjectsAll, 30, false},
				{"never expires", "read", "", &owner, true, http.StatusCreated, hubapi.HostedProjectsAll, 0, true},
				{"viewer never expires", "read", `,"project_access":"selected","project_ids":["` + string(f.project) + `"]`, &viewer, true, http.StatusCreated, hubapi.HostedProjectsSelected, 0, true},
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
					var key hubapi.TokenResponse
					decodeHubResponse(t, response, &key)
					if (key.ExpiresAt == nil) != test.neverExpires {
						t.Fatal("unexpected key expiry")
					}
					var stored accessstore.Credential
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						stored, err = r.AccessCredential(t.Context(), key.ID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if (stored.ExpiresAt != nil) == test.neverExpires {
						t.Fatal("unexpected stored key expiry")
					}
					wantProjects := 0
					if test.access == hubapi.HostedProjectsSelected {
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

func TestHostedProjectRankTools(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"shared", "dedicated"} {
		for _, role := range []string{"owner", "admin", "member", "viewer"} {
			t.Run(deployment+"/"+role, func(t *testing.T) {
				f, ctx := newHostedKeyMCPFixture(t, deployment, role)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					template, err := w.ProjectRecord(t.Context(), f.project)
					if err != nil {
						return err
					}
					_, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.service.accessOrganization(), ID: "prj_rank_second", Name: "Second", Profile: template.Core.Profile, CreatedAt: template.Core.CreatedAt}, States: template.States})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				readStoredRank := func() (organizationProjectRank, error) {
					var value organizationProjectRank
					err := f.service.config.TenantRepository.ReadTenant(ctx, f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
						owner := r.(projectstore.OrganizationReader)
						var err error
						value.Revision, err = owner.OrganizationSchedulingRevision(ctx)
						if err != nil {
							return err
						}
						projects, err := owner.OrganizationRankProjects(ctx, 201)
						if err != nil {
							return err
						}
						if len(projects) > 200 {
							return corestore.ErrInvalidPage
						}
						for _, project := range projects {
							value.ProjectIDs = append(value.ProjectIDs, project.ID)
						}
						return nil
					})
					return value, err
				}
				executor := hostedOperatorExecutor{f.service}
				read, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.OrganizationProjectRank, Arguments: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Data organizationProjectRank `json:"data"`
				}
				if err := json.Unmarshal(read.Content, &envelope); err != nil {
					t.Fatal(err)
				}
				visible := envelope.Data
				if !slices.Equal(visible.ProjectIDs, []tracker.ProjectID{f.project}) {
					t.Fatalf("rank leaked hidden projects: %#v", visible)
				}
				before, err := readStoredRank()
				if err != nil {
					t.Fatal(err)
				}
				ids := slices.Clone(visible.ProjectIDs)
				args, err := json.Marshal(struct {
					projectRankChange
					RequestID string `json:"request_id"`
				}{projectRankChange{ExpectedRevision: before.Revision, ProjectIDs: ids}, "rank-update"})
				if err != nil {
					t.Fatal(err)
				}
				call := operatortool.Call{Name: operatortool.OrganizationProjectRankUpdate, Arguments: args}
				_, err = executor.Execute(ctx, call)
				if role == "viewer" || role == "member" {
					if !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("viewer update: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if _, err = executor.Execute(ctx, call); err != nil {
						t.Fatalf("replay: %v", err)
					}
					stale := strings.Replace(string(args), "rank-update", "rank-stale", 1)
					if _, err = executor.Execute(ctx, operatortool.Call{Name: call.Name, Arguments: json.RawMessage(stale)}); err == nil {
						t.Fatal("stale revision accepted")
					}
				}
				after, err := readStoredRank()
				if err != nil {
					t.Fatal(err)
				}
				expected, revision := before.ProjectIDs, before.Revision
				if role == "owner" || role == "admin" {
					expected, revision = before.ProjectIDs, revision+1
				}
				if !slices.Equal(after.ProjectIDs, expected) || after.Revision != revision {
					t.Fatalf("rank changed incorrectly: %#v", after)
				}
			})
		}
	}
}
