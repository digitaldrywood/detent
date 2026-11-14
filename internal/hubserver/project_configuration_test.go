package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSelectedProjectPolicyRoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"legacy", "authored"} {
		t.Run(kind, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			current := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", current)
			selected := current
			selected.Profile = "runner"
			selected.Gates.HumanReview = true
			selected = selected.WithID()
			if kind == "authored" {
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
					ConfigPath: "detent.yaml", HasConfig: true,
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: make check-land\n  required_status_checks: []\n"),
					WorkflowPath: "WORKFLOW.md", Workflow: []byte("Implement the assigned issue.\n"),
				})
				if err != nil {
					t.Fatal(err)
				}
				selected, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
			}
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "hub_configuration", Registered: true, RuntimeRegistered: true, Source: "configured_committed_workflow", SelectedPolicy: &selected, EffectivePolicy: &current, ObservedAt: f.service.config.Clock()}
			seedHubConfiguration(t, f, r.binding.RunnerID, view)
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
			}
			post()

			reports, err := f.service.tenantProjectPolicyRead(t.Context(), f.project.OrganizationID, f.project.ID, policy.Approval{Policy: current}, 100, "")
			if err != nil || len(reports.ObservedPolicies) != 1 || reports.ObservedPolicies[0].Policy.ID != selected.ID {
				t.Fatalf("heartbeat selected policy reports=%+v %v", reports, err)
			}
			source := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("b", 40)}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(r.redemption.Credential), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.recordConfigurationPolicyObservation(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}, policy.Observation{Descriptor: selected, Source: source}); err != nil {
				t.Fatal(err)
			}
			post()
			contexts := make(chan context.Context, 1)
			f.service.echo.GET("/api/v2/organizations/:organization/policy-round-trip-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/policy-round-trip-test", testHubAdminToken, nil), http.StatusOK)
			ctx := operatortool.BindConnection(<-contexts, "policy-round-trip", "policy fixture")
			executor := hubProjectExecutor{f.service}
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(operatortool.LocalProjectArguments{ProjectID: view.ProjectID})
			if err != nil {
				t.Fatal(err)
			}
			read := operatortool.Call{Name: operatortool.LocalProjectConfiguration, Arguments: raw}
			result, err := executor.Execute(ctx, read)
			if err != nil {
				t.Fatal(err)
			}
			var observed struct {
				Selected json.RawMessage `json:"selected_policy"`
				Mismatch bool            `json:"policy_mismatch"`
			}
			if err := json.Unmarshal(result.Content, &observed); err != nil {
				t.Fatal(err)
			}
			if !observed.Mismatch {
				t.Fatalf("policy mismatch missing from MCP read: %s", result.Content)
			}
			readHealth := func(want bool) {
				t.Helper()
				credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
				if err != nil {
					t.Fatal(err)
				}
				runner, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}, r.binding.RunnerID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, problem := range runner.Problems {
					if problem.Code == "policy_mismatch" {
						found = problem.Message != "" && problem.FixHint != ""
					}
				}
				if found != want || (runner.Health == "needs_attention") != want {
					t.Fatalf("runner policy health=%s problems=%+v, want mismatch=%v", runner.Health, runner.Problems, want)
				}
				if exclusions := runner.Exclusions(f.project.ID, policy.Requirements{}, false); len(exclusions) != 0 {
					t.Fatalf("policy observation affected dispatch: %+v", exclusions)
				}
			}
			readReports := func(want int) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var setup onboarding.Project
				decodeHubResponse(t, response, &setup)
				if len(setup.ObservedPolicies) != want || want > 0 && setup.ObservedPolicies[0].Policy.ID != selected.ID {
					t.Fatalf("Integrations policy reports=%+v", setup.ObservedPolicies)
				}
				if want > 0 && (setup.ObservedPolicies[0].Source == nil || *setup.ObservedPolicies[0].Source != *source) {
					t.Fatalf("heartbeat discarded repository provenance: %+v", setup.ObservedPolicies)
				}
			}
			readHealth(false)
			readReports(1)
			omitted := selected
			if kind == "authored" {
				omitted.Authored = nil
			} else {
				omitted.Profile = ""
			}
			var refusal *operatortool.RequestError
			if _, err := executor.Execute(ctx, projectCall(t, "approve_project_policy", view.ProjectID, "incomplete-policy", operatortool.PolicyApprovalInput{ExpectedID: current.ID, Policy: omitted})); !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "entire local_project_configuration.selected_policy") {
				t.Fatalf("incomplete policy refusal=%v", err)
			}
			call := projectCall(t, "approve_project_policy", view.ProjectID, "selected-policy", struct {
				ExpectedID string          `json:"expected_policy_id"`
				Policy     json.RawMessage `json:"policy"`
			}{current.ID, observed.Selected})
			approved := projectAction(t, executor, ctx, call)
			if approved.Status != chatpkg.ActionSucceeded {
				t.Fatalf("selected policy approval=%+v", approved)
			}
			stored, err := f.service.readTenantProjectPolicy(t.Context(), f.project.OrganizationID, f.project.ID)
			if err != nil || stored.Policy.ID != selected.ID {
				t.Fatalf("approved policy=%+v %v", stored, err)
			}
			readReports(0)
			readHealth(false)
			readResult, err := executor.Execute(ctx, read)
			if err != nil {
				t.Fatal(err)
			}
			var before runnerauth.ProjectConfiguration
			if err := json.Unmarshal(readResult.Content, &before); err != nil {
				t.Fatal(err)
			}
			applyArgs, err := json.Marshal(operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: before.RunnerID, ExpectedRunnerRevision: before.RunnerRevision, RequestID: "apply-selected", ExpectedConfigRevision: before.ConfigRevision, ExpectedPolicyID: current.ID, PolicyID: selected.ID, SourceRevision: selected.SourceRevision})
			if err != nil {
				t.Fatal(err)
			}
			applied, err := executor.Execute(ctx, operatortool.Call{Name: "apply_local_project_policy", Arguments: applyArgs})
			if err != nil {
				t.Fatalf("apply selected=%s %v", applied.Content, err)
			}
			delivery := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
			requireNativeStatus(t, delivery, http.StatusOK)
			var routing runnerauth.RoutingSnapshot
			decodeHubResponse(t, delivery, &routing)
			if routing.ProjectConfigurationRequest == nil || routing.ProjectConfigurationRequest.Desired == nil {
				t.Fatal("selected policy command was not delivered")
			}
			view = *routing.ProjectConfigurationRequest.Desired
			view.RuntimeRegistered = true
			view.Applied = true
			view.Saved = true
			view.Pending = false
			view.EffectivePolicy, view.PolicyMismatch = &selected, true
			post()
			readHealth(false)
			readReports(0)
			result, err = executor.Execute(ctx, read)
			if err != nil || json.Unmarshal(result.Content, &observed) != nil || observed.Mismatch {
				t.Fatalf("applied policy read=%s %v", result.Content, err)
			}
		})
	}
}

