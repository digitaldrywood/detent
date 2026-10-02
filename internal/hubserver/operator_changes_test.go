package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func changeOperatorContext(t *testing.T, s *Service, token, organization string) context.Context {
	t.Helper()
	credential, _, err := s.authenticateAPIToken(t.Context(), token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	connection := operatortool.Connection{ID: "test-connection", Client: "test-client", Identity: operatorIdentity(credential, organization), Resolve: func(ctx context.Context) (operatortool.Authority, error) {
		current, _, err := s.authenticateAPIToken(ctx, token, "", "")
		if err != nil {
			return operatortool.Authority{}, err
		}
		return s.operatorCurrentAuthority(ctx, current, organization)
	}}
	return operatortool.WithConnection(t.Context(), connection)
}
func changeToolCall(name string, args operatortool.ChangeArguments) operatortool.Call {
	raw, _ := json.Marshal(args)
	return operatortool.Call{Name: name, Arguments: raw}
}

func TestOperatorChangePublication(t *testing.T) {
	for _, test := range []struct {
		name           string
		review, checks bool
		availability   string
		lane, status   string
	}{
		{"automatic promotion", false, false, "available", "Merging", "reviewed"},
		{"unverified artifacts", false, false, "unverified", "Merging", "reviewed"},
		{"required review", true, false, "available", "Human Review", "needs_evidence"},
		{"required checks", false, true, "available", "Human Review", "needs_evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			states := append(nativeFixtureStates(), tracker.NativeState{Name: "Human Review", Transitions: []string{"Merging"}}, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}})
			raw, err := json.Marshal(states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, state := range states[3:] {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Human Review') WHERE native_id=?", f.project.ID, f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			rules := f.rules
			rules.RequireReview = test.review
			if !test.checks {
				rules.RequiredChecks = []tracker.ChangeCheckSpec{}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "publication-rules"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			executor := hubOperatorExecutor{f.service}
			created, err := executor.Execute(ctx, changeToolCall(operatortool.CreateChange, operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), RequestID: "operator-create", Title: "Human source"}))
			if err != nil {
				t.Fatal(err)
			}
			var result operatortool.ChangeResult
			if err := json.Unmarshal(created.Content, &result); err != nil {
				t.Fatal(err)
			}
			f.change.ID, f.path = result.ChangeID, result.URL
			input := changeTestInput()
			input.Code.Availability = test.availability
			expected := ""
			args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: result.ChangeID, RequestID: "operator-publish", ExpectedVersionID: &expected, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, MergeBaseSHA: input.MergeBaseSHA, Repository: input.Repository, Code: &input.Code, Artifacts: input.Artifacts, PolicyID: input.PolicyID, External: &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://github.com/example/repo/pull/42"}}
			for _, denied := range []struct {
				name string
				edit func(*operatortool.ChangeArguments)
			}{
				{"stale current version", func(a *operatortool.ChangeArguments) { v := "version_stale"; a.ExpectedVersionID = &v }},
				{"stale policy", func(a *operatortool.ChangeArguments) { a.PolicyID = "policy_stale" }},
				{"foreign project", func(a *operatortool.ChangeArguments) { a.ProjectID = "prj_foreign" }},
				{"foreign change", func(a *operatortool.ChangeArguments) { a.ChangeID = "change_foreign" }},
				{"foreign item", func(a *operatortool.ChangeArguments) { a.ItemID = "wi_foreign" }},
				{"forged PR identity", func(a *operatortool.ChangeArguments) {
					external := *a.External
					external.ID = "43"
					a.External = &external
				}},
			} {
				t.Run(denied.name, func(t *testing.T) {
					a := args
					a.RequestID = denied.name
					denied.edit(&a)
					if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, a)); err == nil {
						t.Fatal("invalid publication succeeded")
					}
				})
			}
			foreign := changeOperatorContext(t, f.service, f.token, "org_foreign")
			if _, err := executor.Execute(foreign, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("foreign organization: %v", err)
			}
			worker := changeOperatorContext(t, f.service, f.worker(t, "publication-worker"), string(f.project.OrganizationID))
			if _, err := executor.Execute(worker, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("worker operator publication: %v", err)
			}
			if len(f.detail(t).Versions) != 0 {
				t.Fatal("denied publication had effects")
			}
			call := changeToolCall(operatortool.PublishChangeVersion, args)
			published, err := executor.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(published.Content, &result); err != nil {
				t.Fatal(err)
			}
			if result.Version == nil || result.Detail == nil || result.Detail.Change.CurrentVersion != result.Version.ID || result.WorkItemState != test.lane || result.Detail.Summary.Status != test.status || result.Detail.Summary.ExternalReview != "external_gate" {
				t.Fatalf("publication effects: %s", published.Content)
			}
			version := *result.Version
			if version.RunID != "" || version.AttemptID != "" || version.Actor != (tracker.Actor{Kind: "human", PrincipalID: operatortool.ConnectionIdentity(ctx).PrincipalID}) || version.Code.Availability != test.availability || version.External == nil || *version.External != *args.External || len(version.Checks) != len(rules.RequiredChecks) {
				t.Fatalf("forged publication identity or policy: %#v", version)
			}
			originalReceipt := result.Receipt
			connection := operatortool.CurrentConnection(ctx)
			connection.ID = "publication-reconnect"
			ctx = operatortool.WithConnection(ctx, connection)
			replayed, err := executor.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(replayed.Content, &result) != nil || !bytes.Equal(originalReceipt, result.Receipt) || len(f.detail(t).Versions) != 1 {
				t.Fatal("replay changed identity or repeated effects")
			}
			input.External = args.External
			rest := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: args.RequestID}, ChangeVersionInput: input})
			requireNativeStatus(t, rest, http.StatusOK)
			if !bytes.Equal(bytes.TrimSpace(rest.Body.Bytes()), originalReceipt) {
				t.Fatal("REST and MCP do not share the publication receipt")
			}
			args.HeadSHA = strings.Repeat("d", 40)
			if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("changed retry: %v", err)
			}
			args.RequestID = "stale-current"
			if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale current: %v", err)
			}
			args.RequestID, expected = "second-publication", version.ID
			if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, args)); err != nil {
				t.Fatal(err)
			}
			replayed, err = executor.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(replayed.Content, &result) != nil || result.Version.ID != version.ID || result.Detail.Change.CurrentVersion == version.ID || !bytes.Equal(originalReceipt, result.Receipt) || len(f.detail(t).Versions) != 2 {
				t.Fatal("historical replay lost original receipt or live current version")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id=(SELECT id FROM api_tokens WHERE token_hash=?)", operatortool.ConnectionIdentity(ctx).CredentialID); err != nil {
				t.Fatal(err)
			}
			if _, err := executor.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("lost authority replay: %v", err)
			}
		})
	}
}

