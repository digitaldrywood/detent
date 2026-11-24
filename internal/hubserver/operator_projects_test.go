package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectCall[T any](t *testing.T, name, project, key string, input T) operatortool.Call {
	t.Helper()
	raw, err := json.Marshal(operatortool.ProjectRequest[T]{ProjectID: project, RequestID: key, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return operatortool.Call{Name: name, Arguments: raw}
}
func projectAction(t *testing.T, e hubProjectExecutor, ctx context.Context, call operatortool.Call) chatpkg.Action {
	t.Helper()
	result, err := e.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var action chatpkg.Action
	if err := json.Unmarshal(result.Content, &action); err != nil {
		t.Fatal(err)
	}
	return action
}

func TestBoundedProjectPolicyHistory(t *testing.T) {
	t.Parallel()
	descriptor := hubTestPolicy()
	full := projectPolicyRead{
		Approval:         policy.Approval{Policy: descriptor, ApprovedBy: "administrator", ApprovedAt: "2026-10-06T21:12:49Z", HistoryNext: "10"},
		Policy:           &descriptor,
		ObservedPolicies: []policy.ObservedPolicy{{Policy: descriptor, RunnerID: "runner_example", ObservedAt: "2026-10-06T21:12:49Z"}},
	}
	for _, id := range []int64{30, 20, 10} {
		full.History = append(full.History, policy.WorkflowApply{ID: id, AppliedBy: strings.Repeat("承認者", 100), PreviousDefinition: &descriptor, Definition: &descriptor})
	}
	compact := full
	compact.History = append([]policy.WorkflowApply(nil), full.History...)
	for i := range compact.History {
		compact.History[i].PreviousDefinition, compact.History[i].Definition = nil, nil
	}
	prefix := compact
	prefix.History, prefix.HistoryNext = compact.History[:2], "20"
	raw, err := json.Marshal(prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		budget int
		count  int
		next   string
		fails  bool
	}{
		{"exact bytes including cursor", len(raw), 2, "20", false},
		{"one byte less", len(raw) - 1, 1, "30", false},
		{"row cursor retained", operatortool.MaxResultBytes, 3, "10", false},
		{"no entry fits", 1, 0, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := boundedProjectPolicyHistory(full, test.budget)
			if test.fails {
				if !errors.Is(err, hubapi.ErrProjectServiceUnavailable) {
					t.Fatalf("unpageable history=%v", err)
				}
				return
			}
			if err != nil || len(page.History) != test.count || page.HistoryNext != test.next || !reflect.DeepEqual(page.Policy, full.Policy) || !reflect.DeepEqual(page.ObservedPolicies, full.ObservedPolicies) || page.ApprovedBy != full.ApprovedBy || page.ApprovedAt != full.ApprovedAt {
				t.Fatalf("bounded policy lost descriptor, provenance or cursor: %v", err)
			}
			encoded, err := json.Marshal(page)
			if err != nil || len(encoded) > test.budget || !reflect.DeepEqual(page.History, compact.History[:test.count]) {
				t.Fatalf("bounded policy exceeds bytes or changes history: bytes=%d, %v", len(encoded), err)
			}
			for _, entry := range full.History {
				if entry.Definition == nil || entry.PreviousDefinition == nil {
					t.Fatal("MCP projection mutated immutable REST history")
				}
			}
		})
	}
}

func TestHostedProjectCutoverReceiptReads(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			objects := &memoryRunDetails{}
			configure := func(cfg *Config) { cfg.RunDetails = objects }
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			if deployment == "shared" {
				shared = newHostedSharedFixture(t, configure)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t, configure)
			}
			owner := f.user(t, "cutover-owner", "owner", "cutover-owner@example.test", "write", "")
			viewer := f.user(t, "cutover-viewer", "member", "cutover-viewer@example.test", "read", "")
			captured := make(chan context.Context, 1)
			f.service.echo.GET("/cutover-read-test", func(c echo.Context) error { captured <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			connect := func(user hostedSecurityUser, id string) context.Context {
				t.Helper()
				if deployment == "shared" {
					requireNativeStatus(t, shared.serve(t, hostedSharedRequest{user: &user, method: http.MethodGet, target: "/organizations/org_security/cutover-read-test"}), http.StatusOK)
				} else {
					requireNativeStatus(t, f.request(t, user, http.MethodGet, "/cutover-read-test", nil), http.StatusOK)
				}
				ctx := operatortool.BindConnection(<-captured, id, "cutover receipt fixture")
				if err := (hubProjectExecutor{f.service}).OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				return ctx
			}
			ctx, deniedCtx := connect(owner, "cutover-owner"), connect(viewer, "cutover-viewer")
			repository := f.service.database.postgres
			organization := tracker.OrganizationID("org_security")
			create := func(id tracker.ProjectID) {
				t.Helper()
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Slug: strings.ReplaceAll(string(id), "_", "-"), Profile: "github_compatible"}, States: nativeFixtureStates()})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				projectFixture := f
				projectFixture.project = id
				projectFixture.grant(t, owner, true, false)
			}
			call := func(id tracker.ProjectID) operatortool.Call {
				return operatortool.Call{Name: "get_cutover_receipt", Arguments: json.RawMessage(`{"project_id":"` + string(id) + `"}`)}
			}
			for _, id := range []tracker.ProjectID{"prj_cutover_first", "prj_cutover_second"} {
				create(id)
				for _, transport := range []string{"stdio", "http"} {
					t.Run(string(id)+"/missing/"+transport, func(t *testing.T) {
						reply := hostedContextProtocol(t, f.service, ctx, transport)("tools/call", "get_cutover_receipt", map[string]any{"project_id": string(id)})
						var result struct {
							IsError bool                      `json:"isError"`
							Failure operatortool.RequestError `json:"structuredContent"`
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						}
						if err := json.Unmarshal(reply.Result, &result); err != nil {
							t.Fatal(err)
						}
						var content operatortool.RequestError
						if !result.IsError || result.Failure.Code != "not_found" || result.Failure.Message != "No cutover receipt exists for this project" || len(result.Content) != 1 || json.Unmarshal([]byte(result.Content[0].Text), &content) != nil || content != result.Failure {
							t.Fatalf("missing cutover receipt=%s", reply.Result)
						}
					})
				}
				checkpoint := artifact.Digest([]byte("stored-cutover-" + string(id)))
				expected := CutoverReceipt{Checkpoint: checkpoint, Applied: true, Issues: 2, HistoryLimitations: []string{"Example retained limitation"}}
				raw, err := json.Marshal(expected)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteCutover(t.Context(), organization, corestore.Serving, func(w postgres.CutoverWriter) error {
					project, err := w.ProjectRecord(t.Context(), id)
					if err != nil {
						return err
					}
					_, err = w.CommitProjectCutover(t.Context(), postgres.CutoverCommit{ExpectedContentRevision: project.Core.ContentSettingsRevision, Receipt: pgquery.StoreCutoverReceiptParams{OrganizationID: organization, ProjectID: id, Checkpoint: checkpoint, SourceProfile: "github_compatible", ExpectedRevision: project.Settings.Revision, ReceiptObjectKey: ref.Key, ReceiptSha256: ref.SHA256, ReceiptBytes: ref.CompressedSize, ReceiptContentBytes: ref.UncompressedSize, ActorID: owner.identity.Subject, CreatedAt: f.service.config.Clock().UTC()}})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				result, err := (hubProjectExecutor{f.service}).Execute(ctx, call(id))
				if err != nil || !strings.Contains(string(result.Content), `"observed_at"`) || !strings.Contains(string(result.Content), `"checkpoint":"`+checkpoint+`"`) || !strings.Contains(string(result.Content), `"applied":true`) {
					t.Fatalf("stored cutover receipt=%s error=%v", result.Content, err)
				}
				var page struct {
					Data CutoverReceipt `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &page); err != nil || !reflect.DeepEqual(page.Data, expected) {
					t.Fatalf("stored cutover payload=%+v error=%v", page.Data, err)
				}
				target := "/api/v2/organizations/org_security/projects/" + string(id) + "/integration/cutover"
				var response *httptest.ResponseRecorder
				if deployment == "shared" {
					response = shared.serve(t, hostedSharedRequest{user: &owner, method: http.MethodGet, target: target})
				} else {
					response = f.request(t, owner, http.MethodGet, target, nil)
				}
				requireNativeStatus(t, response, http.StatusOK)
				var got CutoverReceipt
				decodeHubResponse(t, response, &got)
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("HTTP cutover receipt=%+v want=%+v", got, expected)
				}
				if _, err := (hubProjectExecutor{f.service}).Execute(deniedCtx, call(id)); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("ungranted cutover receipt read=%v", err)
				}
				scope, err := (hubProjectExecutor{f.service}).projectScope(ctx)
				if err != nil {
					t.Fatal(err)
				}
				scope.Project = id
				objects.mu.Lock()
				retained := append([]byte(nil), objects.data[ref.Key]...)
				delete(objects.data, ref.Key)
				objects.mu.Unlock()
				_, missingErr := f.service.readCutoverReceipt(t.Context(), scope)
				objects.mu.Lock()
				objects.data[ref.Key] = retained
				objects.mu.Unlock()
				if !errors.Is(missingErr, artifact.ErrIntegrity) {
					t.Fatalf("missing receipt object error=%v", missingErr)
				}
				mismatch := expected
				mismatch.Checkpoint = strings.Repeat("0", 64)
				mismatched, err := json.Marshal(mismatch)
				if err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				objects.data[ref.Key] = mismatched
				objects.mu.Unlock()
				_, mismatchErr := f.service.readCutoverReceipt(t.Context(), scope)
				objects.mu.Lock()
				objects.data[ref.Key] = retained
				objects.mu.Unlock()
				if !errors.Is(mismatchErr, artifact.ErrIntegrity) {
					t.Fatalf("mismatched checkpoint receipt error=%v", mismatchErr)
				}
			}
		})
	}
}

func TestHostedProjectTools(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			now := time.Now().UTC().Truncate(time.Microsecond)
			configure := func(cfg *Config) {
				cfg.Workspace = &WorkspaceConfig{Enabled: true}
				cfg.Clock = func() time.Time { return now }
			}
			if deployment == "shared" {
				shared = newHostedSharedFixture(t, configure)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t, configure)
			}
			owner := f.user(t, "owner-tools", "owner", "owner-tools@example.test", "write", "")
			f.grant(t, owner, true, true)
			viewer := f.user(t, "quota-viewer", "member", "quota-viewer@example.test", "read", "")
			captureds := make(chan context.Context, 1)
			f.service.echo.GET("/operator-project-test", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			connectAs := func(user hostedSecurityUser, id string) context.Context {
				t.Helper()
				if deployment == "shared" {
					r := shared.serve(t, hostedSharedRequest{user: &user, method: http.MethodGet, target: "/organizations/org_security/operator-project-test"})
					requireNativeStatus(t, r, http.StatusOK)
				} else {
					requireNativeStatus(t, f.request(t, user, http.MethodGet, "/operator-project-test", nil), http.StatusOK)
				}
				ctx := operatortool.BindConnection(<-captureds, id, "project fixture")
				if err := (hubProjectExecutor{f.service}).OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				return ctx
			}
			connect := func(id string) context.Context { return connectAs(owner, id) }
			ctx := connect("project-tools")
			e := hubProjectExecutor{f.service}
			id := string(f.project)
			if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", "org_security", f.project); err != nil {
				t.Fatal(err)
			}
			read := func(name string) operatortool.Call {
				return operatortool.Call{Name: name, Arguments: json.RawMessage(`{"project_id":"` + id + `"}`)}
			}
			for _, transport := range []string{"stdio", "http"} {
				t.Run("missing reads/"+transport, func(t *testing.T) {
					call := hostedContextProtocol(t, f.service, ctx, transport)
					for _, missing := range []struct{ name, code, message string }{
						{operatortool.GetChangeReviewPolicy, "not_found", "No approved Change review policy exists for this project"},
					} {
						reply := call("tools/call", missing.name, map[string]any{"project_id": id})
						var result struct {
							IsError bool                      `json:"isError"`
							Failure operatortool.RequestError `json:"structuredContent"`
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						}
						if err := json.Unmarshal(reply.Result, &result); err != nil {
							t.Fatal(err)
						}
						var content operatortool.RequestError
						if !result.IsError || result.Failure.Code != missing.code || result.Failure.Message != missing.message || len(result.Content) != 1 || json.Unmarshal([]byte(result.Content[0].Text), &content) != nil || content != result.Failure {
							t.Fatalf("missing %s=%s", missing.name, reply.Result)
						}
					}
				})
			}
			for _, name := range []string{"get_project_policy", operatortool.LocalProjectConfiguration, "list_projects", "get_native_project", "get_onboarding", "get_project_integration", "project_secret_metadata", "project_setup"} {
				result, err := e.Execute(ctx, read(name))
				if err != nil || !strings.Contains(string(result.Content), `"observed_at"`) {
					t.Fatalf("%s=%s %v", name, result.Content, err)
				}
				if name == operatortool.LocalProjectConfiguration && (!strings.Contains(string(result.Content), "not installed or is stopped") || strings.Contains(string(result.Content), `"applied":true`)) {
					t.Fatalf("Cloud fabricated local application: %s", result.Content)
				}

			}
			ordinary := projectCall(t, "save_onboarding", id, "progress", operatortool.OnboardingInput{Progress: onboarding.Progress{Repository: "existing"}})
			scope, err := e.projectScope(ctx)
			if err != nil {
				t.Fatal(err)
			}
			scope.Project = f.project
			before, err := f.service.readOnboardingProgressRecord(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if raw, err := e.command(ctx, ordinary, projectCommandPreview); err != nil || raw != nil {
				t.Fatalf("onboarding preview=%s err=%v", raw, err)
			}
			if after, err := f.service.readOnboardingProgressRecord(t.Context(), scope); err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("preview changed onboarding: before=%+v after=%+v err=%v", before, after, err)
			}
			a := projectAction(t, e, ctx, ordinary)
			if a.Status != chatpkg.ActionSucceeded {
				t.Fatalf("ordinary=%+v", a)
			}
			saved, err := f.service.readOnboardingProgressRecord(t.Context(), scope)
			if err != nil || saved.Repository != "existing" || saved.Revision != before.Revision+1 || saved.UpdatedAt == "" {
				t.Fatalf("operator save missing from typed onboarding owner: progress=%+v err=%v", saved, err)
			}
			reconnect := connect("project-reconnect")
			if replay := projectAction(t, e, reconnect, ordinary); replay.Status != chatpkg.ActionSucceeded || replay.Result != a.Result {
				t.Fatalf("replay=%+v", replay)
			}
			if after, err := f.service.readOnboardingProgressRecord(t.Context(), scope); err != nil || !reflect.DeepEqual(after, saved) {
				t.Fatalf("replay changed onboarding: saved=%+v after=%+v err=%v", saved, after, err)
			}
			changed := projectCall(t, "save_onboarding", id, "progress", operatortool.OnboardingInput{Progress: onboarding.Progress{Repository: "generate"}})
			if _, err := e.Execute(connect("changed-retry"), changed); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("changed retry=%v", err)
			}
			stale := projectCall(t, "save_onboarding", id, "stale-progress", operatortool.OnboardingInput{Progress: onboarding.Progress{Repository: "generate"}})
			if _, err := e.Execute(ctx, stale); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale=%v", err)
			}
			t.Run("onboarding requires collaboration", func(t *testing.T) {
				var original billingstore.Plan
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					assignment, err := w.BillingAssignment(t.Context())
					if err != nil {
						return err
					}
					plan := assignment.Base
					if assignment.Subscription != nil && assignment.ExpiresAt != nil && assignment.ExpiresAt.After(f.service.config.Clock()) {
						plan = *assignment.Subscription
					}
					original, err = w.BillingPlan(t.Context(), plan)
					if err != nil {
						return err
					}
					withoutCollaboration := original
					withoutCollaboration.Features = slices.DeleteFunc(slices.Clone(original.Features), func(feature string) bool { return feature == "collaboration" })
					return w.ReplaceBillingPlan(t.Context(), withoutCollaboration)
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
					defer cancel()
					if err := f.service.config.TenantRepository.WriteTenant(cleanup, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.ReplaceBillingPlan(cleanup, original)
					}); err != nil {
						t.Error(err)
					}
				})
				if raw, err := e.command(ctx, ordinary, projectCommandReplay); err != nil || raw == nil {
					t.Fatalf("existing onboarding receipt unavailable after feature removal: replay=%s err=%v", raw, err)
				}
				requestID := "onboarding-without-collaboration"
				denied := projectCall(t, "save_onboarding", id, requestID, operatortool.OnboardingInput{Progress: onboarding.Progress{Revision: saved.Revision, Repository: "generate"}})
				var limited *hostedplans.LimitError
				if raw, err := e.command(ctx, denied, projectCommandExecute); !errors.As(err, &limited) || limited.Resource != "collaboration" || raw != nil {
					t.Fatalf("unentitled onboarding save=%s err=%v", raw, err)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					current, err := r.ProjectOnboardingProgress(t.Context(), scope.Project)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(current, saved) {
						t.Errorf("unentitled onboarding save changed progress: saved=%+v current=%+v", saved, current)
					}
					if _, err := r.ReplayReceipt(t.Context(), scope.Credential.ID, projectOperationID(scope, "PUT", "/onboarding"), requestID); !errors.Is(err, corestore.ErrNotFound) {
						t.Errorf("unentitled onboarding save wrote a receipt: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			policyScope := "org_security/" + id
			previous, err := f.service.approveTestTenantPolicy(t.Context(), policyScope, operatortool.ConnectionIdentity(ctx).PrincipalID, policy.Change{Policy: hubTestPolicy()})
			if err != nil {
				t.Fatal(err)
			}
			sources := workflowconfig.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Review the material policy.\nImplement the assigned issue.\n", 100)),
				AgentsPath: "AGENTS.md", HasAgents: true, Agents: []byte(strings.Repeat("Preserve policy authority.\n", 100)),
			}
			workflow, err := workflowconfig.ParseProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			observedRevision, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), "org_security", candidate)
			if err != nil {
				t.Fatal(err)
			}
			observedRevision.ProjectID = f.project
			seedHostedContextRunner(t, f, "observing-runner", "test", f.service.config.Clock(), false)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: "tok_observing-runner", ProjectID: f.project}); err != nil {
					return err
				}
				_, err := w.RecordProjectPolicyObservation(t.Context(), policystore.Observation{OrganizationID: "org_security", ProjectID: f.project, RunnerID: "observing-runner", PolicyID: candidate.ID, ObservedAt: f.service.config.Clock(), Revision: observedRevision})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			observed, err := e.Execute(ctx, read("get_onboarding"))
			if err != nil {
				t.Fatal(err)
			}
			var onboardingRead struct {
				Data onboarding.Project `json:"data"`
			}
			if err := json.Unmarshal(observed.Content, &onboardingRead); err != nil {
				t.Fatal(err)
			}
			setup := onboardingRead.Data
			if !reflect.DeepEqual([]string{setup.Steps[0].Name, setup.Steps[1].Name, setup.Steps[2].Name, setup.Steps[3].Name}, []string{"Runner", "Repository", "Checkout", "Run files"}) || setup.Policy == nil || setup.Policy.Policy.ID != previous.Policy.ID || len(setup.ObservedPolicies) != 1 || !reflect.DeepEqual(setup.ObservedPolicies[0].Policy, candidate) {
				t.Fatalf("observed candidate = %+v", setup)
			}
			approve := projectCall(t, "approve_project_policy", id, "approve", operatortool.PolicyApprovalInput{ExpectedID: setup.Policy.Policy.ID, ObservedPolicyID: setup.ObservedPolicies[0].Policy.ID})
			t.Run("hosted allowance refusal", func(t *testing.T) {
				viewerContext := connectAs(viewer, "quota-viewer")
				credential, err := (hubAdministration{f.service}).credential(ctx)
				if err != nil {
					t.Fatal(err)
				}
				created, err := f.service.createNativeIssueCommand(t.Context(), hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: credential}, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "quota-issue"}, Title: "Allowance fixture", State: "Todo", Body: "## Acceptance criteria\nPreserve allowance refusal.\n## Must not break\nKeep writes denied.\n## How we know it worked\nAssert no comment is stored."})
				if err != nil {
					t.Fatal(err)
				}
				var issue tracker.NativeIssue
				if err := json.Unmarshal(created, &issue); err != nil || issue.WorkItemID == "" {
					t.Fatalf("issue=%s err=%v", created, err)
				}
				usage, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), f.service.config.Clock()))
				if err != nil {
					t.Fatal(err)
				}
				comment := map[string]any{"project_id": id, "request_id": "quota-comment", "identifier": issue.WorkItemID, "body": "Refused comment"}
				var approvalArguments map[string]any
				if err := json.Unmarshal(approve.Arguments, &approvalArguments); err != nil {
					t.Fatal(err)
				}
				for _, transport := range []string{"stdio", "http"} {
					t.Run(transport, func(t *testing.T) {
						approvalArguments["request_id"] = "quota-approve-" + transport
						call := hostedContextProtocol(t, f.service, ctx, transport)
						deniedCall := hostedContextProtocol(t, f.service, viewerContext, transport)
						for _, test := range []struct {
							name, resource string
							arguments      map[string]any
						}{
							{operatortool.AddComment, "collaboration_bytes", comment},
							{"approve_project_policy", "api_mutations", approvalArguments},
							{"create_workspace", "api_mutations", map[string]any{"project_id": id, "request_id": "quota-workspace-" + transport, "input": map[string]any{"work_item_id": issue.WorkItemID}}},
							{"create_project_action", "api_mutations", map[string]any{"project_id": id, "request_id": "quota-action-" + transport, "input": map[string]any{"name": "Refused action", "command": "true"}}},
						} {
							t.Run(test.name, func(t *testing.T) {
								consumption := usage.Allowances[test.resource] + 1
								if test.resource == "collaboration_bytes" {
									var delta int64
									if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
										used, err := w.CollaborationBytes(t.Context())
										if err != nil {
											return err
										}
										delta = consumption - used
										result, err := w.AdjustCollaborationBytes(t.Context(), delta, consumption)
										if err == nil && (!result.Allowed || result.Used != consumption) {
											return errors.New("collaboration quota fixture was not applied")
										}
										return err
									}); err != nil {
										t.Fatal(err)
									}
									t.Cleanup(func() {
										cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
										defer cancel()
										if err := f.service.config.TenantRepository.WriteTenant(cleanup, "org_security", corestore.Serving, func(w tenantstore.Writer) error {
											_, err := w.AdjustCollaborationBytes(cleanup, -delta, consumption)
											return err
										}); err != nil {
											t.Error(err)
										}
									})
								} else {
									window := usage.WindowEndsAt.Add(-time.Duration(f.service.database.plansService().Plans().WindowSeconds) * time.Second)
									admin := hostedPostgresAdmin(t, f.service)
									var before int64
									if err := admin.QueryRowContext(t.Context(), "SELECT COALESCE((SELECT amount FROM hosted_usage_windows WHERE organization_id=$1 AND window_start=$2 AND metric=$3),0)", "org_security", window, test.resource).Scan(&before); err != nil {
										t.Fatal(err)
									}
									if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_usage_windows(organization_id,window_start,metric,amount) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id,window_start,metric) DO UPDATE SET amount=excluded.amount", "org_security", window, test.resource, consumption); err != nil {
										t.Fatal(err)
									}
									delta := consumption - before
									t.Cleanup(func() {
										cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
										defer cancel()
										result, err := admin.ExecContext(cleanup, "UPDATE hosted_usage_windows SET amount=amount-$4 WHERE organization_id=$1 AND window_start=$2 AND metric=$3 AND amount>=$4", "org_security", window, test.resource, delta)
										if err != nil {
											t.Error(err)
										} else if count, err := result.RowsAffected(); err != nil || count != 1 {
											t.Errorf("usage fixture cleanup rows=%d error=%v", count, err)
										}
									})
								}
								reply := call("tools/call", test.name, test.arguments)
								var result struct {
									IsError bool `json:"isError"`
									Failure struct {
										Code        string `json:"code"`
										HTTPStatus  int    `json:"http_status"`
										Resource    string `json:"resource"`
										Allowance   int64  `json:"allowance"`
										Consumption int64  `json:"consumption"`
										Recovery    string `json:"recovery"`
									} `json:"structuredContent"`
								}
								if err := json.Unmarshal(reply.Result, &result); err != nil {
									t.Fatal(err)
								}
								if !result.IsError || result.Failure.Code != "allowance_exhausted" || result.Failure.HTTPStatus != http.StatusTooManyRequests || result.Failure.Resource != test.resource || result.Failure.Allowance != usage.Allowances[test.resource] || result.Failure.Consumption != consumption || !strings.Contains(result.Failure.Recovery, operatortool.HostedPlan) || !strings.Contains(result.Failure.Recovery, operatortool.BillingExport) {
									t.Fatalf("quota refusal=%s", reply.Result)
								}
								denied := deniedCall("tools/call", test.name, test.arguments)
								var denial struct {
									IsError bool `json:"isError"`
								}
								if json.Unmarshal(denied.Result, &denial) != nil || !denial.IsError || !strings.Contains(string(denied.Result), "access") || strings.Contains(string(denied.Result), "allowance") || strings.Contains(string(denied.Result), "consumption") {
									t.Fatalf("unauthorized caller received usage or succeeded: %s", denied.Result)
								}
							})
						}
					})
				}
				var comments int
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", "org_security", f.project, issue.WorkItemID).Scan(&comments); err != nil || comments != 0 {
					t.Fatalf("refused comment effects=%d err=%v", comments, err)
				}
				for _, table := range []string{"workspace_sessions", "project_actions"} {
					var effects int
					if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE organization_id=$1 AND project_id=$2", "org_security", f.project).Scan(&effects); err != nil || effects != 0 {
						t.Fatalf("refused %s effects=%d err=%v", table, effects, err)
					}
				}
				var receipts int
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key IN ('quota-comment','quota-approve-stdio','quota-approve-http')", "org_security").Scan(&receipts); err != nil || receipts != 0 {
					t.Fatalf("refused writes persisted receipts=%d err=%v", receipts, err)
				}
				unchanged, err := f.service.readTenantProjectPolicy(t.Context(), "org_security", f.project)
				if err != nil || !reflect.DeepEqual(unchanged.Policy, previous.Policy) {
					t.Fatalf("refused approval changed policy=%+v err=%v", unchanged, err)
				}
			})
			a = projectAction(t, e, ctx, approve)
			if a.Status != chatpkg.ActionSucceeded {
				t.Fatalf("policy=%+v", a)
			}
			var attribution policy.Approval
			if json.Unmarshal([]byte(a.Result), &attribution) != nil || attribution.ApprovedBy != operatortool.ConnectionIdentity(ctx).PrincipalID {
				t.Fatalf("project command lost originating principal: approval=%+v requester=%s", attribution, operatortool.ConnectionIdentity(ctx).PrincipalID)
			}
			storedPolicy, err := e.Execute(ctx, read("get_project_policy"))
			if err != nil {
				t.Fatal(err)
			}
			var policyRead struct {
				Data policy.Approval `json:"data"`
			}
			if err := json.Unmarshal(storedPolicy.Content, &policyRead); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(attribution.Policy, candidate) || !reflect.DeepEqual(policyRead.Data.Policy, candidate) {
				t.Fatal("MCP approval changed the observed descriptor")
			}
			if len(policyRead.Data.History) != 1 || policyRead.Data.History[0].PreviousDefinitionDigest != previous.Policy.SourceDigest || policyRead.Data.History[0].DefinitionDigest != candidate.SourceDigest || policyRead.Data.History[0].AppliedBy != attribution.ApprovedBy || policyRead.Data.History[0].AppliedAt == "" {
				t.Fatalf("MCP lost workflow apply history: %+v", policyRead.Data.History)
			}
			integrationRecord, err := f.service.readProjectRecord(t.Context(), hubscope.NativeScope{Organization: "org_security", Project: f.project})
			integration := projectIntegrationRecord(integrationRecord)
			wantStates := append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Backlog", "Todo", "Cancelled"}}}, candidate.Workflow.States...)
			if err != nil || integration.Authority["workflow"] != "repository" || integration.WorkflowSource != candidate.Workflow.Source || integration.WorkflowSourceRevision != candidate.SourceRevision || !reflect.DeepEqual(integration.States, wantStates) {
				t.Fatalf("approved workflow = %+v, %v", integration, err)
			}
			if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read("get_change_review_policy")); err != nil {
				t.Fatal(err)
			}
			t.Run("same policy reapproval", func(t *testing.T) {
				committed, err := e.command(ctx, approve, projectCommandExecute)
				if err != nil || committed == nil {
					t.Fatalf("committed approval replay=%s err=%v", committed, err)
				}
				unchanged := projectCall(t, "approve_project_policy", id, "reapprove-unchanged", operatortool.PolicyApprovalInput{ExpectedID: candidate.ID, ObservedPolicyID: candidate.ID})
				if raw, err := e.command(ctx, unchanged, projectCommandExecute); err != nil || raw == nil {
					t.Fatalf("unchanged reapproval=%s err=%v", raw, err)
				}
				admin := hostedPostgresAdmin(t, f.service)
				if _, err := admin.ExecContext(t.Context(), "UPDATE projects SET workflow_source=$3 WHERE organization_id=$1 AND id=$2", "org_security", f.project, "RENAMED-WORKFLOW.md"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.DefaultCleanupBudget)
					defer cancel()
					if _, err := admin.ExecContext(cleanup, "UPDATE projects SET workflow_source=$3 WHERE organization_id=$1 AND id=$2", "org_security", f.project, candidate.Workflow.Source); err != nil {
						t.Error(err)
					}
				})
				scope := hubscope.NativeScope{Organization: "org_security", Project: f.project}
				policyBefore, err := f.service.readTenantProjectPolicy(t.Context(), "org_security", f.project)
				if err != nil {
					t.Fatal(err)
				}
				recordBefore, err := f.service.readProjectRecord(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				reviewBefore, err := f.service.changeReviewPolicyRecord(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				for _, test := range []struct {
					name  string
					input operatortool.PolicyApprovalInput
				}{
					{"observed", operatortool.PolicyApprovalInput{ExpectedID: candidate.ID, ObservedPolicyID: candidate.ID}},
					{"descriptor", operatortool.PolicyApprovalInput{ExpectedID: candidate.ID, Policy: candidate}},
				} {
					key := "reapprove-changed-" + test.name
					var refusal *hubapi.NativeError
					if raw, err := e.command(ctx, projectCall(t, "approve_project_policy", id, key, test.input), projectCommandExecute); !errors.As(err, &refusal) || refusal.Code != "policy_mismatch" || refusal.Status != http.StatusConflict || raw != nil {
						t.Fatalf("%s reapproval after workflow change=%s err=%v", test.name, raw, err)
					}
					policyAfter, err := f.service.readTenantProjectPolicy(t.Context(), "org_security", f.project)
					if err != nil || !reflect.DeepEqual(policyAfter, policyBefore) {
						t.Fatalf("%s refusal changed policy=%+v err=%v", test.name, policyAfter, err)
					}
					recordAfter, err := f.service.readProjectRecord(t.Context(), scope)
					if err != nil || !reflect.DeepEqual(projectIntegrationRecord(recordAfter), projectIntegrationRecord(recordBefore)) || recordAfter.Core.WorkflowSource != "RENAMED-WORKFLOW.md" {
						t.Fatalf("%s refusal changed workflow=%+v err=%v", test.name, projectIntegrationRecord(recordAfter), err)
					}
					reviewAfter, err := f.service.changeReviewPolicyRecord(t.Context(), scope)
					if err != nil || !reflect.DeepEqual(reviewAfter, reviewBefore) {
						t.Fatalf("%s refusal changed review defaults=%+v err=%v", test.name, reviewAfter, err)
					}
					var receipts int
					if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$2", "org_security", key).Scan(&receipts); err != nil || receipts != 0 {
						t.Fatalf("%s refusal receipts=%d err=%v", test.name, receipts, err)
					}
				}
				if replay, err := e.command(ctx, approve, projectCommandExecute); err != nil || string(replay) != string(committed) {
					t.Fatalf("committed approval replay after workflow change=%s want %s err=%v", replay, committed, err)
				}
			})
			sources.Workflow = append(sources.Workflow, []byte("Changed policy.\n")...)
			sources.ConfigPath = "stale-detent.yaml"
			workflow, err = workflowconfig.ParseProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			if updated.ID == candidate.ID {
				t.Fatal("stale approval fixture must change the authored policy")
			}
			stalePolicy := projectCall(t, "approve_project_policy", id, "stale-policy", operatortool.PolicyApprovalInput{ExpectedID: previous.Policy.ID, Policy: updated})
			if _, err := e.Execute(ctx, stalePolicy); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale expected policy = %v", err)
			}
			unchanged, err := f.service.readTenantProjectPolicy(t.Context(), "org_security", f.project)
			if err != nil || !reflect.DeepEqual(unchanged.Policy, candidate) {
				t.Fatalf("stale approval changed policy = %+v, %v", unchanged, err)
			}
			unchangedRecord, err := f.service.readProjectRecord(t.Context(), hubscope.NativeScope{Organization: "org_security", Project: f.project})
			unchangedIntegration := projectIntegrationRecord(unchangedRecord)
			if err != nil || !reflect.DeepEqual(unchangedIntegration, integration) {
				t.Fatalf("stale approval changed workflow = %+v, %v", unchangedIntegration, err)
			}
			rules, err := f.service.changeReviewPolicyRecord(t.Context(), hubscope.NativeScope{Organization: "org_security", Project: f.project})
			if err != nil {
				t.Fatal(err)
			}
			changeExecutor := hostedOperatorExecutor{f.service}
			reviewInput := operatortool.ChangeArguments{ProjectID: id, RequestID: "foreign-check", ExpectedPolicyID: rules.ID, Policy: &rules}
			reviewInput.Policy.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "ci", PrincipalID: "foreign-principal", Source: "independent"}}
			raw, _ := json.Marshal(reviewInput)
			if _, err := changeExecutor.Execute(ctx, operatortool.Call{Name: operatortool.ApproveChangeReviewPolicy, Arguments: raw}); err == nil {
				t.Fatal("unowned CI principal accepted")
			}
			reviewInput.RequestID = "review-policy"
			reviewInput.Policy.RequiredChecks = []tracker.ChangeCheckSpec{}
			raw, _ = json.Marshal(reviewInput)
			if _, err := changeExecutor.Execute(ctx, operatortool.Call{Name: operatortool.ApproveChangeReviewPolicy, Arguments: raw}); err != nil {
				t.Fatalf("review policy=%v", err)
			}
			create := projectCall(t, "create_hosted_project", "", "create-hosted", operatortool.HostedProjectCreateInput{Name: "new MCP project", GrantAccess: true})
			created := projectAction(t, e, ctx, create)
			if created.Status != chatpkg.ActionSucceeded || !strings.Contains(created.Result, "new MCP project") {
				t.Fatalf("create=%+v", created)
			}
			if replay := projectAction(t, e, connect("create-reconnect"), create); replay.Result != created.Result {
				t.Fatalf("create replay=%+v", replay)
			}
			var newProject tracker.NativeProject
			if err := json.Unmarshal([]byte(created.Result), &newProject); err != nil {
				t.Fatal(err)
			}
			repositoryPolicy := hubTestPolicy()
			repositoryPolicy.Workflow = &policy.Workflow{Source: "detent.yaml", States: HostedProjectStates()}
			repositoryPolicy = repositoryPolicy.WithID()
			approveCall := projectCall(t, "approve_project_policy", string(newProject.ID), "repository-workflow", operatortool.PolicyApprovalInput{Policy: repositoryPolicy})
			if result := projectAction(t, e, ctx, approveCall); result.Status != chatpkg.ActionSucceeded {
				t.Fatalf("repository approval through MCP = %+v", result)
			}
			markdown := "---\ntracker:\n  kind: hub_native\n---\nWork\n"
			states := HostedProjectStates()
			for _, input := range []operatortool.IntegrationInput{
				{ExpectedRevision: 2, Intake: "disabled", Projection: "disabled", States: &states},
				{ExpectedRevision: 2, Intake: "disabled", Projection: "disabled", WorkflowMarkdown: &markdown},
			} {
				call := projectCall(t, "update_project_integration", string(newProject.ID), "repository-override-"+strconv.FormatBool(input.WorkflowMarkdown != nil), input)
				result, err := e.Execute(ctx, call)
				if err == nil && !strings.Contains(string(result.Content), "controlled by the repository") {
					t.Fatalf("MCP accepted repository workflow override: %s", result.Content)
				}
				if err != nil {
					var refusal *operatortool.RequestError
					if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "controlled by the repository") {
						t.Fatalf("MCP override refusal = %v", err)
					}
				}
				storedRecord, readErr := f.service.readProjectRecord(t.Context(), hubscope.NativeScope{Organization: "org_security", Project: newProject.ID})
				stored := projectIntegrationRecord(storedRecord)
				if readErr != nil || stored.Authority["workflow"] != "repository" || !reflect.DeepEqual(stored.States, states) || stored.Revision != 2 {
					t.Fatalf("MCP override changed repository workflow: %#v, %v", stored, readErr)
				}
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), owner.identity.Subject)
				member.Role = "member"
				return errors.Join(err, w.UpdateAccessMember(t.Context(), member), w.DeleteAccessProjectGrant(t.Context(), owner.identity.Subject, newProject.ID))
			}); err != nil {
				t.Fatal(err)
			}
			for _, retryCtx := range []context.Context{ctx, connect("create-revoked")} {
				if _, err := e.Execute(retryCtx, create); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("created resource revoked replay=%v", err)
				}
			}
			if _, err := e.Execute(ctx, operatortool.Call{Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + created.ID + `"}`)}); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("created resource revoked action result=%v", err)
			}
			revoke := projectCall(t, "revoke_project_policy", id, "revoke", operatortool.PolicyRevokeInput{ExpectedID: candidate.ID})
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), owner.identity.Subject)
				if err != nil {
					return err
				}
				member.Role, member.UpdatedAt = "viewer", f.service.config.Clock()
				return w.UpdateAccessMember(t.Context(), member)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Execute(ctx, revoke); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("downgraded revoke=%v", err)
			}
			if _, err := e.Execute(ctx, ordinary); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("downgraded replay=%v", err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				grants, err := w.AccessProjectGrants(t.Context(), owner.identity.Subject, "", 200)
				if err != nil {
					return err
				}
				if len(grants) >= 200 {
					return errors.New("project tool fixture grants exceed one bounded page")
				}
				for _, grant := range grants {
					if err := w.DeleteAccessProjectGrant(t.Context(), owner.identity.Subject, grant.ProjectID); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Execute(ctx, read("get_native_project")); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked direct read=%v", err)
			}
			for _, name := range []string{"get_cutover_receipt", operatortool.GetChangeReviewPolicy, "get_project_policy"} {
				if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, read(name)); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revoked %s=%v", name, err)
				}
			}
		})
	}
}