func TestCloudProjectConfigurationOwner(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"resume", "resume drained", "resume stale runner", "resume stale configuration", "resume denied", "resume revoked", "drain", "entry personal drain", "entry service drain", "entry revoked drain", "entry expired drain", "entry member removed drain", "entry grant removed drain", "entry scoped drain", "entry service scoped drain", "entry downgraded drain", "entry service downgraded drain", "entry foreign organization drain", "entry rotated drain", "entry missing origin drain", "apply", "cloud apply", "running apply", "drained apply", "refused apply", "storage exhausted", "payload outage", "settlement without diagnostics", "settlement older diagnostics", "restart pending", "restart completion", "stale configuration", "stale policy", "stale source", "stale runner", "foreign runner", "foreign project", "busy", "busy binding enable", "busy binding disable", "racing binding enable", "serialization conflict", "replace pending", "revoked issuer", "revoked policy", "changed routing", "downgraded issuer", "wrong candidate"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "project-configuration")
			details := &configurationDetailCounter{DetailStore: f.service.runDetailStore()}
			f.service.config.RunDetails = details
			f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					if c.Request().Method == http.MethodPost && strings.HasSuffix(c.Request().URL.Path, "/heartbeat") {
						c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), configurationDetailRequestKey{}, true)))
					}
					return next(c)
				}
			})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.enroll(t)
			current := hubTestPolicy()
			candidate := current
			candidate.ConfigDigest = strings.Repeat("c", 64)
			candidate = candidate.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", current)
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "hub_configuration", ConfigRevision: strings.Repeat("a", 64), Registered: true, RuntimeRegistered: true, Paused: true, Source: "configured_committed_workflow", EffectivePolicy: &current, SelectedPolicy: &current, LocalBindingPolicy: &candidate, ObservedAt: time.Now()}
			if scenario == "cloud apply" {
				view.Source = "cloud_workflow"
			}
			if scenario == "running apply" || scenario == "resume drained" {
				view.Paused = false
			}
			if scenario == "drained apply" {
				view.Paused, view.Draining = false, true
			}
			if strings.HasPrefix(scenario, "settlement ") {
				view.Diagnostics = &runnerauth.ProjectDiagnostics{Source: "runner_runtime_and_durable_attempt_owners", ObservedAt: f.service.config.Clock().UTC(), RuntimeObservedAt: f.service.config.Clock().UTC(), Counts: map[string]int{}, Unavailable: map[string]string{}}
			}
			initialDiagnostics := view.Diagnostics
			seedHubConfiguration(t, f, r.binding.RunnerID, view)
			var entryNow time.Time
			heartbeat := func() runnerauth.RoutingSnapshot {
				t.Helper()
				if !entryNow.IsZero() {
					entryNow = entryNow.Add(time.Millisecond)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				return snapshot
			}
			heartbeat()
			writes := details.stores.Load()
			view.ObservedAt = view.ObservedAt.Add(time.Nanosecond)
			heartbeat()
			if got := details.stores.Load(); got != writes {
				t.Fatalf("unchanged heartbeat wrote configuration objects: %d -> %d", writes, got)
			}
			contexts := make(chan context.Context, 1)
			f.service.echo.GET("/api/v2/organizations/:organization/configuration-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/configuration-context-test", testHubAdminToken, nil), http.StatusOK)
			ctx := <-contexts
			var entryClaims cloudassert.Claims
			var entryRevoked bool
			var entryExpires time.Time
			var entrySubject string
			var entryResolutions int
			if strings.HasPrefix(scenario, "entry ") {
				entryNow = time.Now().UTC()
				f.service.config.Clock = func() time.Time { return entryNow }
				provider := newHostedSecurityProvider()
				shared := hostedSharedFixture{hostedSecurityFixture: hostedSecurityFixture{service: f.service, provider: provider, project: f.project.ID, base: f.base}}
				f.service.config.Hosted = hostedSharedTestConfig(provider, hostedSharedKey(7), 1).Hosted
				f.service.config.Hosted.OrganizationID = string(f.project.OrganizationID)
				tamperHostedBinding(t, configurationTestDatabase(t, f), "INSERT INTO hosted_tenant(organization_id,provider_id,bootstrap_subject,public_url,deployment,allocation_generation) VALUES ($1,'org_provider','user_owner','https://hub.example.test','shared',1) ON CONFLICT (organization_id) DO UPDATE SET provider_id=EXCLUDED.provider_id,bootstrap_subject=EXCLUDED.bootstrap_subject,public_url=EXCLUDED.public_url,deployment=EXCLUDED.deployment,allocation_generation=EXCLUDED.allocation_generation", f.project.OrganizationID)
				owner := shared.member(t, "configuration-owner", "owner", "")
				entrySubject = owner.identity.Subject
				if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner) VALUES ($1,$2,$3,true,true) ON CONFLICT (organization_id,user_id,project_id) DO UPDATE SET can_write=true,manage_runner=true", entrySubject, f.project.OrganizationID, f.project.ID); err != nil {
					t.Fatal(err)
				}
				entryClaims = cloudassert.Claims{Issuer: "entry", Audience: string(f.project.OrganizationID), ProviderOrganization: "org_provider", Subject: entrySubject, Role: "owner", Kind: cloudassert.KindKey, Generation: 1, IssuedAt: entryNow, ExpiresAt: entryNow.Add(20 * time.Second), Key: &apikey.KeyAuthority{ID: "key_configuration", CredentialBinding: strings.Repeat("b", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}
				if strings.Contains(scenario, "service") {
					entryClaims.Key.Kind, entryClaims.Subject = "service", "service:"+entryClaims.Key.ID
				}
				f.service.config.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
					entryResolutions++
					if entryRevoked || !entryExpires.IsZero() && !entryExpires.After(entryNow) || origin.KeyID != entryClaims.Key.ID || origin.CredentialBinding != entryClaims.Key.CredentialBinding || origin.Subject != entryClaims.Subject {
						return cloudassert.Claims{}, cloudassert.ErrInvalid
					}
					fresh := entryClaims
					fresh.IssuedAt, fresh.ExpiresAt = entryNow, entryNow.Add(20*time.Second)
					return fresh, nil
				}
				credential, _, err := f.service.entryKeyCredential(t.Context(), entryClaims)
				if err != nil {
					t.Fatal(err)
				}
				resolve := func(context.Context) (hubapi.APICredential, error) { return credential, nil }
				ctx = context.WithValue(ctx, hubOperatorResolverKey{}, resolve)
				ctx = context.WithValue(ctx, hubscope.NativeOperatorScopeKey{}, func(context.Context) (hubscope.NativeScope, error) {
					return hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: credential}, nil
				})
				connection := operatortool.CurrentConnection(ctx)
				connection.Identity = hubscope.OperatorIdentity(credential, string(f.project.OrganizationID))
				connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
					return f.service.operatorCurrentAuthority(ctx, credential, string(f.project.OrganizationID))
				}
				ctx = operatortool.WithConnection(ctx, connection)
				origin, err := json.Marshal(entryauthority.Origin{KeyID: entryClaims.Key.ID, CredentialBinding: entryClaims.Key.CredentialBinding, Subject: entryClaims.Subject, Issuer: "entry", Generation: 1})
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "entry missing origin drain" {
					origin = nil
				}
				ctx = context.WithValue(ctx, hubscope.OperatorEntryOriginKey{}, string(origin))
			}
			if scenario == "resume denied" {
				connection := operatortool.CurrentConnection(ctx)
				resolve := connection.Resolve
				connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
					authority, err := resolve(ctx)
					check := authority.Check
					authority.Check = func(ctx context.Context, requirement operatortool.Requirement) error {
						if requirement.ResourceKind == "runners" {
							return operatortool.ErrAccessDenied
						}
						return check(ctx, requirement)
					}
					return authority, err
				}
				ctx = operatortool.WithConnection(ctx, connection)
			}
			executor := hubProjectExecutor{f.service}
			call := func(name string, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
				t.Helper()
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
				if err == nil && name != operatortool.LocalProjectConfiguration && strings.Contains(string(result.Content), `"pending":true`) {
					credential, err := currentHubOperator(ctx)
					if err != nil {
						t.Fatal(err)
					}
					requireCommandPayloadObject(t, f.service, f.project.OrganizationID, name+" "+args.RunnerID+" "+args.ProjectID, args.RequestID, result.Content, credential.ID)
				}
				return result, err
			}
			result, err := call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID})
			if err != nil {
				t.Fatal(err)
			}
			var observed runnerauth.ProjectConfiguration
			if err := json.Unmarshal(result.Content, &observed); err != nil {
				t.Fatal(err)
			}
			if observed.RunnerID != r.binding.RunnerID || observed.ConfigRevision != view.ConfigRevision || observed.AllowLocalBinding || observed.LocalBindingPolicy == nil || observed.LocalBindingPolicy.ID != candidate.ID {
				t.Fatalf("configuration=%s", result.Content)
			}
			if scenario == "drain" || scenario == "resume drained" || strings.HasPrefix(scenario, "entry ") {
				args := operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: observed.RunnerID, ExpectedRunnerRevision: observed.RunnerRevision, RequestID: "drain", ExpectedConfigRevision: observed.ConfigRevision, ExpectedPolicyID: current.ID}
				result, err := call("drain_local_project", args)
				if scenario == "entry missing origin drain" {
					if err == nil {
						t.Fatal("entry drain queued without a durable origin")
					}
					return
				}
				if err != nil || !strings.Contains(string(result.Content), `"pending":true`) {
					t.Fatalf("drain queued=%s %v", result.Content, err)
				}
				if strings.HasPrefix(scenario, "entry ") {
					entryNow = entryNow.Add(time.Minute)
					switch scenario {
					case "entry revoked drain":
						entryRevoked = true
					case "entry expired drain":
						entryExpires = entryNow
					case "entry member removed drain":
						if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "UPDATE hosted_members SET active=false WHERE user_id=$1", entrySubject); err != nil {
							t.Fatal(err)
						}
					case "entry grant removed drain":
						if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=$1 AND project_id=$2", entrySubject, f.project.ID); err != nil {
							t.Fatal(err)
						}
					case "entry scoped drain", "entry service scoped drain":
						entryClaims.Key.ProjectContext = apikey.ProjectContext{Access: "project", Projects: []string{"prj_other"}}
					case "entry downgraded drain", "entry service downgraded drain":
						entryClaims.Key.Permission = apikey.ScopeRead
					case "entry foreign organization drain":
						entryClaims.Audience = "org_other"
					case "entry rotated drain":
						entryClaims.Key.CredentialBinding = strings.Repeat("c", 64)
					}
				}
				objectsBefore := details.stores.Load()
				request := heartbeat().ProjectConfigurationRequest
				if request != nil && details.stores.Load() != objectsBefore {
					t.Fatal("delivery heartbeat wrote an unused payload object")
				}
				if strings.HasPrefix(scenario, "entry ") && entryResolutions == 0 {
					t.Fatal("queued entry authority was not resolved from its current owner")
				}
				if strings.HasPrefix(scenario, "entry ") && scenario != "entry personal drain" && scenario != "entry service drain" {
					if request != nil {
						t.Fatalf("revoked current entry authority delivered drain: %+v", request)
					}
					return
				}
				if request == nil || request.Operation != "drain_local_project" || request.RequestID != args.RequestID {
					t.Fatalf("drain delivery=%+v", request)
				}
				view = *request.Desired
				view.RuntimeRegistered = true
				view.RequestID, view.Saved, view.Applied, view.Draining = args.RequestID, true, true, true
				if heartbeat().ProjectConfigurationRequest != nil {
					t.Fatal("acknowledged drain repeated")
				}
				if scenario == "drain" || strings.HasPrefix(scenario, "entry ") {
					return
				}
				view.RequestID = ""
				heartbeat()
				result, err = call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID})
				if err != nil || json.Unmarshal(result.Content, &observed) != nil || observed.Paused || !observed.Draining || observed.UnsettledAttempts != 0 {
					t.Fatalf("drain readback=%s %v", result.Content, err)
				}
			}
			if strings.HasPrefix(scenario, "resume") {
				args := operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: observed.RunnerID, ExpectedRunnerRevision: observed.RunnerRevision, RequestID: "resume", ExpectedConfigRevision: observed.ConfigRevision, ExpectedPolicyID: current.ID}
				if scenario == "resume stale runner" {
					args.ExpectedRunnerRevision++
				}
				if scenario == "resume stale configuration" {
					args.ExpectedConfigRevision = strings.Repeat("b", 64)
				}
				result, err := call("resume_local_project", args)
				if scenario == "resume denied" || strings.Contains(scenario, "stale") {
					if err == nil || heartbeat().ProjectConfigurationRequest != nil {
						t.Fatalf("resume refusal=%s %v", result.Content, err)
					}
					if strings.Contains(scenario, "stale") {
						var conflict *hubapi.NativeError
						if !errors.As(err, &conflict) || conflict.Code != "revision_conflict" || int64(conflict.CurrentRevision) != observed.RunnerRevision {
							t.Fatalf("resume CAS mismatch=%v", err)
						}
					}
					if scenario == "resume denied" && (!errors.Is(err, operatortool.ErrAccessDenied) || !strings.Contains(err.Error(), "manage_runner")) {
						t.Fatalf("unnamed permission=%v", err)
					}
					return
				}
				if err != nil || !strings.Contains(string(result.Content), `"pending":true`) {
					t.Fatalf("resume queued=%s %v", result.Content, err)
				}
				if scenario == "resume revoked" {
					if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = $1 WHERE token_hash = $2", time.Now().UTC(), apikey.HashToken(testHubAdminToken)); err != nil {
						t.Fatal(err)
					}
					if heartbeat().ProjectConfigurationRequest != nil {
						t.Fatal("revoked resume reached runner")
					}
					return
				}
				request := heartbeat().ProjectConfigurationRequest
				if request == nil || request.Operation != "resume_local_project" || request.RequestID != args.RequestID {
					t.Fatalf("resume delivery=%+v", request)
				}
				view = *request.Desired
				view.RuntimeRegistered = true
				view.RequestID, view.Saved, view.Applied, view.Paused, view.Draining = args.RequestID, true, true, false, false
				if heartbeat().ProjectConfigurationRequest != nil {
					t.Fatal("acknowledged resume repeated")
				}
				result, err = call("resume_local_project", args)
				if err != nil || !strings.Contains(string(result.Content), `"applied":true`) || !strings.Contains(string(result.Content), `"paused":false`) || !strings.Contains(string(result.Content), `"draining":false`) {
					t.Fatalf("resume receipt=%s %v", result.Content, err)
				}
				view.RequestID = ""
				heartbeat()
				result, err = call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID})
				if err != nil || json.Unmarshal(result.Content, &observed) != nil || observed.Paused || observed.Draining || observed.LastOperation == nil || observed.LastOperation.Operation != "resume_local_project" || !observed.LastOperation.Applied {
					t.Fatalf("resume readback=%s %v", result.Content, err)
				}
				return
			}
			var activeLease tracker.NativeLease
			var activeEvent tracker.NativeRunEvent
			var activeWorker, activePath string
			if scenario == "busy" || strings.HasPrefix(scenario, "busy binding ") {
				item := f.create(t, "Running work")
				activeWorker = r.redemption.Credential
				r.reportCapacity(t)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", activeWorker, tracker.NativeClaim{PolicyID: current.ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: "active-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &activeLease)
				activeEvent = nativeStartedEvent(activeLease)
				activePath = f.base + "/work-items/" + string(item.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, activePath+"/events", activeWorker, activeEvent), http.StatusOK)
			}
			enabled := true
			args := operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: observed.RunnerID, ExpectedRunnerRevision: observed.RunnerRevision, RequestID: "enable-local-binding", ExpectedConfigRevision: observed.ConfigRevision, ExpectedPolicyID: current.ID, PolicyID: candidate.ID, SourceRevision: candidate.SourceRevision, AllowLocalBinding: &enabled}
			if scenario == "busy binding disable" {
				enabled = false
				view.AllowLocalBinding = true
				view.RestrictedBindingPolicy = &candidate
				seedHubConfiguration(t, f, r.binding.RunnerID, view)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: current.ID, Policy: candidate}), http.StatusOK)
			switch scenario {
			case "stale configuration":
				args.ExpectedConfigRevision = strings.Repeat("b", 64)
			case "stale policy":
				args.ExpectedPolicyID = "policy_stale"
			case "stale source":
				args.SourceRevision = strings.Repeat("f", 64)
			case "stale runner":
				args.ExpectedRunnerRevision++
			case "foreign runner":
				args.RunnerID = runnerauth.NewBinding().RunnerID
			case "foreign project":
				args.ProjectID = "prj_foreign"
			case "wrong candidate":
				forged := candidate
				forged.ConfigDigest = strings.Repeat("e", 64)
				forged = forged.WithID()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: candidate.ID, Policy: forged}), http.StatusOK)
				args.PolicyID = forged.ID
			case "busy":
				args.RequestID, args.AllowLocalBinding = "apply-policy", nil
				view.UnsettledAttempts = 1
				heartbeat()
			case "busy binding enable", "busy binding disable":
				view.UnsettledAttempts = 1
				heartbeat()
			}
			if scenario == "racing binding enable" {
				item := f.create(t, "Concurrent work")
				r.reportCapacity(t)
				ctx = context.WithValue(ctx, configurationDetailMutationKey{}, true)
				details.interleave = func() {
					details.interleave = nil
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, tracker.NativeClaim{PolicyID: candidate.ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: "racing-configuration", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			var serialization *configurationSerializationRepository
			if scenario == "serialization conflict" {
				serialization = &configurationSerializationRepository{Repository: f.service.config.TenantRepository}
				f.service.config.TenantRepository = serialization
				ctx = context.WithValue(ctx, configurationSerializationKey{}, true)
			}
			preview, previewErr := executor.projectConfigurationCommand(ctx, "apply_local_project_policy", args, true)
			result, err = call("apply_local_project_policy", args)
			if scenario == "serialization conflict" {
				var conflict *hubapi.NativeError
				if previewErr != nil || !errors.As(err, &conflict) || conflict.Code != "revision_conflict" || serialization.attempts.Load() != 1 {
					t.Fatalf("serialization retry spun: preview=%v result=%s error=%v attempts=%d", previewErr, result.Content, err, serialization.attempts.Load())
				}
				return
			}
			if scenario == "racing binding enable" {
				var conflict *hubapi.NativeError
				if previewErr != nil || !errors.As(err, &conflict) || conflict.Code != "policy_mismatch" {
					t.Fatalf("claim between plan and write was not refused: preview=%s %v result=%s %v", preview.Content, previewErr, result.Content, err)
				}
				if heartbeat().ProjectConfigurationRequest != nil {
					t.Fatal("settlement race delivered a binding change")
				}
				return
			}
			refused := scenario == "stale configuration" || scenario == "stale policy" || scenario == "stale source" || scenario == "stale runner" || scenario == "foreign runner" || scenario == "foreign project" || scenario == "wrong candidate" || strings.HasPrefix(scenario, "busy binding ")
			if refused {
				if scenario == "foreign runner" {
					if !strings.Contains(string(preview.Content), `"source":"unavailable"`) || !strings.Contains(string(result.Content), `"source":"unavailable"`) {
						t.Fatalf("foreign owner resolved: preview=%s apply=%s", preview.Content, result.Content)
					}
				} else if previewErr == nil || err == nil {
					t.Fatalf("invalid request accepted: preview=%s %v apply=%s %v", preview.Content, previewErr, result.Content, err)
				}
				if strings.HasPrefix(scenario, "busy binding ") {
					for _, refusal := range []error{previewErr, err} {
						var conflict *hubapi.NativeError
						if !errors.As(refusal, &conflict) || conflict.Code != "policy_mismatch" {
							t.Fatalf("binding refusal=%v", refusal)
						}
					}
				}
				if heartbeat().ProjectConfigurationRequest != nil {
					t.Fatal("refusal reached runner")
				}
				return
			}
			if previewErr != nil || err != nil || !strings.Contains(string(result.Content), `"pending":true`) {
				t.Fatalf("preview=%s %v queued=%s %v", preview.Content, previewErr, result.Content, err)
			}
			if scenario == "busy" {
				var previewView runnerauth.ProjectConfiguration
				if err := json.Unmarshal(preview.Content, &previewView); err != nil || previewView.UnsettledAttempts != 1 || previewView.Pending {
					t.Fatalf("busy preview=%s %v", preview.Content, err)
				}
			}
			switch scenario {
			case "revoked issuer":
				if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = $1 WHERE token_hash = $2", time.Now().UTC(), apikey.HashToken(testHubAdminToken)); err != nil {
					t.Fatal(err)
				}
			case "downgraded issuer":
				if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "UPDATE api_tokens SET scope = $1 WHERE token_hash = $2", "operator", apikey.HashToken(testHubAdminToken)); err != nil {
					t.Fatal(err)
				}
			case "revoked policy":
				if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id = $1 AND project_id = $2", f.project.OrganizationID, f.project.ID); err != nil {
					t.Fatal(err)
				}
			case "changed routing":
				if _, err := configurationTestDatabase(t, f).ExecContext(t.Context(), "UPDATE runner_identities SET revision = revision + 1 WHERE runner_id = $1", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "replace pending" {
				oldArgs := args
				delivered := heartbeat().ProjectConfigurationRequest
				if delivered == nil || delivered.Desired == nil || delivered.Desired.RunnerRevision <= observed.RunnerRevision {
					t.Fatalf("pending delivery did not carry committed runner revision: %+v", delivered)
				}
				view = *delivered.Desired
				view.RuntimeRegistered = true
				view.Applied = false
				view.Saved = false
				view.AllowLocalBinding = false
				view.Constraint = "Runtime policy changed before lifecycle settlement."
				if next := heartbeat().ProjectConfigurationRequest; next == nil || next.RequestID != oldArgs.RequestID {
					t.Fatal("partial effect was falsely settled")
				}
				currentResult, err := call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID})
				var currentView runnerauth.ProjectConfiguration
				if err != nil || json.Unmarshal(currentResult.Content, &currentView) != nil {
					t.Fatalf("read pending=%s %v", currentResult.Content, err)
				}
				args.RequestID = "replace-partial-application"
				args.AllowLocalBinding = nil
				args.ExpectedRunnerRevision = currentView.RunnerRevision
				args.ExpectedConfigRevision = currentView.ConfigRevision
				args.ExpectedPolicyID = currentView.EffectivePolicy.ID
				replacement, err := call("apply_local_project_policy", args)
				if err != nil || !strings.Contains(string(replacement.Content), `"pending":true`) {
					t.Fatalf("pending request prevented corrective intent: %s %v", replacement.Content, err)
				}
				superseded, err := call("apply_local_project_policy", oldArgs)
				var oldOutcome runnerauth.ProjectConfiguration
				if err != nil || json.Unmarshal(superseded.Content, &oldOutcome) != nil || oldOutcome.Pending || oldOutcome.Applied || !strings.Contains(oldOutcome.Constraint, "superseded") || !strings.Contains(oldOutcome.Constraint, "not confirmed") {
					t.Fatalf("old request lost uncertain outcome: %s %v", superseded.Content, err)
				}
				oldReport := *delivered.Desired
				oldReport.RuntimeRegistered = true
				oldReport.Applied = true
				oldReport.Saved = true
				stale := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": oldReport})
				requireNativeCode(t, stale, http.StatusUnprocessableEntity, "invalid_request")
				view = currentView
				view.RequestID = ""
				view.RunnerRevision = 0
				view.Constraint = ""
			}
			if scenario == "restart pending" {
				f.service = reopenPostgresNativeService(t, f.service)
				executor = hubProjectExecutor{f.service}
			}
			if scenario == "payload outage" {
				details.fail.Store(true)
				unavailable := heartbeat()
				if unavailable.ProjectConfigurationRequest != nil || unavailable.ProjectConfiguration != nil {
					t.Fatal("object outage delivered or acknowledged configuration")
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					command, err := reader.(postgres.ConfigurationReader).ConfigurationCommand(t.Context(), r.binding.RunnerID, f.project.ID)
					if err != nil {
						return err
					}
					if command.Status != "pending" {
						t.Fatalf("object outage settled command: %+v", command)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				details.fail.Store(false)
			}
			snapshot := heartbeat()
			if scenario != "apply" && scenario != "cloud apply" && scenario != "drained apply" && scenario != "running apply" && scenario != "busy" && scenario != "refused apply" && scenario != "storage exhausted" && scenario != "payload outage" && !strings.HasPrefix(scenario, "settlement ") && scenario != "restart pending" && scenario != "restart completion" && scenario != "replace pending" {
				if snapshot.ProjectConfigurationRequest != nil {
					t.Fatal("revoked or stale request reached runner")
				}
				if scenario == "changed routing" || scenario == "revoked policy" {
					replay, err := call("apply_local_project_policy", args)
					var outcome runnerauth.ProjectConfiguration
					want := "runner revision"
					if scenario == "revoked policy" {
						want = "approved policy"
					}
					if err != nil || json.Unmarshal(replay.Content, &outcome) != nil || !strings.Contains(outcome.Constraint, want) || strings.Contains(outcome.Constraint, "permission") {
						t.Fatalf("wrong refusal cause: %s %v", replay.Content, err)
					}
				}
				var pending int
				if err := configurationTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM runner_project_configurations WHERE runner_id = $1 AND request_status='pending'", r.binding.RunnerID).Scan(&pending); err != nil || pending != 0 {
					t.Fatalf("refused request retained application: %d %v", pending, err)
				}
				return
			}
			if snapshot.ProjectConfigurationRequest == nil || snapshot.ProjectConfigurationRequest.RequestID != args.RequestID || snapshot.ProjectConfigurationRequest.ProjectID != view.ProjectID || !reflect.DeepEqual(snapshot.ProjectConfigurationRequest.AllowLocalBinding, args.AllowLocalBinding) {
				t.Fatalf("delivery=%+v", snapshot)
			}
			queuedReplay, err := call("apply_local_project_policy", args)
			if err != nil || !strings.Contains(string(queuedReplay.Content), `"pending":true`) {
				t.Fatalf("queued replay=%s %v", queuedReplay.Content, err)
			}
			view = *snapshot.ProjectConfigurationRequest.Desired
			if scenario == "settlement older diagnostics" {
				view.Diagnostics = &runnerauth.ProjectDiagnostics{Source: "runner_runtime_and_durable_attempt_owners", ObservedAt: f.service.config.Clock().UTC().Add(-time.Hour), RuntimeObservedAt: f.service.config.Clock().UTC().Add(-time.Hour), Counts: map[string]int{}, Unavailable: map[string]string{}}
			}
			view.RuntimeRegistered = true
			view.RequestID, view.Saved, view.Applied, view.AllowLocalBinding = args.RequestID, true, true, true
			if scenario == "busy" || scenario == "replace pending" {
				view.AllowLocalBinding = false
			}
			view.EffectivePolicy = &candidate
			if scenario == "refused apply" {
				view.Applied, view.Saved, view.AllowLocalBinding = false, false, false
				view.EffectivePolicy, view.ConfigRevision = &current, args.ExpectedConfigRevision
				view.Constraint = "The configured definition cannot supply the approved policy revision."
			}
			if scenario == "storage exhausted" {
				view.Constraint = ""
				f.service.config.Hosted = &hostedplans.HostedConfig{}
				f.service.config.Hosted.OrganizationID = string(f.project.OrganizationID)
				configurationTestStorageLimit(t, f, 1)
				_, storageErr := prepareChangeContent(f.service.contentContext(t.Context()), string(f.project.OrganizationID), []byte(strings.Repeat("ordinary billable storage probe", 80)))
				var quota *hostedplans.LimitError
				if !errors.As(storageErr, &quota) || quota.Resource != "collaboration_bytes" || quota.Allowance != 1 {
					t.Fatalf("one-byte storage allowance did not refuse an ordinary write: %v", storageErr)
				}
				f.service.config.Hosted = nil
			}
			if heartbeat().ProjectConfigurationRequest != nil {
				t.Fatal("acknowledged request repeated")
			}
			if scenario == "busy" {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(activeLease.ID)+"/renew", activeWorker, tracker.NativeLeaseMutation{FencingToken: activeLease.FencingToken, TTLSeconds: 90})
				requireNativeStatus(t, response, http.StatusOK)
				var renewed tracker.NativeLease
				decodeHubResponse(t, response, &renewed)
				if renewed.PolicyID != current.ID || renewed.FencingToken != activeLease.FencingToken {
					t.Fatalf("apply changed active lease identity: %+v", renewed)
				}
				activeEvent.Type, activeEvent.IdempotencyKey, activeEvent.Data.Sequence = "run.checkpointed", "after-policy-apply", 2
				activeEvent.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, activePath+"/events", activeWorker, activeEvent), http.StatusOK)
				response = performHubAPIRequest(t, f.service, http.MethodGet, activePath+"/runtime", activeWorker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &evidence)
				if evidence.Attempt == nil || evidence.Attempt.PolicyID != current.ID || evidence.Attempt.Sequence != 2 {
					t.Fatalf("apply changed running attempt policy: %+v", evidence.Attempt)
				}
			}
			if scenario == "refused apply" {
				view.RequestID, view.Constraint = "", ""
				heartbeat()
				result, err := call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID})
				if err != nil {
					t.Fatal(err)
				}
				var readback runnerauth.ProjectConfiguration
				if err := json.Unmarshal(result.Content, &readback); err != nil {
					t.Fatal(err)
				}
				if readback.LastOperation == nil || readback.LastOperation.RequestID != args.RequestID || readback.LastOperation.Constraint == "" || readback.LastOperation.Applied || readback.LastOperation.Saved {
					t.Fatalf("refusal receipt lost after heartbeat: %s", result.Content)
				}
				if readback.Constraint != "" {
					t.Fatal("historical refusal became a current configuration constraint")
				}
				args.RequestID = "retry-policy-application"
				args.ExpectedRunnerRevision = readback.RunnerRevision
				retry, err := call("apply_local_project_policy", args)
				if err != nil || !strings.Contains(string(retry.Content), `"pending":true`) {
					t.Fatalf("historical refusal blocked retry: %s %v", retry.Content, err)
				}
				return
			}
			if scenario == "restart completion" {
				f.service = reopenPostgresNativeService(t, f.service)
				executor = hubProjectExecutor{f.service}
			}
			if strings.HasPrefix(scenario, "settlement ") {
				result, err := call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID})
				var fresh runnerauth.ProjectConfiguration
				if err != nil || json.Unmarshal(result.Content, &fresh) != nil || fresh.EffectivePolicy == nil || fresh.EffectivePolicy.ID != candidate.ID || fresh.SelectedPolicy == nil || fresh.SelectedPolicy.ID != candidate.ID || fresh.Diagnostics == nil || !fresh.Diagnostics.ObservedAt.Equal(initialDiagnostics.ObservedAt) || !fresh.Diagnostics.RuntimeObservedAt.Equal(initialDiagnostics.RuntimeObservedAt) {
					t.Fatalf("settlement lost current policy or retained diagnostics: %s %v", result.Content, err)
				}
			}
			completedReplay, err := call("apply_local_project_policy", args)
			if err != nil || !strings.Contains(string(completedReplay.Content), `"applied":true`) || strings.Contains(string(completedReplay.Content), `"pending":true`) {
				t.Fatalf("completed replay=%s %v", completedReplay.Content, err)
			}
			if scenario == "storage exhausted" {
				return
			}
			changed := args
			changed.PolicyID = current.ID
			if _, err := call("apply_local_project_policy", changed); err == nil {
				t.Fatal("changed idempotent request replayed")
			}
			result, err = call(operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: r.binding.RunnerID})
			if err != nil || !strings.Contains(string(result.Content), `"applied":true`) || strings.Contains(string(result.Content), r.redemption.Credential) {
				t.Fatalf("receipt=%s %v", result.Content, err)
			}
		})
	}
}

