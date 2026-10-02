package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestHostedAPIKeyCurrentAuthority(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			for _, scenario := range []string{"read", "write", "admin", "key revoked", "key expired", "key grant removed", "user grant removed", "provider membership removed", "local membership removed", "membership replaced", "provider role downgraded", "local role downgraded", "write grant removed", "foreign organization", "foreign project", "unselected project", "issuer principal revoked", "machine", "worker"} {
				t.Run(scenario, func(t *testing.T) {
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
					if scenario == "read" {
						keyScope = apikey.ScopeRead
					}
					scope := apiScopeOperator
					if scenario == "admin" {
						scope = apiScopeAdmin
						keyScope = apikey.ScopeAdmin
					}
					request := tokenRequest{Name: "external-client", Scope: scope, Issuer: &credential, KeyScope: keyScope, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}}
					if scenario == "machine" || scenario == "worker" {
						request.Issuer = nil
						request.ProjectIDs = nil
						if scenario == "worker" {
							request.Scope = apiScopeWorker
						}
					}
					key, err := f.service.createAPITokenFor(t.Context(), request)
					if err != nil {
						t.Fatal(err)
					}
					serve := func(method, path, body string) *httptest.ResponseRecorder {
						if deployment == "shared" {
							return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: method, target: "/organizations/org_security" + path, bearer: key.Token, body: body})
						}
						r := httptest.NewRequest(method, path, strings.NewReader(body))
						r.Header.Set("Authorization", "Bearer "+key.Token)
						r.Header.Set("Content-Type", "application/json")
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
					project := string(f.project)
					readDenied, writeDenied := false, scenario == "read"
					switch scenario {
					case "key revoked":
						operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
						readDenied = true
					case "key expired":
						operatorSQL(t, f, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(time.Now().Add(-time.Minute)), key.ID)
						readDenied = true
					case "key grant removed":
						operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=?", key.ID)
						readDenied = true
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
					case "foreign project":
						project = "prj_foreign"
						readDenied = true
					case "unselected project":
						project = "prj_other"
						operatorSQL(t, f, "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT ?,organization_id,'other',profile,states_json,created_at FROM projects WHERE id=?", project, f.project)
						operatorSQL(t, f, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write) VALUES (?,'org_security',?,1)", user.identity.Subject, project)
						operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,'org_security',?)", credential.ID, project)
						readDenied = true
					case "issuer principal revoked":
						operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), credential.ID)
						readDenied = true
					}
					for _, required := range []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite, apikey.ScopeAdmin} {
						_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: required, ProjectID: project})
						denied := readDenied || required != apikey.ScopeRead && (writeDenied || required == apikey.ScopeAdmin && keyScope != apikey.ScopeAdmin)
						if (err != nil) != denied {
							t.Fatalf("scope %s denied=%t err=%v", required, denied, err)
						}
					}
					read := serve(http.MethodGet, strings.Replace(f.base, string(f.project), project, 1), "")
					if (read.Code != http.StatusOK) != readDenied {
						t.Fatalf("API read status=%d want denied=%t", read.Code, readDenied)
					}
					write := serve(http.MethodPost, strings.Replace(f.base, string(f.project), project, 1)+"/work-items", `{"title":"key-write","state":"Todo","idempotency_key":"key-write"}`)
					if readDenied || writeDenied {
						if write.Code < 400 {
							t.Fatalf("API write unexpectedly succeeded: %d", write.Code)
						}
					} else {
						requireNativeStatus(t, write, http.StatusOK)
					}
					_, err = (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkList, Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)})
					if readDenied && !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("MCP read after authority change: %v", err)
					}
					if !readDenied && err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestHostedAPIKeyManagement(t *testing.T) {
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	viewer := f.member(t, "viewer", "viewer", "read")
	endpoint := "/organizations/org_security/api/v2/organizations/org_security/api-keys"
	request := func(user *hostedSecurityUser, method, path, body string, csrf bool, bearer string) *httptest.ResponseRecorder {
		r := hostedSharedRequest{user: user, method: method, target: path, body: body, bearer: bearer}
		if csrf && user != nil {
			r.csrf = cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")
		}
		if bearer != "" {
			r.kind = cloudassert.KindMachine
			r.user = nil
		}
		return f.serve(t, r)
	}
	for _, test := range []struct {
		name, scope, project string
		user                 *hostedSecurityUser
		csrf                 bool
		status               int
	}{
		{"no CSRF", "read", string(f.project), &owner, false, http.StatusForbidden},
		{"viewer elevation", "admin", string(f.project), &viewer, true, http.StatusUnprocessableEntity},
		{"viewer write", "write", string(f.project), &viewer, true, http.StatusUnprocessableEntity},
		{"foreign project", "read", "foreign", &owner, true, http.StatusNotFound},
		{"no project", "read", "", &owner, true, http.StatusNotFound},
		{"owner", "read", string(f.project), &owner, true, http.StatusCreated},
		{"viewer", "read", string(f.project), &viewer, true, http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"name":"` + test.name + `","scope":"` + test.scope + `","expires_days":30,"project_ids":["` + test.project + `"]}`
			response := request(test.user, http.MethodPost, endpoint, body, test.csrf, "")
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusCreated {
				return
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("key response must not be cached")
			}
			var key tokenResponse
			decodeHubResponse(t, response, &key)
			listed := request(test.user, http.MethodGet, endpoint, "", false, "")
			requireNativeStatus(t, listed, http.StatusOK)
			if strings.Contains(listed.Body.String(), key.Token) || strings.Contains(listed.Body.String(), apikey.HashToken(key.Token)) {
				t.Fatal("key listing exposed secret material")
			}
			var result struct {
				Keys []hostedAPIKey `json:"keys"`
			}
			decodeHubResponse(t, listed, &result)
			if len(result.Keys) != 1 || len(result.Keys[0].Projects) != 1 || result.Keys[0].Scope != apikey.ScopeRead {
				t.Fatal("key metadata or grants missing")
			}
			requireNativeStatus(t, request(nil, http.MethodGet, endpoint, "", false, key.Token), http.StatusForbidden)
			if test.user != &viewer {
				requireNativeStatus(t, request(&viewer, http.MethodDelete, endpoint+"/"+key.ID, "", true, ""), http.StatusNotFound)
			}
			requireNativeStatus(t, request(test.user, http.MethodDelete, endpoint+"/"+key.ID, "", true, ""), http.StatusNoContent)
			if _, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", ""); err == nil {
				t.Fatal("revoked key authenticated")
			}
		})
	}
}
