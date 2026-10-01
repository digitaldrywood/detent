package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// Both entry paths must resolve current membership/grants when executing an
// action after discovery, and fail before the application action is reached.
func TestHostedOperatorCurrentAuthority(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			for _, test := range []struct {
				name, role, grant, project, kind string
				scope                            apikey.Scope
				change                           func(*testing.T, hostedSecurityFixture, hostedSecurityUser)
				denied                           bool
			}{
				{name: "viewer read", role: "viewer", grant: "read", scope: apikey.ScopeRead},
				{name: "viewer write", role: "viewer", grant: "write", scope: apikey.ScopeWrite, denied: true},
				{name: "member write", role: "member", grant: "write", scope: apikey.ScopeWrite},
				{name: "member admin", role: "member", grant: "write", scope: apikey.ScopeAdmin, denied: true},
				{name: "owner admin", role: "owner", grant: "write", scope: apikey.ScopeAdmin},
				{name: "owner billing read", role: "owner", kind: "billing", scope: apikey.ScopeRead},
				{name: "admin billing denied", role: "admin", kind: "billing", scope: apikey.ScopeRead, denied: true},
				{name: "viewer billing denied", role: "viewer", kind: "billing", scope: apikey.ScopeRead, denied: true},
				{name: "admin plan read", role: "admin", kind: "plan", scope: apikey.ScopeRead},
				{name: "member plan denied", role: "member", kind: "plan", scope: apikey.ScopeRead, denied: true},
				{name: "missing grant", role: "admin", scope: apikey.ScopeRead, denied: true},
				{name: "foreign project", role: "owner", grant: "write", project: "other", scope: apikey.ScopeRead, denied: true},
				{name: "local membership removed", role: "member", grant: "write", scope: apikey.ScopeRead, denied: true, change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
					operatorSQL(t, f, "UPDATE hosted_members SET active=0 WHERE user_id=?", u.identity.Subject)
				}},
				{name: "local role downgraded", role: "owner", grant: "write", scope: apikey.ScopeWrite, denied: true, change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
					operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", u.identity.Subject)
				}},
				{name: "grant removed after discovery", role: "member", grant: "write", scope: apikey.ScopeRead, denied: true, change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
					operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", u.identity.Subject)
				}},
				{name: "write grant downgraded", role: "member", grant: "write", scope: apikey.ScopeWrite, denied: true, change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
					operatorSQL(t, f, "UPDATE hosted_project_grants SET can_write=0 WHERE user_id=?", u.identity.Subject)
				}},
				{name: "session revoked", role: "owner", grant: "write", scope: apikey.ScopeRead, denied: true, change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
					operatorSQL(t, f, "UPDATE hosted_sessions SET revoked_at=?", formatHubTime(time.Now()))
				}},
				{name: "session expired", role: "owner", grant: "write", scope: apikey.ScopeRead, denied: true, change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
					operatorSQL(t, f, "UPDATE hosted_sessions SET expires_at=?", formatHubTime(time.Now().Add(-time.Hour)))
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					var shared hostedSharedFixture
					var f hostedSecurityFixture
					if deployment == "shared" {
						shared = newHostedSharedFixture(t)
						f = shared.hostedSecurityFixture
					} else {
						f = newHostedSecurityFixture(t)
					}
					u := f.user(t, "operator", test.role, "operator@example.test", test.grant, "")
					project := test.project
					if project == "" && test.kind == "" {
						project = string(f.project)
					}
					var request *http.Request
					f.service.echo.POST("/authority-test", func(c echo.Context) error {
						request = c.Request()
						return c.NoContent(http.StatusOK)
					}, f.service.operatorAuthority)
					var status int
					if deployment == "shared" {
						csrf := cloudassert.CSRFToken("shared-"+u.identity.Subject, "org_security")
						response := shared.serve(t, hostedSharedRequest{user: &u, method: http.MethodPost, target: "/organizations/org_security/authority-test", csrf: csrf})
						status = response.Code
					} else {
						status = f.request(t, u, http.MethodPost, "/authority-test", nil).Code
					}
					if status != http.StatusOK {
						t.Fatalf("initial auth=%d", status)
					}
					if test.change != nil {
						test.change(t, f, u)
					}
					_, err := operatortool.AuthorizeCurrent(request.Context(), operatortool.Requirement{Scope: test.scope, ProjectID: project, ResourceKind: test.kind})
					if (err != nil) != test.denied {
						t.Fatalf("authorization=%v want denied=%t", err, test.denied)
					}
					// Direct tools/call reaches the identical adapter, even if a
					// client never listed tools. No absent runtime is touched.
					_, callErr := operatortool.NewAuthorizedExecutor(nil).Execute(request.Context(), operatortool.Call{Name: operatortool.BoardState, Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)})
					if test.kind == "" && test.scope == apikey.ScopeRead && test.denied && !errors.Is(callErr, operatortool.ErrAccessDenied) {
						t.Fatalf("direct call=%v", callErr)
					}
					// New work reads must also resolve grants/roles on direct calls.
					if test.kind == "" && test.scope == apikey.ScopeRead && test.denied {
						for _, tool := range []string{operatortool.WorkList, operatortool.WorkItem, operatortool.WorkComments, operatortool.WorkReferences} {
							arguments := map[string]any{"project_id": project}
							if tool != operatortool.WorkList {
								arguments["reference"] = "wi_hidden"
							}
							raw, err := json.Marshal(arguments)
							if err != nil {
								t.Fatal(err)
							}
							_, err = operatortool.NewAuthorizedExecutor(operatortool.NewExecutor(operatortool.Dependencies{})).Execute(request.Context(), operatortool.Call{Name: tool, Arguments: raw})
							if !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("%s direct call=%v", tool, err)
							}
						}
					}
					if test.kind == "" && test.scope == apikey.ScopeRead {
						_, err := (hubProjectExecutor{f.service}).Execute(request.Context(), operatortool.Call{Name: "get_native_project", Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)})
						if (err != nil) != test.denied {
							t.Fatalf("direct project read=%v want denied=%v", err, test.denied)
						}
					} else if test.kind == "" && test.denied {
						name := "save_onboarding"
						if test.scope == apikey.ScopeAdmin {
							name = "update_project_integration"
						}
						_, err := (hubProjectExecutor{f.service}).Execute(request.Context(), operatortool.Call{Name: name, Arguments: json.RawMessage(`{"project_id":"` + project + `","request_id":"denied","input":{}}`)})
						if !errors.Is(err, operatortool.ErrAccessDenied) {
							t.Fatalf("direct project write=%v", err)
						}
					}
					if _, err := operatortool.AuthorizeCurrent(request.Context(), operatortool.Requirement{Scope: apikey.ScopeRead, OrganizationID: "other", ProjectID: project}); err == nil {
						t.Fatal("foreign organization authorized")
					}
					if _, err := operatortool.AuthorizeCurrent(request.Context(), operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: project, ResourceKind: "unknown", ResourceID: "foreign"}); err == nil {
						t.Fatal("unowned resource authorized")
					}
				})
			}
		})
	}
}

func operatorSQL(t *testing.T, f hostedSecurityFixture, query string, args ...any) {
	t.Helper()
	if _, err := f.service.database.db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestHubMCPOperatorCredentialBoundary(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "mcp-worker", "scope": "worker"})
	requireNativeStatus(t, response, http.StatusCreated)
	var worker tokenResponse
	decodeHubResponse(t, response, &worker)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
	for _, test := range []struct {
		name, token, target string
		status              int
	}{
		{"operator", f.token, path, http.StatusOK},
		{"administrator", testHubAdminToken, path, http.StatusOK},
		{"worker cannot become operator", worker.Token, path, http.StatusForbidden},
		{"no credential", "", path, http.StatusUnauthorized},
		{"foreign organization", f.token, "/api/v2/organizations/other/mcp", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodPost, test.target, test.token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}}})
			if response.Code != test.status {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
}