func configurationTestDatabase(t *testing.T, f nativeFixture) *sql.DB {
	t.Helper()
	value, ok := projectRecordDatabases.Load(f.service.config.TenantRepository)
	if !ok {
		t.Fatal("PostgreSQL fixture administrator is unavailable")
	}
	return value.(*sql.DB)
}

func seedHubConfiguration(t *testing.T, f nativeFixture, runner string, view runnerauth.ProjectConfiguration) {
	t.Helper()
	view.Authority = "hub_configuration"
	if view.ConfigRevision == "" {
		var err error
		view.ConfigRevision, err = projectConfigurationDigest(view)
		if err != nil {
			t.Fatal(err)
		}
	}
	ref, err := f.service.prepareConfigurationPayload(t.Context(), f.project.OrganizationID, view)
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), runner)
		if err != nil {
			return err
		}
		owner := w.(postgres.ConfigurationWriter)
		now := f.service.config.Clock().UTC()
		authority := postgres.ConfigurationAuthority{RunnerID: runner, EnrollmentID: identity.EnrollmentID, Revision: identity.Revision, At: now}
		_, err = owner.InitializeConfiguration(t.Context(), postgres.ConfigurationHeader{OrganizationID: f.project.OrganizationID, RunnerID: runner, SourceRevision: view.ConfigRevision, CreatedAt: now}, authority)
		if err != nil {
			return err
		}
		header, err := owner.ConfigurationHeader(t.Context(), runner)
		if err != nil {
			return err
		}
		_, err = owner.ApplyConfiguration(t.Context(), postgres.ConfigurationChange{OrganizationID: f.project.OrganizationID, Authority: authority, ExpectedRevision: header.Revision, ExpectedSourceRevision: header.SourceRevision, SourceRevision: view.ConfigRevision, Projects: []postgres.ConfigurationProject{configurationProjectRow(f.project.OrganizationID, runner, view, ref, now)}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func configurationTestStorageLimit(t *testing.T, f nativeFixture, limit int64) {
	t.Helper()
	plans := hostedplans.PilotPlans()
	selected := plans.Plans[0]
	selected.PlanReference = hostedplans.PlanReference{ID: "configuration_storage", Version: 1}
	selected.Allowances["collaboration_bytes"] = limit
	plans.Plans = []hostedplans.HostedPlan{selected}
	plans.Base = selected.PlanReference
	cfg := *f.service.config.Hosted
	cfg.Plans = &plans
	f.service.database.billingAuditStore = f.service.runDetailStore()
	f.service.database.hostedOrganization = f.project.OrganizationID
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		assignment, err := w.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		assignment.Base = billingstore.PlanReference{ID: selected.ID, Version: selected.Version}
		assignment.Subscription = nil
		return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
	})
	if err != nil {
		t.Fatal(err)
	}
}

