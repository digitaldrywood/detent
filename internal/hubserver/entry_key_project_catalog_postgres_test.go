package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresEntryKeyProjectCatalog(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
		org := cfg.Hosted.OrganizationID
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	for _, org := range []string{"org_startup", "org_other"} {
		service := f.services[org]
		organization := tracker.OrganizationID(org)
		project, runner := org+"_project", org+"_runner"
		now := time.Now().UTC()
		expires := now.Add(time.Hour)
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: tracker.ProjectID(project), Name: org + " example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: tracker.ProjectID(project), CanWrite: true, ManageRunner: true}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: org + "_owner", ProjectID: tracker.ProjectID(project)}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: runner, Name: runner, Hash: apikey.HashToken(runner), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "all", Kind: "service"}); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: runner, Hostname: runner, Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: runner, RunnerID: runner, MachineID: tracker.MachineID(runner), TokenHash: apikey.HashToken(runner + "_enrollment"), Scope: "organization", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: expires, CreatedBy: org + "_owner"}); err != nil {
				return err
			}
			return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runner, MachineID: tracker.MachineID(runner), TokenID: runner, EnrollmentID: runner, Scope: "organization", State: "active", DisplayName: runner, Operations: []string{runnerauth.Read}, CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "arm64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			project, runner := org+"_project", org+"_runner"
			keyClaims := func(c *cloudassert.Claims) {
				c.Kind = cloudassert.KindKey
				c.ProviderOrganization = "provider_" + org
				c.Key = &apikey.KeyAuthority{ID: org + "_owner", CredentialBinding: strings.Repeat("d", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
			}
			catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, keyClaims)
			requireNativeStatus(t, catalog, http.StatusOK)
			var definitions []operatortool.Definition
			decodeHubResponse(t, catalog, &definitions)
			available := map[string]bool{}
			for _, definition := range definitions {
				available[definition.Name] = true
			}
			call := func(t *testing.T, name, arguments, connection string) json.RawMessage {
				t.Helper()
				body := fmt.Sprintf(`{"name":%q,"connection_id":%q,"arguments":%s}`, name, connection, arguments)
				response := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
				requireNativeStatus(t, response, http.StatusOK)
				if !available[name] {
					t.Fatalf("successful tool missing from catalog: %s", name)
				}
				return append(json.RawMessage(nil), response.Body.Bytes()...)
			}
			for _, test := range []struct {
				name, arguments, marker string
			}{
				{"project_setup", fmt.Sprintf(`{"project_id":%q}`, project), `"required_steps"`},
				{"get_onboarding", fmt.Sprintf(`{"project_id":%q}`, project), `"repository_binding_required"`},
				{"get_git_hub_batch", fmt.Sprintf(`{"project_id":%q}`, project), `"data"`},
				{"get_project_integration", fmt.Sprintf(`{"project_id":%q}`, project), `"profile":"native"`},
				{operatortool.GetProjectModelSelection, fmt.Sprintf(`{"project_id":%q}`, project), `"effective"`},
				{operatortool.RunnerProjectDiagnostics, fmt.Sprintf(`{"project_id":%q,"limit":10}`, project), `"project_id":"` + project + `"`},
				{operatortool.GetRunnerUpdate, fmt.Sprintf(`{"runner_id":%q}`, runner), runner},
				{operatortool.GetUrgentRunnerUpdate, `{}`, `"data"`},
				{operatortool.GetRunnerCapacity, fmt.Sprintf(`{"runner_id":%q}`, runner), `"recommendation"`},
				{operatortool.ListRunnerRouting, `{"limit":10}`, runner},
				{operatortool.Activity, fmt.Sprintf(`{"project_id":%q,"limit":10}`, project), `"organization_id":"` + org + `"`},
			} {
				t.Run(test.name, func(t *testing.T) {
					raw := call(t, test.name, test.arguments, org+"_catalog")
					if !strings.Contains(string(raw), test.marker) || strings.Contains(string(raw), map[string]string{"org_startup": "org_other_runner", "org_other": "org_startup_runner"}[org]) {
						t.Fatalf("unexpected tenant tool result: %s", raw)
					}
				})
			}
			for _, test := range []struct {
				name, input string
			}{
				{"rename_project", fmt.Sprintf(`{"name":%q,"expected_revision":"1"}`, org+" renamed")},
				{"update_project_integration", `{"expected_revision":"2","intake":"disabled","projection":"disabled","repository_enabled":false,"duplicate_challenge":false}`},
				{operatortool.UpdateProjectModelSelection, fmt.Sprintf(`{"expected_revision":"1","selection":{"normal_model":%q}}`, org+"_model")},
			} {
				t.Run(test.name, func(t *testing.T) {
					arguments := fmt.Sprintf(`{"project_id":%q,"request_id":%q,"input":%s}`, project, org+"_"+test.name, test.input)
					if test.name == "rename_project" {
						arguments = fmt.Sprintf(`{"organization_id":%q,"project_id":%q,"request_id":%q,"name":%q,"expected_revision":"1"}`, org, project, org+"_rename_project", org+" renamed")
					}
					var firstResult string
					for _, connection := range []string{org + "_catalog", org + "_reconnected"} {
						raw := call(t, test.name, arguments, connection)
						var action chatpkg.Action
						if err := json.Unmarshal(raw, &action); err != nil {
							t.Fatal(err)
						}
						if action.Status != chatpkg.ActionSucceeded || action.Result == "" || firstResult != "" && action.Result != firstResult {
							t.Fatalf("mutation did not succeed or replay: %s", raw)
						}
						firstResult = action.Result
					}
				})
			}
			t.Run("project_actions", func(t *testing.T) { assertEntryKeyProjectActions(t, call, org, project) })
			if err := f.services[org].config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
				record, err := r.ProjectRecord(t.Context(), tracker.ProjectID(project))
				if err != nil {
					return err
				}
				if record.Core.Name != org+" renamed" || record.Settings.DuplicateChallenge || record.Settings.Revision != 3 || record.Settings.ModelRevision != 2 {
					t.Fatalf("mutations not persisted exactly once: %+v", record)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var selection struct {
				Data cloudModelSelection `json:"data"`
			}
			if err := json.Unmarshal(call(t, operatortool.GetProjectModelSelection, fmt.Sprintf(`{"project_id":%q}`, project), org+"_catalog"), &selection); err != nil {
				t.Fatal(err)
			}
			if selection.Data.Effective.Model("normal") != org+"_model" || selection.Data.Revision != 2 {
				t.Fatalf("saved tenant model selection=%+v", selection.Data)
			}
		})
	}
}

