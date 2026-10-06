package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func modelSelectionCall(t *testing.T, name, project, key string, input operatortool.ModelSelectionInput) operatortool.Call {
	t.Helper()
	call := projectCall(t, name, project, key, input)
	if operatortool.IsOrganizationModelSelection(name) {
		raw, err := json.Marshal(struct {
			RequestID string                           `json:"request_id"`
			Input     operatortool.ModelSelectionInput `json:"input"`
		}{key, input})
		if err != nil {
			t.Fatal(err)
		}
		call.Arguments = raw
	}
	return call
}

func TestHostedModelSelectionTools(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "owner")
			executor := hostedOperatorExecutor{f.service}
			definitions, err := executor.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, definition := range operatortool.ModelSelectionCatalog() {
				if !slices.ContainsFunc(definitions, func(d operatortool.Definition) bool { return d.Name == definition.Name }) {
					t.Fatalf("missing registered deployment tool %s", definition.Name)
				}
			}
			read := func(project string) cloudModelSelection {
				t.Helper()
				name := operatortool.GetOrganizationModelSelection
				if project != "" {
					name = operatortool.GetProjectModelSelection
				}
				raw, err := json.Marshal(operatortool.ModelSelectionReadRequest{ProjectID: project})
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				var view struct {
					Data cloudModelSelection `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &view); err != nil {
					t.Fatal(err)
				}
				return view.Data
			}
			var replayProjectCall, replayOrganizationCall operatortool.Call
			var replayProjectAction chatpkg.Action
			initial := read("")
			selection := *initial.Selection
			selection.NormalModel = new("organization-model")
			for _, test := range []struct {
				name, project, model string
				selection            *config.ModelSelection
			}{
				{"organization", "", "organization-model", &selection},
				{"override", string(f.project), "project-model", &config.ModelSelection{NormalModel: new("project-model")}},
				{"inherit", string(f.project), "organization-model", nil},
			} {
				t.Run(test.name, func(t *testing.T) {
					current := read(test.project)
					name := operatortool.UpdateOrganizationModelSelection
					if test.project != "" {
						name = operatortool.UpdateProjectModelSelection
					}
					input := operatortool.ModelSelectionInput{ExpectedRevision: current.Revision, Selection: test.selection}
					call := modelSelectionCall(t, name, test.project, test.name, input)
					result, err := executor.Execute(ctx, call)
					if err != nil {
						t.Fatal(err)
					}
					var action chatpkg.Action
					if err := json.Unmarshal(result.Content, &action); err != nil {
						t.Fatal(err)
					}
					if action.Status != chatpkg.ActionSucceeded {
						t.Fatalf("action=%+v", action)
					}
					if test.project == "" {
						replayOrganizationCall = call
					} else {
						replayProjectCall, replayProjectAction = call, action
					}
					saved := read(test.project)
					if saved.Revision != current.Revision+1 || saved.Effective.Model("normal") != test.model || *saved.Effective.Stages["plan"].Effort != "low" || (saved.Selection == nil) != (test.selection == nil) {
						t.Fatalf("saved=%+v", saved)
					}
					for _, retryCtx := range []context.Context{ctx, operatortool.BindConnection(ctx, "reconnect-"+test.name, "test")} {
						if err := executor.OpenConnection(retryCtx); err != nil {
							t.Fatal(err)
						}
						replay, err := executor.Execute(retryCtx, call)
						if err != nil {
							t.Fatal(err)
						}
						var replayAction chatpkg.Action
						if err := json.Unmarshal(replay.Content, &replayAction); err != nil {
							t.Fatal(err)
						}
						if replayAction.Status != chatpkg.ActionSucceeded || replayAction.Result != action.Result {
							t.Fatalf("replay=%+v", replayAction)
						}
					}
					if _, err := executor.Execute(ctx, modelSelectionCall(t, name, test.project, test.name+"-stale", input)); !errors.Is(err, mutation.ErrConflict) {
						t.Fatalf("stale revision=%v", err)
					}
					input.Selection = &config.ModelSelection{NormalModel: new("changed")}
					if _, err := executor.Execute(operatortool.BindConnection(ctx, "changed-"+test.name, "test"), modelSelectionCall(t, name, test.project, test.name, input)); !errors.Is(err, mutation.ErrConflict) {
						t.Fatalf("changed retry=%v", err)
					}
					if deployment == "dedicated" {
						path := "/api/v2/organizations/org_security/model-selection"
						if test.project != "" {
							path = f.base + "/model-selection"
						}
						request := cloudModelSelectionRequest{ExpectedRevision: current.Revision, Selection: test.selection}
						request.IdempotencyKey = test.name
						response := f.request(t, f.user, http.MethodPut, path, request)
						requireNativeStatus(t, response, http.StatusOK)
						var delivered cloudModelSelection
						decodeHubResponse(t, response, &delivered)
						if delivered.Revision != saved.Revision {
							t.Fatalf("HTTP replay=%+v", delivered)
						}
					}
				})
			}
			for _, name := range []string{operatortool.UpdateOrganizationModelSelection, operatortool.UpdateProjectModelSelection} {
				project := ""
				if name == operatortool.UpdateProjectModelSelection {
					project = string(f.project)
				}
				current := read(project)
				_, err := executor.Execute(ctx, modelSelectionCall(t, name, project, "invalid-"+name, operatortool.ModelSelectionInput{ExpectedRevision: current.Revision, Selection: &config.ModelSelection{Enabled: new(true), DefaultLevel: new("missing")}}))
				if err == nil || read(project).Revision != current.Revision {
					t.Fatalf("invalid effective selection saved: %v", err)
				}
			}
			operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
			for _, retryCtx := range []context.Context{ctx, operatortool.BindConnection(ctx, "revoked-replay", "test")} {
				if _, err := executor.Execute(retryCtx, replayProjectCall); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revoked replay=%v", err)
				}
			}
			actionRead := operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + replayProjectAction.ID + `"}`)}
			if _, err := executor.Execute(ctx, actionRead); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked action result=%v", err)
			}
			if _, err := executor.ExecuteAction(ctx, replayProjectAction); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked execution=%v", err)
			}
			if err := f.provider.SetMembershipRole(t.Context(), "membership_"+f.user.identity.Subject, "member"); err != nil {
				t.Fatal(err)
			}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='member' WHERE user_id=?", f.user.identity.Subject)
			if _, err := executor.Execute(ctx, replayOrganizationCall); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("downgraded organization replay=%v", err)
			}
		})
	}
}