type configurationDetailRequestKey struct{}
type configurationDetailMutationKey struct{}
type configurationDetailCounter struct {
	attachment.DetailStore
	stores     atomic.Int64
	fail       atomic.Bool
	interleave func()
}

func (d *configurationDetailCounter) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	if ctx.Value(configurationDetailRequestKey{}) == true {
		d.stores.Add(1)
	}
	if d.fail.Load() {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	ref, err := d.DetailStore.StoreDetail(ctx, org, raw)
	if err == nil && ctx.Value(configurationDetailMutationKey{}) == true && d.interleave != nil {
		d.interleave()
	}
	return ref, err
}
func (d *configurationDetailCounter) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if d.fail.Load() {
		return nil, artifact.ErrStorage
	}
	return d.DetailStore.ReadDetail(ctx, org, ref)
}

type configurationSerializationKey struct{}
type configurationSerializationRepository struct {
	tenantstore.Repository
	attempts atomic.Int64
}

type configurationSerializationWriter struct {
	tenantstore.Writer
	attempts *atomic.Int64
}

func (w configurationSerializationWriter) LockRunnerUpdate(context.Context, string) error {
	w.attempts.Add(1)
	return corestore.ErrSerializationFailure
}

func (r *configurationSerializationRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if ctx.Value(configurationSerializationKey{}) == true {
		return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
			return write(configurationSerializationWriter{Writer: w, attempts: &r.attempts})
		})
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}
