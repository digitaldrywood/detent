package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type catalogCountingProvider struct {
	auth.HostedProvider
	calls int
}

func (p *catalogCountingProvider) Memberships(ctx context.Context, user, organization string) ([]auth.Membership, error) {
	p.calls++
	return p.HostedProvider.Memberships(ctx, user, organization)
}

func (p *catalogCountingProvider) CurrentSession(ctx context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.calls++
	return p.HostedProvider.CurrentSession(ctx, identity)
}

type catalogCountingApplication struct {
	operatoradmin.Application
	calls int
}

func (a *catalogCountingApplication) Authorize(ctx context.Context, name string, input operatoradmin.Input, resource string) error {
	a.calls++
	return a.Application.Authorize(ctx, name, input, resource)
}

func TestHubCatalogProviderCalls(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, test := range []struct {
			name    string
			scope   apikey.Scope
			role    string
			runners bool
		}{
			{"browser", "", "owner", false},
			{"read key", apikey.ScopeRead, "owner", false},
			{"write key", apikey.ScopeWrite, "owner", false},
			{"admin key", apikey.ScopeAdmin, "owner", false},
			{"browser runner grants", "", "owner", true},
			{"admin key runner grants", apikey.ScopeAdmin, "owner", true},
			{"member browser", "", "member", false},
			{"member write key", apikey.ScopeWrite, "member", false},
			{"member read key", apikey.ScopeRead, "member", false},
			{"viewer browser", "", "viewer", false},
		} {
			t.Run(deployment+"/"+test.name, func(t *testing.T) {
				f, mcpCtx := newHostedKeyMCPFixture(t, deployment, test.role)
				ctx := mcpCtx
				credential, err := currentHubOperator(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if test.runners {
					f.grant(t, f.user, true, true)
				}
				if test.scope != "" {
					scope := apiScopeOperator
					if test.scope == apikey.ScopeAdmin {
						scope = apiScopeAdmin
					}
					expiry := time.Now().Add(time.Hour)
					key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "catalog-test", Scope: scope, Issuer: &credential, KeyScope: test.scope, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}})
					if err != nil {
						t.Fatal(err)
					}
					credential, _, err = f.service.authenticateAPIToken(t.Context(), key.Token, "", "")
					if err != nil {
						t.Fatal(err)
					}
					connection := operatortool.CurrentConnection(ctx)
					connection.Identity = operatorIdentity(credential, "org_security")
					connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
						current, _, err := f.service.authenticateAPIToken(ctx, key.Token, "", "")
						if err != nil {
							return operatortool.Authority{}, err
						}
						return f.service.operatorCurrentAuthority(ctx, current, "org_security")
					}
					ctx = operatortool.WithConnection(ctx, connection)
					resolveCredential := func(ctx context.Context) (apiCredential, error) {
						current, _, err := f.service.authenticateAPIToken(ctx, key.Token, "", "")
						return current, err
					}
					ctx = context.WithValue(ctx, hubOperatorResolverKey{}, resolveCredential)
					ctx = context.WithValue(ctx, operatorCredentialKey{}, billingAuthorization(resolveCredential))
				}
				ctx = f.service.withOperatorCatalog(ctx, credential, "org_security")
				provider := &catalogCountingProvider{HostedProvider: f.service.config.Hosted.Provider}
				f.service.config.Hosted.Provider = provider
				application := &catalogCountingApplication{Application: f.service.administration.App}
				f.service.administration.App = application
				connection := operatortool.CurrentConnection(ctx)
				resolve := connection.Resolve
				resolutions := 0
				connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
					resolutions++
					return resolve(ctx)
				}
				ctx = operatortool.WithConnection(ctx, connection)
				for _, part := range []struct {
					name string
					list func(context.Context) ([]operatortool.Definition, error)
				}{
					{"hosted context", func(ctx context.Context) ([]operatortool.Definition, error) {
						return (hostedContextExecutor{f.service}).listTools(ctx), nil
					}},
					{"work", (nativeOperatorExecutor{f.service}).ListTools},
					{"projects", (hubProjectExecutor{f.service}).ListTools},
					{"workspace", (workspaceOperatorExecutor{f.service}).ListTools},
					{"changes", (hubOperatorExecutor{f.service}).ListTools},
					{"administration", f.service.administrationCatalog},
					{"fleet", (hubFleetExecutor{f.service}).ListTools},
					{"combined", (hostedOperatorExecutor{f.service}).ListTools},
					{"combined with services", func(ctx context.Context) ([]operatortool.Definition, error) {
						conversations, workspaces := f.service.conversations, f.service.workspaces
						defer func() {
							f.service.conversations, f.service.workspaces = conversations, workspaces
						}()
						f.service.conversations, f.service.workspaces = &conversationService{}, &workspaceService{}
						return (hostedOperatorExecutor{f.service}).ListTools(ctx)
					}},
				} {
					t.Run(part.name, func(t *testing.T) {
						provider.calls, application.calls, resolutions = 0, 0, 0
						start := time.Now()
						definitions, err := part.list(ctx)
						if err != nil {
							t.Fatal(err)
						}
						t.Logf("ListTools part=%s elapsed=%s provider_calls=%d resolutions=%d application_calls=%d tools=%d", part.name, time.Since(start), provider.calls, resolutions, application.calls, len(definitions))
						if part.name == "combined" || part.name == "combined with services" {
							hasRunnerTool := slices.ContainsFunc(definitions, func(d operatortool.Definition) bool { return d.Name == operatortool.UpdateRunnerCapacity })
							wantRunners := test.runners || (test.role == "owner" || test.role == "admin") && (test.scope == "" || test.scope == apikey.ScopeAdmin)
							if hasRunnerTool != wantRunners {
								t.Fatalf("runner tool visibility=%v, grants=%v", hasRunnerTool, test.runners)
							}
							projectTools := []string{operatortool.FileIssue, operatortool.EditItem, operatortool.CreateChange, "save_onboarding"}
							if part.name == "combined with services" {
								projectTools = append(projectTools, "create_workspace")
							}
							for _, name := range projectTools {
								visible := slices.ContainsFunc(definitions, func(d operatortool.Definition) bool { return d.Name == name })
								writable := test.role != "viewer" && test.scope != apikey.ScopeRead
								if visible != writable {
									t.Fatalf("project tool %s visibility=%v, want %v", name, visible, writable)
								}
							}
							if test.role != "owner" {
								for _, name := range []string{operatortool.ApproveChangeReviewPolicy, operatortool.InvitationSend, "create_hosted_project"} {
									if slices.ContainsFunc(definitions, func(d operatortool.Definition) bool { return d.Name == name }) {
										t.Fatalf("role %s discovered organization/admin tool %s", test.role, name)
									}
								}
							}
						}
						if provider.calls != 0 || resolutions != 0 || application.calls != 0 {
							t.Fatalf("catalog performed remote authorization: provider=%d resolutions=%d application=%d", provider.calls, resolutions, application.calls)
						}
					})
				}
				provider.calls, resolutions = 0, 0
				workArguments, err := json.Marshal(map[string]any{"project_id": string(f.project), "limit": 1})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkList, Arguments: workArguments}); err != nil {
					t.Fatal(err)
				}
				if resolutions == 0 || test.scope != "" && provider.calls == 0 {
					t.Fatalf("execution skipped current authority: provider=%d resolutions=%d", provider.calls, resolutions)
				}
				if test.role == "member" || test.role == "viewer" {
					for index, scenario := range []string{"current project access", "removed project grant", "revoked principal"} {
						if index == 1 {
							operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
						}
						if index == 2 {
							f.grant(t, f.user, true, false)
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), credential.ID)
						}
						arguments, err := json.Marshal(map[string]any{"project_id": string(f.project), "request_id": "catalog-parity-" + strconv.Itoa(index), "title": "Project member write", "description": "Current authority fixture", "state": "Todo"})
						if err != nil {
							t.Fatal(err)
						}
						_, err = (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: arguments})
						denied := index > 0 || test.role == "viewer" || test.scope == apikey.ScopeRead
						if denied && !errors.Is(err, operatortool.ErrAccessDenied) || !denied && err != nil {
							t.Fatalf("%s write denied=%v, err=%v", scenario, denied, err)
						}
					}
				}
				ctx = operatortool.WithConnection(ctx, operatortool.Connection{Identity: operatortool.Identity{PrincipalID: "foreign", OrganizationID: "org_security", CredentialID: "foreign"}})
				if _, err := (hostedOperatorExecutor{f.service}).ListTools(ctx); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("foreign connection discovery=%v", err)
				}
			})
		}
	}
}