// Catches discovery bypass reaching foreign nested resources, stale review
// identities, and replay repeating discussion effects after reconnect.
func TestOperatorChangeCommands(t *testing.T) {
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
	version := f.publish(t, "v1", "")
	bundle := seedReviewBundle(t, f, version, time.Now().Add(time.Hour))
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	executor := hubOperatorExecutor{f.service}
	base := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID}
	for _, test := range []struct {
		name, tool string
		edit       func(*operatortool.ChangeArguments)
		want       error
	}{
		{"detail", operatortool.GetChange, nil, nil},
		{"PR panel", operatortool.ListWorkItemPullRequests, func(a *operatortool.ChangeArguments) { a.ChangeID = "" }, nil},
		{"missing library", operatortool.ArtifactLibrary, func(a *operatortool.ChangeArguments) { a.ChangeID = ""; a.ItemID = "" }, operatortool.ErrServiceUnavailable},
		{"version", operatortool.GetChangeVersion, func(a *operatortool.ChangeArguments) { a.VersionID = version.ID }, nil},
		{"foreign project", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.ProjectID = "prj_foreign" }, operatortool.ErrAccessDenied},
		{"foreign item", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.ItemID = "wi_foreign" }, operatortool.ErrAccessDenied},
		{"foreign change", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.ChangeID = "change_foreign" }, operatortool.ErrAccessDenied},
		{"foreign version", operatortool.GetChangeVersion, func(a *operatortool.ChangeArguments) { a.VersionID = "version_foreign" }, operatortool.ErrAccessDenied},
		{"missing publication identity", operatortool.PublishChangeVersion, nil, operatortool.ErrInvalidArguments},
		{"worker landing", "land_change", nil, operatortool.ErrUnknownTool},
		{"worker CI", "submit_change_check", nil, operatortool.ErrUnknownTool},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := base
			if test.edit != nil {
				test.edit(&args)
			}
			result, err := executor.Execute(ctx, changeToolCall(test.tool, args))
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}

			if test.tool == operatortool.ListWorkItemPullRequests {
				var value operatortool.ChangeResult
				if json.Unmarshal(result.Content, &value) != nil || len(value.PullRequests) != 1 || value.PullRequests[0].ChangeID != f.change.ID || value.PullRequests[0].FetchedAt.IsZero() {
					t.Fatalf("missing PR panel identity/freshness: %s", result.Content)
				}
			}
		})
	}
	args := base
	args.RequestID = "discussion"
	args.Body = "message"
	call := changeToolCall(operatortool.DiscussChange, args)
	first, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	reconnect := operatortool.CurrentConnection(ctx)
	reconnect.ID = "reconnected"
	ctx = operatortool.WithConnection(ctx, reconnect)
	second, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var one, two operatortool.ChangeResult
	if json.Unmarshal(first.Content, &one) != nil || json.Unmarshal(second.Content, &two) != nil || !bytes.Equal(one.Receipt, two.Receipt) {
		t.Fatal("replay changed command receipt")
	}
	args.Body = "different"
	if _, err := executor.Execute(ctx, changeToolCall(operatortool.DiscussChange, args)); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if count := len(f.detail(t).Discussion); count != 1 {
		t.Fatalf("discussion effects=%d", count)
	}
	args = base
	args.VersionID = version.ID
	args.RequestID = "review"
	args.ExpectedRevision = int64(f.detail(t).Change.Revision)
	args.Decision = "approved"
	args.Bundle = &bundle
	for _, test := range []struct {
		name string
		edit func(*operatortool.ChangeArguments)
	}{
		{"stale revision", func(a *operatortool.ChangeArguments) { a.ExpectedRevision++ }},
		{"forged bundle", func(a *operatortool.ChangeArguments) {
			b := *a.Bundle
			b.SHA256 = strings.Repeat("f", 64)
			a.Bundle = &b
		}},
		{"stale version", func(a *operatortool.ChangeArguments) { a.VersionID = "version_stale" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := args
			test.edit(&a)
			authorized, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: "write", ProjectID: a.ProjectID})
			if err != nil {
				t.Fatal(err)
			}
			app, err := operatortool.CurrentChanges(authorized)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = app.MutateChange(authorized, operatortool.ReviewChange, a); err == nil {
				t.Fatal("invalid review succeeded")
			}
		})
	}
	if len(f.detail(t).Reviews) != 0 {
		t.Fatal("denied review had side effects")
	}
	if _, err := executor.Execute(ctx, changeToolCall(operatortool.ReviewChange, args)); !errors.Is(err, operatortool.ErrServiceUnavailable) {
		t.Fatalf("absent approval service: %v", err)
	}

	// Catches creation returning only the caller's empty selector rather than
	// the new application identity needed for a subsequent detail call.
	created, err := executor.Execute(ctx, changeToolCall(operatortool.CreateChange, operatortool.ChangeArguments{ProjectID: base.ProjectID, ItemID: base.ItemID, RequestID: "new-change", Title: "Follow-up change"}))
	if err != nil {
		t.Fatal(err)
	}
	var createdResult operatortool.ChangeResult
	if json.Unmarshal(created.Content, &createdResult) != nil || createdResult.ChangeID == "" || !strings.HasSuffix(createdResult.URL, "/changes/"+createdResult.ChangeID) {
		t.Fatalf("unusable creation result: %s", created.Content)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id=(SELECT id FROM api_tokens WHERE token_hash=?)", operatortool.ConnectionIdentity(ctx).CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("lost grant replay: %v", err)
	}
}