func assertEntryKeyProjectActions(t *testing.T, call func(*testing.T, string, string, string) json.RawMessage, org, project string) {
	t.Helper()
	actionID := ""
	for _, test := range []struct {
		name, input, command string
		revision             int64
	}{
		{"create_project_action", `,"input":{"name":"Example action","command":"printf example"}`, "printf example", 1},
		{"patch_project_action", `,"input":{"expected_revision":1,"name":"Updated action","command":"printf updated"}`, "printf updated", 2},
		{"delete_project_action", "", "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector := ""
			if actionID != "" {
				selector = fmt.Sprintf(`,"action_id":%q`, actionID)
			}
			arguments := fmt.Sprintf(`{"project_id":%q,"request_id":%q%s%s}`, project, org+"_"+test.name, selector, test.input)
			var firstData string
			for _, connection := range []string{org + "_actions", org + "_actions_reconnected"} {
				raw := call(t, test.name, arguments, connection)
				var result struct {
					Status chatpkg.ActionStatus `json:"status"`
					Data   json.RawMessage      `json:"data"`
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != chatpkg.ActionSucceeded || len(result.Data) == 0 || firstData != "" && string(result.Data) != firstData {
					t.Fatalf("project action did not succeed or replay: %s", raw)
				}
				firstData = string(result.Data)
				if test.revision == 0 {
					if firstData != `{"deleted":true}` {
						t.Fatalf("action was not deleted: %s", raw)
					}
				} else {
					var action workspacesession.Action
					if err := json.Unmarshal(result.Data, &action); err != nil {
						t.Fatal(err)
					}
					if action.ID == "" || action.OrganizationID != org || action.ProjectID != project || action.Revision != test.revision || action.Command != test.command {
						t.Fatalf("unexpected saved action: %+v", action)
					}
					actionID = action.ID
				}
			}
			var listed struct {
				Data projectActionList `json:"data"`
			}
			if err := json.Unmarshal(call(t, "list_project_actions", fmt.Sprintf(`{"project_id":%q}`, project), org+"_actions"), &listed); err != nil {
				t.Fatal(err)
			}
			if test.revision == 0 && len(listed.Data.Items) != 0 || test.revision > 0 && (len(listed.Data.Items) != 1 || listed.Data.Items[0].ID != actionID || listed.Data.Items[0].Command != test.command) {
				t.Fatalf("project action changes not persisted: %+v", listed.Data)
			}
		})
	}
}
