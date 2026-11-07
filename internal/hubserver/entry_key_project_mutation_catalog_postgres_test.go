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
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeyProjectMutationCatalog(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
		org := cfg.Hosted.OrganizationID
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{ConfigPath: "detent.yaml", HasConfig: true, Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: true\n  required_status_checks: []\n"), WorkflowPath: "WORKFLOW.md", Workflow: []byte("Implement the assigned issue.\n")})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := config.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Workflow != nil {
		descriptor.Workflow.Revision = strings.Repeat("a", 40)
		descriptor = descriptor.WithID()
	}
	policyInput, err := json.Marshal(operatortool.PolicyApprovalInput{Policy: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_startup", "org_other"} {
		organization := tracker.OrganizationID(org)
		project := tracker.ProjectID(org + "_project")
		if err := f.services[org].config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: org + " example", Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			record.Settings.Repository, record.Settings.Intake = "example-org/example-project", "manual"
			if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: org + "_owner", ProjectID: project}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: project, CanWrite: true, ManageRunner: true})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			project := org + "_project"
			keyClaims := func(c *cloudassert.Claims) {
				c.Kind = cloudassert.KindKey
				c.ProviderOrganization = "provider_" + org
				c.Key = &apikey.KeyAuthority{ID: org + "_owner", CredentialBinding: strings.Repeat("f", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
			}
			catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, keyClaims)
			requireNativeStatus(t, catalog, http.StatusOK)
			var definitions []operatortool.Definition
			decodeHubResponse(t, catalog, &definitions)
			available := map[string]bool{}
			for _, definition := range definitions {
				available[definition.Name] = true
			}
			from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			observation := costTestObservation("cpu", from, from.Add(time.Hour))
			observation.ResourceID, observation.SourceID = org+"_sprite", org+"_meter"
			usageInput, err := json.Marshal(operatortool.SpriteUsageInput{Observations: []costObservation{observation}})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name, arguments, marker string
			}{
				{"set_project_icon", fmt.Sprintf(`{"organization_id":%q,"project_id":%q,"request_id":%q,"expected_revision":"2","icon":{"kind":"emoji","emoji":"🚀"}}`, org, project, org+"_icon"), `"emoji":"🚀"`},
				{"set_organization_logo", fmt.Sprintf(`{"organization_id":%q,"request_id":%q,"expected_revision":"1","remove":true}`, org, org+"_logo"), `"revision":"2"`},
				{"bind_native_repository", fmt.Sprintf(`{"project_id":%q,"request_id":%q,"input":{"expected_revision":"3","repository":"example-org/example-project","source":"runner_checkout"}}`, project, org+"_bind"), `"checkout_repository":"example-org/example-project"`},
				{"approve_project_policy", fmt.Sprintf(`{"project_id":%q,"request_id":%q,"input":%s}`, project, org+"_policy", policyInput), descriptor.ID},
				{"start_git_hub_import", fmt.Sprintf(`{"project_id":%q,"request_id":%q,"input":{"issue_number":1}}`, project, org+"_import"), `"status":"pending"`},
				{"import_sprite_usage", fmt.Sprintf(`{"project_id":%q,"request_id":%q,"input":%s}`, project, org+"_usage", usageInput), `"recorded":1`},
			} {
				t.Run(test.name, func(t *testing.T) {
					var first string
					for _, connection := range []string{org + "_mutations", org + "_mutations_reconnected"} {
						body := fmt.Sprintf(`{"name":%q,"connection_id":%q,"arguments":%s}`, test.name, connection, test.arguments)
						response := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
						requireNativeStatus(t, response, http.StatusOK)
						var action chatpkg.Action
						decodeHubResponse(t, response, &action)
						if !available[test.name] || action.Status != chatpkg.ActionSucceeded || !strings.Contains(action.Result, test.marker) || first != "" && action.Result != first {
							t.Fatalf("project mutation missing, failed, or replay changed: %s", response.Body)
						}
						first = action.Result
					}
				})
			}
			t.Run("monthly_usage_costs", func(t *testing.T) {
				body := fmt.Sprintf(`{"name":"monthly_usage_costs","connection_id":%q,"arguments":{"project_id":%q,"month":"2026-01"}}`, org+"_mutations", project)
				response := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
				requireNativeStatus(t, response, http.StatusOK)
				var report monthlyCostReport
				decodeHubResponse(t, response, &report)
				if !available["monthly_usage_costs"] || report.OrganizationID != org || report.SourceCount != 1 || len(report.Totals) != 1 || report.Totals[0].EstimatedMicros == nil || *report.Totals[0].EstimatedMicros != 20000 {
					t.Fatalf("saved monthly usage not returned: %+v", report)
				}
			})
			if err := f.services[org].config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
				record, err := r.ProjectRecord(t.Context(), tracker.ProjectID(project))
				if err != nil {
					return err
				}
				logo, err := r.OrganizationLogo(t.Context())
				if err != nil {
					return err
				}
				approval, err := r.ProjectPolicy(t.Context(), tracker.ProjectID(project))
				if err != nil {
					return err
				}
				job, err := r.(projectstore.GitHubImportReader).GitHubImportForIssue(t.Context(), tracker.ProjectID(project), 1)
				if err != nil {
					return err
				}
				if record.Settings.Icon.Emoji != "🚀" || record.Settings.CheckoutRepository != "example-org/example-project" || logo.Revision != 2 || logo.ImageKey != "" || approval.PolicyID != descriptor.ID || job.Revision != 1 || job.Status != "pending" {
					t.Fatalf("project mutations not saved: record=%+v logo=%+v approval=%+v import=%+v", record, logo, approval, job)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