// Catches forged artifact identity, token persistence/logging, permission loss
// during replay, read grants incorrectly requiring write, and unusable URLs.
func TestOperatorArtifactResults(t *testing.T) {
	for _, scenario := range []string{"valid replay", "historical revision", "viewer", "forged hash", "foreign item", "missing service", "expired", "lost grant", "revoked session"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			deadline := time.Now().Add(time.Hour)
			if scenario == "expired" {
				deadline = time.Now().Add(-time.Minute)
			}
			ref, _, _ := seedHostedArtifact(t, f, deadline)
			role, grant := "member", "write"
			if scenario == "viewer" {
				role, grant = "viewer", "read"
			}
			u := f.user(t, "download", role, "download@example.test", grant, "")
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			ctxs := make(chan context.Context, 1)
			f.service.echo.GET("/capture-authority", func(c echo.Context) error {
				ctxs <- operatortool.BindConnection(c.Request().Context(), "download-connection", "generic-client")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			requireNativeStatus(t, f.request(t, u, http.MethodGet, "/capture-authority", nil), http.StatusOK)
			ctx := <-ctxs
			if scenario == "historical revision" {
				next := ref
				next.Revision++
				next.ManifestID = artifact.NewID("manifest")
				raw, _ := json.Marshal(next)
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO artifact_references(organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,reference_json) VALUES(?,?,?,?,?,?,?,?)`, next.OrganizationID, next.ProjectID, next.WorkItemID, next.ServiceID, next.ArtifactID, next.Revision, next.ManifestID, raw); err != nil {
					t.Fatal(err)
				}
			}
			args := operatortool.ChangeArguments{ProjectID: string(f.project), ItemID: ref.WorkItemID, ArtifactID: ref.ArtifactID, Revision: ref.Revision, SHA256: ref.SHA256, RequestID: "download"}
			ex := hubOperatorExecutor{f.service}
			switch scenario {
			case "forged hash":
				args.SHA256 = strings.Repeat("f", 64)
			case "foreign item":
				args.ItemID = "wi_foreign"
			case "missing service":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE artifact_services SET binding_json='{}'"); err != nil {
					t.Fatal(err)
				}
			}
			call := changeToolCall(operatortool.ArtifactAccess, args)
			result, err := ex.Execute(ctx, call)
			if scenario == "forged hash" || scenario == "foreign item" || scenario == "missing service" || scenario == "expired" {
				if err == nil {
					t.Fatal("invalid download succeeded")
				}
				var count int
				if e := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM artifact_grants").Scan(&count); e != nil || count != 0 {
					t.Fatalf("denied grant effect=%d: %v", count, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var value operatortool.ChangeResult
			if err := json.Unmarshal(result.Content, &value); err != nil {
				t.Fatal(err)
			}
			if value.Access == nil || value.Access.Grant.Token == "" || !strings.HasSuffix(value.Access.ManifestURL, "/manifests/1") || !strings.HasSuffix(value.Access.ObjectURLTemplate, "/manifests/1/objects/{object_id}") {
				t.Fatalf("unusable result: %s", result.Content)
			}
			token := value.Access.Grant.Token
			if strings.Contains(logs.String(), token) || strings.Contains(string(value.Receipt), token) {
				t.Fatal("secret in audit/receipt")
			}
			var receipt string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE command_key='download'").Scan(&receipt); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(receipt, token) {
				t.Fatal("persisted token")
			}
			if time.Until(value.Access.Grant.ExpiresAt) > time.Minute {
				t.Fatal("grant exceeded existing TTL")
			}
			if scenario == "lost grant" {
				operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", u.identity.Subject)
			}
			if scenario == "revoked session" {
				operatorSQL(t, f, "UPDATE hosted_sessions SET revoked_at=?", formatHubTime(time.Now()))
			}
			replay, err := ex.Execute(ctx, call)
			if scenario == "lost grant" || scenario == "revoked session" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("lost authority replay: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var repeated operatortool.ChangeResult
			if json.Unmarshal(replay.Content, &repeated) != nil || repeated.Access == nil || repeated.Access.Grant.Token == token {
				t.Fatal("replay must freshly authorize ephemeral access")
			}
		})
	}
}

// Catches forged API approval, material actions running before confirmation,
// stale policy previews, and approval under a downgraded originating role.
func TestHostedOperatorPolicyApproval(t *testing.T) {
	for _, tool := range []string{operatortool.ApproveChangeReviewPolicy, operatortool.BindArtifactService} {
		t.Run(tool, func(t *testing.T) {
			for _, scenario := range []string{"confirm", "reject", "YOLO", "role lost", "stale policy", "API approval", "policy floor", "key confirm", "key revoked", "key approval"} {
				if tool == operatortool.BindArtifactService && (scenario == "stale policy" || scenario == "policy floor") {
					continue
				}
				t.Run(scenario, func(t *testing.T) {
					f := newHostedSecurityFixture(t)
					u := f.user(t, "approver", "owner", "approver@example.test", "write", "")
					ctxs := make(chan context.Context, 1)
					f.service.echo.GET("/capture-authority", func(c echo.Context) error {
						ctxs <- operatortool.BindConnection(c.Request().Context(), "policy-connection", "generic-client")
						return c.NoContent(http.StatusOK)
					}, f.service.operatorAuthority)
					var key tokenResponse
					if strings.HasPrefix(scenario, "key ") {
						issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: u.identity.Hosted, Email: u.identity.Email}, apikey.HashToken(u.token))
						if err != nil {
							t.Fatal(err)
						}
						expiry := time.Now().Add(time.Hour)
						key, err = f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "approval-client", Scope: apiScopeAdmin, Issuer: &issuer, KeyScope: apikey.ScopeAdmin, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project)}})
						if err != nil {
							t.Fatal(err)
						}
						r := httptest.NewRequest(http.MethodGet, "/capture-authority", nil)
						r.Header.Set("Authorization", "Bearer "+key.Token)
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, r)
						requireNativeStatus(t, response, http.StatusOK)
					} else {
						requireNativeStatus(t, f.request(t, u, http.MethodGet, "/capture-authority", nil), http.StatusOK)
					}
					ctx := <-ctxs
					ex := hubOperatorExecutor{f.service}
					if err := ex.OpenConnection(ctx); err != nil {
						t.Fatal(err)
					}
					f.grant(t, u, true, true)
					descriptor := hubTestPolicy()
					requireNativeStatus(t, f.request(t, u, http.MethodPut, f.base+"/onboarding/policy", policy.Change{Policy: descriptor}), http.StatusOK)
					args := operatortool.ChangeArguments{ProjectID: string(f.project), RequestID: "policy", Policy: &tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}
					if scenario == "policy floor" {
						args.Policy.PolicyID = "forged-policy"
					}

					if tool == operatortool.BindArtifactService {
						seedHostedArtifact(t, f)
						args.Policy = nil
						args.Binding = &artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: "artifact-publisher"}
					}
					assertNoEffect := func() {
						t.Helper()
						if tool == operatortool.BindArtifactService {
							var count int
							if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM artifact_services WHERE id=?", args.Binding.ServiceID).Scan(&count); err != nil || count != 0 {
								t.Fatalf("denied binding side effects=%d: %v", count, err)
							}
						}
					}

					if scenario == "YOLO" {
						if err := f.service.operatorChat.SetConnectionMode(chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx)), "policy-connection", chat.YOLOMode); err != nil {
							t.Fatal(err)
						}
					}
					result, err := ex.Execute(ctx, changeToolCall(tool, args))

					if scenario == "YOLO" {
						if err != nil {
							t.Fatal(err)
						}
						var value struct {
							Status string `json:"status"`
						}
						if json.Unmarshal(result.Content, &value) != nil || value.Status != "succeeded" {
							t.Fatal("YOLO failed authorized command")
						}
						return
					}

					if err != nil {
						t.Fatal(err)
					}
					var preview struct {
						ID     string `json:"action_id"`
						Status string `json:"status"`
					}
					if json.Unmarshal(result.Content, &preview) != nil || preview.Status != "pending" {
						t.Fatalf("material action not pending: %s", result.Content)
					}
					assertNoEffect()
					page := f.request(t, u, http.MethodGet, "/chat/approval?connection_id=policy-connection", nil)
					requireNativeStatus(t, page, http.StatusOK)
					tokenRE := regexp.MustCompile(`name="form_token" value="([^"]+)"`)
					matches := tokenRE.FindAllStringSubmatch(page.Body.String(), -1)
					if len(matches) < 2 {
						t.Fatalf("approval form missing: %s", page.Body)
					}
					form := url.Values{"connection_id": {"policy-connection"}, "action_id": {preview.ID}, "decision": {"confirm"}, "form_token": {matches[1][1]}}
					if scenario == "reject" {
						form.Set("decision", "reject")
					}
					if scenario == "role lost" {
						operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", u.identity.Subject)
					}
					if scenario == "key revoked" {
						if err := f.service.revokeAPITokenFor(t.Context(), key.ID); err != nil {
							t.Fatal(err)
						}
					}

					if scenario == "role lost" {
						requireNativeStatus(t, f.request(t, u, http.MethodGet, "/capture-authority", nil), http.StatusOK)
						definitions, err := ex.ListTools(<-ctxs)
						if err != nil {
							t.Fatal(err)
						}
						for _, definition := range definitions {
							if definition.Name == tool {
								t.Fatal("material admin command discovered after role loss")
							}
						}
						if _, err := ex.Execute(ctx, changeToolCall(tool, args)); !errors.Is(err, operatortool.ErrAccessDenied) {
							t.Fatalf("role loss bypass: %v", err)
						}
					}
					if scenario == "stale policy" {
						args.RequestID = "concurrent-policy"
						args.Policy.RequireReview = true
						requireNativeStatus(t, f.request(t, u, http.MethodPut, f.base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "concurrent-policy"}, Policy: *args.Policy}), http.StatusOK)
					}
					var response *httptest.ResponseRecorder
					if scenario == "API approval" || scenario == "key approval" {
						r := httptest.NewRequest(http.MethodPost, "/chat/approval", strings.NewReader(form.Encode()))
						r.Header.Set("Content-Type", echo.MIMEApplicationForm)
						r.Header.Set("Authorization", "Bearer forged")
						if scenario == "key approval" {
							r.Header.Set("Authorization", "Bearer "+key.Token)
						}
						response = httptest.NewRecorder()
						f.service.echo.ServeHTTP(response, r)
					} else {
						response = f.request(t, u, http.MethodPost, "/chat/approval", form)
					}
					if scenario == "reject" {
						requireNativeStatus(t, response, http.StatusSeeOther)
						a, _ := f.service.operatorChat.Action("policy-connection", preview.ID)
						if a.Status != chat.ActionRejected {
							t.Fatal("rejection ignored")
						}
					} else if scenario == "confirm" || scenario == "key confirm" {
						requireNativeStatus(t, response, http.StatusSeeOther)
						action, _ := f.service.operatorChat.Action("policy-connection", preview.ID)
						if action.Status != chat.ActionSucceeded {
							t.Fatal("confirmed policy did not execute")
						}

						if _, err := ex.Execute(ctx, changeToolCall(tool, args)); err != nil {
							t.Fatalf("confirmed replay: %v", err)
						}
						if tool == operatortool.BindArtifactService {
							var count int
							if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE command_key=?", args.RequestID).Scan(&count); err != nil || count != 1 {
								t.Fatalf("binding replay receipts=%d: %v", count, err)
							}
						}
					} else if response.Code < 400 {
						t.Fatalf("invalid authorization/policy succeeded: %d", response.Code)
					}
					if scenario != "confirm" && scenario != "key confirm" {
						assertNoEffect()
					}
				})
			}
		})
	}
}
