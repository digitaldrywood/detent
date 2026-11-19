package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresGitHubImportRequiresEntitlement(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	now := time.Now().UTC()
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, Clock: func() time.Time { return now }}}
	service.database.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: hostedplans.HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	service.registerNativeRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_entitled", "org_unentitled"} {
		seedStartupOrganization(t, repository, org)
		project := tracker.ProjectID("project_" + string(org))
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			if err != nil {
				return fmt.Errorf("project creation: %w", err)
			}
			record.Settings.Repository, record.Settings.Intake = "example-org/"+string(project), "manual"
			if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
				return fmt.Errorf("project settings: %w", err)
			}
			for _, role := range []string{"write", "read"} {
				id := string(org) + "_" + role
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: "Example member " + id, Hash: apikey.HashToken(id), Scope: "operator", KeyScope: role, NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
					return fmt.Errorf("credential: %w", err)
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: id, ProjectID: project}); err != nil {
					return fmt.Errorf("token grant: %w", err)
				}
			}
			features := []string{}
			if org == "org_entitled" {
				features = append(features, "github_integration")
			}
			plan := billingstore.PlanReference{ID: "plan_" + string(org), Version: 1}
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: org, PlanReference: plan, Name: "Example plan", Features: features, Allowances: map[string]int64{}}); err != nil {
				return fmt.Errorf("billing plan: %w", err)
			}
			return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: org, Base: plan}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name   string
		org    tracker.OrganizationID
		role   string
		status int
		code   string
	}{
		{"entitled writer", "org_entitled", "write", http.StatusOK, ""},
		{"unentitled writer", "org_unentitled", "write", http.StatusTooManyRequests, "allowance_exhausted"},
		{"entitled viewer", "org_entitled", "read", http.StatusForbidden, "access_denied"},
		{"unentitled viewer", "org_unentitled", "read", http.StatusForbidden, "access_denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			project := tracker.ProjectID("project_" + string(tt.org))
			number := 1
			if tt.role == "read" {
				number = 2
			}
			path := "/api/v2/organizations/" + string(tt.org) + "/projects/" + string(project) + "/imports"
			response := performHubAPIRequest(t, service, http.MethodPost, path, string(tt.org)+"_"+tt.role, map[string]any{"idempotency_key": tt.role, "issue_number": number})
			requireNativeStatus(t, response, tt.status)
			if tt.code != "" {
				var result hubapi.APIErrorResponse
				decodeHubResponse(t, response, &result)
				if result.Code != tt.code {
					t.Fatalf("code=%s want %s", result.Code, tt.code)
				}
				if tt.code == "allowance_exhausted" && !strings.Contains(response.Body.String(), "github_integration") {
					t.Fatal("missing refused feature")
				}
			}
			if err := repository.ReadTenant(t.Context(), tt.org, corestore.Serving, func(r tenantstore.Reader) error {
				job, err := r.(projectstore.GitHubImportReader).GitHubImportForIssue(t.Context(), project, int64(number))
				if tt.status != http.StatusOK {
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("refused import persisted: %+v, %v", job, err)
					}
					return nil
				}
				if err != nil {
					return err
				}
				var result GitHubImport
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					return err
				}
				if job.ID != result.ID || job.Status != "pending" || job.OrganizationID != tt.org {
					t.Fatalf("import=%+v response=%+v", job, result)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