// Import page retries must reuse the durable receipt and never expose transport errors.
func TestProjectImportToolReceipts(t *testing.T) {
	t.Parallel()
	backend := &importFixtureBackend{}
	f := newIntegrationFixture(t, backend)
	captureds := make(chan context.Context, 1)
	// The resolver derives the organization from the registered native entry path.
	f.service.echo.GET("/api/v2/organizations/:organization/project-import-test", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/project-import-test"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
	captured := <-captureds
	e := hubProjectExecutor{f.service}
	connect := func(id string) context.Context {
		ctx := operatortool.BindConnection(captured, id, "import fixture")
		if err := e.OpenConnection(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	ctx := connect("import")
	create := projectCall(t, "create_native_project", "", "create-native", operatortool.ProjectCreateInput{Name: "tool project", States: nativeFixtureStates()})
	created := projectAction(t, e, ctx, create)
	if created.Status != chatpkg.ActionSucceeded {
		t.Fatalf("create native=%+v", created)
	}
	if replay := projectAction(t, e, connect("create-native-reconnect"), create); replay.Result != created.Result {
		t.Fatalf("native create replay=%+v", replay)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	legacy, err := e.Execute(ctx, operatortool.Call{Name: "get_project_policy", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","repository_policy":true}`)})
	if err != nil || !strings.Contains(string(legacy.Content), hubTestPolicy().ID) {
		t.Fatalf("repository policy=%s %v", legacy.Content, err)
	}

	start := projectCall(t, "start_git_hub_import", string(f.project.ID), "start-tool", operatortool.ImportStartInput{IssueNumber: 1})
	a := projectAction(t, e, ctx, start)
	if a.Status != chatpkg.ActionSucceeded {
		t.Fatalf("ordinary import=%+v", a)
	}
	var job GitHubImport
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	advance := func(ctx context.Context, key string) chatpkg.Action {
		return projectAction(t, e, ctx, projectCall(t, "advance_git_hub_import", string(f.project.ID), key, operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision}))
	}
	a = advance(ctx, "page-one")
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	if job.Stage != "comments" {
		t.Fatalf("stage=%s", job.Stage)
	}
	call := projectCall(t, "advance_git_hub_import", string(f.project.ID), "page-two", operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision})
	a = projectAction(t, e, ctx, call)
	requests := len(backend.requests)
	if replay := projectAction(t, e, connect("import-reconnected"), call); replay.Result != a.Result || len(backend.requests) != requests {
		t.Fatalf("replay repeated import fetch: %+v", replay)
	}
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	backend.failPage = true
	a = advance(ctx, "failed-page")
	if strings.Contains(a.Result, "GitHub unavailable") || !strings.Contains(a.Result, "Import transport is unavailable") {
		t.Fatalf("raw error escaped: %s", a.Result)
	}
	read := operatortool.Call{Name: "get_git_hub_import", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","import_id":"` + job.ID + `"}`)}
	result, err := e.Execute(ctx, read)
	if err != nil || strings.Contains(string(result.Content), "GitHub unavailable") {
		t.Fatalf("read redaction=%s %v", result.Content, err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-import-project")
	read.Arguments = json.RawMessage(`{"project_id":"` + string(other.project.ID) + `","import_id":"` + job.ID + `"}`)
	if _, err := e.Execute(ctx, read); err == nil {
		t.Fatal("foreign project import disclosed")
	}
}

func TestBoundedProjectPolicyObservations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		count       int
		promptBytes int
		withHistory bool
	}{
		{name: "single oversized descriptor", count: 1, promptBytes: operatortool.MaxResultBytes},
		{name: "several descriptors", count: 4, promptBytes: operatortool.MaxResultBytes / 3},
		{name: "too many summaries", count: 1000, promptBytes: 1},
		{name: "history with oversized descriptor", count: 1, promptBytes: operatortool.MaxResultBytes, withHistory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			full := projectPolicyRead{}
			if test.withHistory {
				descriptor := hubTestPolicy()
				full.Policy = &descriptor
				full.ApprovedBy = "administrator"
				full.History = []policy.WorkflowApply{{ID: 42, Definition: &descriptor}}
			}
			for i := range test.count {
				descriptor := hubTestPolicy()
				descriptor.Configuration = &policy.Configuration{Prompt: strings.Repeat("x", test.promptBytes), Behavior: json.RawMessage(`{}`)}
				descriptor.SourceDigest = policy.Digest([]byte(strconv.Itoa(i)))
				descriptor = descriptor.WithID()
				full.ObservedPolicies = append(full.ObservedPolicies, policy.ObservedPolicy{Policy: descriptor, RunnerID: "runner_example"})
			}
			before, err := json.Marshal(full)
			if err != nil {
				t.Fatal(err)
			}
			page, err := boundedProjectPolicyHistory(full, operatortool.MaxResultBytes-1024)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(page)
			if err != nil || len(raw) > operatortool.MaxResultBytes-1024 {
				t.Fatalf("observations exceed response budget: bytes=%d, %v", len(raw), err)
			}
			var view struct {
				DetailsOmitted bool `json:"observed_policy_details_omitted"`
				Omitted        int  `json:"observed_policies_omitted"`
			}
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(page.Policy, full.Policy) || len(page.History) != len(full.History) || len(page.ObservedPolicies) == 0 || !view.DetailsOmitted || len(page.ObservedPolicies)+view.Omitted != test.count {
				t.Fatalf("missing observation identity or omission metadata: %s", raw)
			}
			if test.count == 1000 && view.Omitted == 0 {
				t.Fatal("oversized summary list was not bounded")
			}
			if test.withHistory && (page.History[0].ID != 42 || page.History[0].Definition != nil || page.ApprovedBy != "administrator") {
				t.Fatal("observation compaction changed approval or history")
			}
			for i, observation := range page.ObservedPolicies {
				if observation.Policy.ID != full.ObservedPolicies[i].Policy.ID || observation.RunnerID != "runner_example" {
					t.Fatal("candidate identity or provenance changed")
				}
			}
			after, err := json.Marshal(full)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("MCP projection mutated REST observations")
			}
		})
	}
}