func TestModelSelectionToolAuthority(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, test := range []struct {
			name, role                                                     string
			key                                                            apikey.Scope
			removeGrant                                                    bool
			organizationRead, projectRead, organizationWrite, projectWrite bool
		}{
			{"owner", "owner", "", false, true, true, true, true},
			{"admin", "admin", "", false, true, true, true, true},
			{"member", "member", "", false, true, true, false, false},
			{"viewer", "viewer", "", false, true, true, false, false},
			{"no grant", "owner", "", true, true, false, true, false},
			{"read key", "owner", apikey.ScopeRead, false, false, true, false, false},
			{"write key", "owner", apikey.ScopeWrite, false, false, true, false, false},
			{"admin key", "owner", apikey.ScopeAdmin, false, false, true, false, true},
		} {
			t.Run(deployment+"/"+test.name, func(t *testing.T) {
				f, ctx := newHostedKeyMCPFixture(t, deployment, test.role)
				if test.key != "" {
					issuer, err := currentHubOperator(ctx)
					if err != nil {
						t.Fatal(err)
					}
					expiry := time.Now().Add(time.Hour)
					scope := apiScopeOperator
					if test.key == apikey.ScopeAdmin {
						scope = apiScopeAdmin
					}
					key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "model-tools", Scope: scope, Issuer: &issuer, KeyScope: test.key, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}})
					if err != nil {
						t.Fatal(err)
					}
					credential, _, err := f.service.authenticateAPIToken(t.Context(), key.Token, "", "")
					if err != nil {
						t.Fatal(err)
					}
					connection := operatortool.CurrentConnection(ctx)
					connection.ID = "key-model-selection"
					connection.Identity = operatorIdentity(credential, "org_security")
					connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
						current, _, err := f.service.authenticateAPIToken(ctx, key.Token, "", "")
						if err != nil {
							return operatortool.Authority{}, err
						}
						return f.service.operatorCurrentAuthority(ctx, current, "org_security")
					}
					ctx = operatortool.WithConnection(ctx, connection)
					ctx = f.service.withOperatorCatalog(ctx, credential, "org_security")
					if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if test.removeGrant {
					operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
				}
				for _, operation := range []struct {
					name, project string
					read, allowed bool
				}{
					{operatortool.GetOrganizationModelSelection, "", true, test.organizationRead},
					{operatortool.GetProjectModelSelection, string(f.project), true, test.projectRead},
					{operatortool.UpdateOrganizationModelSelection, "", false, test.organizationWrite},
					{operatortool.UpdateProjectModelSelection, string(f.project), false, test.projectWrite},
				} {
					call := modelSelectionCall(t, operation.name, operation.project, operation.name, operatortool.ModelSelectionInput{ExpectedRevision: 1, Selection: &config.ModelSelection{Enabled: new(false)}})
					if operation.read {
						call.Arguments, _ = json.Marshal(operatortool.ModelSelectionReadRequest{ProjectID: operation.project})
					}
					_, err := (hostedOperatorExecutor{f.service}).Execute(ctx, call)
					if (err == nil) != operation.allowed {
						t.Fatalf("%s error=%v allowed=%v", operation.name, err, operation.allowed)
					}
				}
			})
		}
	}
}

func TestNativeModelSelectionTools(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/model-tool-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "native-model-tools", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/model-tool-test"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
	ctx := <-contexts
	executor := hostedOperatorExecutor{f.service}
	if err := executor.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	definitions, err := executor.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range definitions {
		if operatortool.IsOrganizationModelSelection(d.Name) {
			t.Fatalf("native hub exposes hosted session operation %s", d.Name)
		}
	}
	for _, project := range []string{string(f.project.ID), "foreign-project"} {
		call := operatortool.Call{Name: operatortool.GetProjectModelSelection, Arguments: json.RawMessage(`{"project_id":"` + project + `"}`)}
		_, err := executor.Execute(ctx, call)
		if (err == nil) != (project == string(f.project.ID)) {
			t.Fatalf("project=%s error=%v", project, err)
		}
	}
	call := modelSelectionCall(t, operatortool.UpdateProjectModelSelection, string(f.project.ID), "native-selection", operatortool.ModelSelectionInput{ExpectedRevision: 1, Selection: &config.ModelSelection{NormalModel: new("native-model")}})
	result, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var action chatpkg.Action
	if err := json.Unmarshal(result.Content, &action); err != nil {
		t.Fatal(err)
	}
	var saved cloudModelSelection
	if err := json.Unmarshal([]byte(action.Result), &saved); err != nil {
		t.Fatal(err)
	}
	if action.Status != chatpkg.ActionSucceeded || saved.Revision != 2 || saved.Effective.Model("normal") != "native-model" {
		t.Fatalf("saved=%+v action=%+v", saved, action)
	}
	if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.GetOrganizationModelSelection, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("native organization authority=%v", err)
	}
}
