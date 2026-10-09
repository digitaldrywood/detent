package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestSelectedProjectPolicyRoundTrip(t *testing.T) {
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
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Registered: true, RuntimeRegistered: true, Source: "configured_committed_workflow", SelectedPolicy: &selected, EffectivePolicy: &current, ObservedAt: f.service.config.now()}
			post := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
			}
			post()
			scope := string(f.project.OrganizationID) + "/" + view.ProjectID
			reports, err := readObservedPolicies(t.Context(), f.service.database.db, scope, current, f.service.config.now())
			if err != nil || len(reports) != 1 || reports[0].Policy.ID != selected.ID {
				t.Fatalf("heartbeat selected policy reports=%+v %v", reports, err)
			}
			source := &policy.RepositorySource{Repository: "digitaldrywood/detent", Commit: strings.Repeat("b", 40)}
			if err := storeObservedPolicy(t.Context(), f.service.database.db, scope, r.binding.RunnerID, policy.Observation{Descriptor: selected, Source: source}, f.service.config.now()); err != nil {
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
				runner, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
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
			stored, err := readProjectPolicy(t.Context(), f.service.database.db, scope)
			if err != nil || stored.Policy.ID != selected.ID {
				t.Fatalf("approved policy=%+v %v", stored, err)
			}
			readReports(0)
			readHealth(true)
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
	for _, scenario := range []string{"resume", "resume drained", "resume stale runner", "resume stale configuration", "resume denied", "resume revoked", "drain", "apply", "running apply", "drained apply", "refused apply", "storage exhausted", "stale configuration", "stale runner", "foreign runner", "foreign project", "busy", "revoked issuer", "revoked policy", "changed routing", "downgraded issuer", "wrong candidate"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "project-configuration")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			current := hubTestPolicy()
			candidate := current
			candidate.ConfigDigest = strings.Repeat("c", 64)
			candidate = candidate.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", current)
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", ConfigRevision: strings.Repeat("a", 64), Registered: true, RuntimeRegistered: true, Paused: true, Source: "configured_committed_workflow", EffectivePolicy: &current, SelectedPolicy: &current, LocalBindingPolicy: &candidate, ObservedAt: time.Now()}
			if scenario == "running apply" || scenario == "resume drained" {
				view.Paused = false
			}
			if scenario == "drained apply" {
				view.Paused, view.Draining = false, true
			}
			heartbeatStatus := http.StatusOK
			heartbeat := func() runnerauth.RoutingSnapshot {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
				requireNativeStatus(t, response, heartbeatStatus)
				if heartbeatStatus != http.StatusOK {
					if !strings.Contains(response.Body.String(), "collaboration_bytes") {
						t.Fatalf("storage refusal=%s", response.Body.String())
					}
					return runnerauth.RoutingSnapshot{}
				}
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				return snapshot
			}
			heartbeat()
			contexts := make(chan context.Context, 1)
			f.service.echo.GET("/api/v2/organizations/:organization/configuration-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/configuration-context-test", testHubAdminToken, nil), http.StatusOK)
			ctx := <-contexts
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
				return executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
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
			if scenario == "drain" || scenario == "resume drained" {
				args := operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: observed.RunnerID, ExpectedRunnerRevision: observed.RunnerRevision, RequestID: "drain", ExpectedConfigRevision: observed.ConfigRevision, ExpectedPolicyID: current.ID}
				result, err := call("drain_local_project", args)
				if err != nil || !strings.Contains(string(result.Content), `"pending":true`) {
					t.Fatalf("drain queued=%s %v", result.Content, err)
				}
				request := heartbeat().ProjectConfigurationRequest
				if request == nil || request.Operation != "drain_local_project" || request.RequestID != args.RequestID {
					t.Fatalf("drain delivery=%+v", request)
				}
				view.RequestID, view.Saved, view.Applied, view.Draining = args.RequestID, true, true, true
				if heartbeat().ProjectConfigurationRequest != nil {
					t.Fatal("acknowledged drain repeated")
				}
				if scenario == "drain" {
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
						var conflict *nativeError
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
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE token_hash = ?", formatHubTime(time.Now()), apikey.HashToken(testHubAdminToken)); err != nil {
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
			enabled := true
			args := operatortool.LocalProjectArguments{ProjectID: view.ProjectID, RunnerID: observed.RunnerID, ExpectedRunnerRevision: observed.RunnerRevision, RequestID: "enable-local-binding", ExpectedConfigRevision: observed.ConfigRevision, ExpectedPolicyID: current.ID, PolicyID: candidate.ID, SourceRevision: candidate.SourceRevision, AllowLocalBinding: &enabled}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: current.ID, Policy: candidate}), http.StatusOK)
			switch scenario {
			case "stale configuration":
				args.ExpectedConfigRevision = strings.Repeat("b", 64)
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
				view.UnsettledAttempts = 1
				heartbeat()
			}
			result, err = call("apply_local_project_policy", args)
			refused := scenario == "stale configuration" || scenario == "stale runner" || scenario == "foreign runner" || scenario == "foreign project" || scenario == "busy" || scenario == "wrong candidate"
			if refused {
				if err == nil && strings.Contains(string(result.Content), `"pending":true`) {
					t.Fatal("invalid request was queued")
				}
				if heartbeat().ProjectConfigurationRequest != nil {
					t.Fatal("refusal reached runner")
				}
				return
			}
			if err != nil || !strings.Contains(string(result.Content), `"pending":true`) {
				t.Fatalf("queued=%s %v", result.Content, err)
			}
			switch scenario {
			case "revoked issuer":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE token_hash = ?", formatHubTime(time.Now()), apikey.HashToken(testHubAdminToken)); err != nil {
					t.Fatal(err)
				}
			case "downgraded issuer":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET scope = ? WHERE token_hash = ?", "operator", apikey.HashToken(testHubAdminToken)); err != nil {
					t.Fatal(err)
				}
			case "revoked policy":
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope = ?", string(f.project.OrganizationID)+"/"+string(f.project.ID)); err != nil {
					t.Fatal(err)
				}
			case "changed routing":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET revision = revision + 1 WHERE id = ?", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := heartbeat()
			if scenario != "apply" && scenario != "drained apply" && scenario != "running apply" && scenario != "refused apply" && scenario != "storage exhausted" {
				if snapshot.ProjectConfigurationRequest != nil {
					t.Fatal("revoked or stale request reached runner")
				}
				var pending int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_identities WHERE id = ? AND json_extract(routing_settings_json, '$.project_configuration_request') IS NOT NULL", r.binding.RunnerID).Scan(&pending); err != nil || pending != 0 {
					t.Fatalf("refused request retained application: %d %v", pending, err)
				}
				return
			}
			if snapshot.ProjectConfigurationRequest == nil || snapshot.ProjectConfigurationRequest.RequestID != args.RequestID || snapshot.ProjectConfigurationRequest.ProjectID != view.ProjectID {
				t.Fatalf("delivery=%+v", snapshot)
			}
			queuedReplay, err := call("apply_local_project_policy", args)
			if err != nil || !strings.Contains(string(queuedReplay.Content), `"pending":true`) {
				t.Fatalf("queued replay=%s %v", queuedReplay.Content, err)
			}
			view.RequestID, view.Saved, view.Applied, view.AllowLocalBinding = args.RequestID, true, true, true
			view.EffectivePolicy, view.ConfigRevision = &candidate, strings.Repeat("d", 64)
			if scenario == "refused apply" {
				view.Applied, view.Saved, view.AllowLocalBinding = false, false, false
				view.EffectivePolicy, view.ConfigRevision = &current, args.ExpectedConfigRevision
				view.Constraint = "The configured definition cannot supply the approved policy revision."
			}
			if scenario == "storage exhausted" {
				view.Constraint = strings.Repeat("x", 1120)
				f.service.config.Hosted = &HostedConfig{}
				f.service.database.hostedOrganization = f.project.OrganizationID
				hostedTestPlans(t, f.service, map[string]int64{"collaboration_bytes": 1})
				f.service.config.Hosted = nil
				heartbeatStatus = http.StatusTooManyRequests
			}
			if heartbeat().ProjectConfigurationRequest != nil {
				t.Fatal("acknowledged request repeated")
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
			completedReplay, err := call("apply_local_project_policy", args)
			if scenario == "storage exhausted" {
				if err != nil || !strings.Contains(string(completedReplay.Content), `"pending":true`) {
					t.Fatalf("rejected receipt persisted=%s %v", completedReplay.Content, err)
				}
				var pending int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_identities WHERE id = ? AND json_extract(routing_settings_json, '$.project_configuration_request') IS NOT NULL", r.binding.RunnerID).Scan(&pending); err != nil || pending != 1 {
					t.Fatalf("rejected receipt lost pending command: %d %v", pending, err)
				}
				return
			}
			if err != nil || !strings.Contains(string(completedReplay.Content), `"applied":true`) || strings.Contains(string(completedReplay.Content), `"pending":true`) {
				t.Fatalf("completed replay=%s %v", completedReplay.Content, err)
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
