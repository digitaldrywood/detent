package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestCloudProjectConfigurationOwner(t *testing.T) {
	for _, scenario := range []string{"apply", "drained apply", "stale configuration", "stale runner", "foreign runner", "foreign project", "busy", "revoked issuer", "revoked policy", "changed routing", "downgraded issuer", "wrong candidate"} {
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
			if scenario == "drained apply" {
				view.Paused, view.Draining = false, true
			}
			heartbeat := func() runnerauth.RoutingSnapshot {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "protocol_major": 2, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				return snapshot
			}
			heartbeat()
			contexts := make(chan context.Context, 1)
			f.service.echo.GET("/api/v2/organizations/:organization/configuration-context-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/configuration-context-test", testHubAdminToken, nil), http.StatusOK)
			ctx := <-contexts
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
			if scenario != "apply" && scenario != "drained apply" {
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
			if heartbeat().ProjectConfigurationRequest != nil {
				t.Fatal("acknowledged request repeated")
			}
			completedReplay, err := call("apply_local_project_policy", args)
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
