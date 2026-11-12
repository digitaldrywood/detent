package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLinkedImportUsesPostgresProjectRepository(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{TenantRepository: repository}}
	if err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org", Name: "Example", Slug: "example", Local: true, CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			record.Settings.Repository = "example-org/" + string(project)
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		})
		if err != nil {
			t.Fatalf("create import project: %v (%v)", err, errors.Unwrap(err))
		}
		for _, tt := range []struct{ name, repository, message string }{
			{"matching", "example-org/" + string(project), "transport is unavailable"},
			{"foreign", "example-org/foreign", linkedIssueRepositoryMismatch},
		} {
			t.Run(string(project)+"/"+tt.name, func(t *testing.T) {
				_, err := service.fetchLinkedSnapshot(t.Context(), nativeScope{organization: "org", project: project}, "https://github.com/"+tt.repository+"/issues/1")
				if err == nil || !strings.Contains(err.Error(), tt.message) {
					t.Fatalf("error=%v, want %q", err, tt.message)
				}
			})
		}
	}
}

func TestPostgresCheckoutBindingCommands(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	importer := &importFixtureBackend{}
	service := &Service{
		database: &database{postgres: repository, hostedOrganization: "org"},
		config: Config{
			TenantRepository:  repository,
			AccessRepository:  repository,
			RunDetails:        &memoryRunDetails{},
			ImportBackend:     importer,
			ReconcileInterval: time.Hour,
			now:               func() time.Time { return now },
			jitter:            func() float64 { return 0.5 },
		},
	}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org", Name: "Example", Slug: "example"})
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org", project: project, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		options := nativeCommandOptions{OperationID: projectOperationID(scope, "POST", "/integration/repository")}
		binding := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "binding"}, ExpectedRevision: 1, Repository: "example-org/" + string(project), Source: "runner_checkout"}
		first, err := service.bindNativeRepositoryCommand(t.Context(), scope, options, binding)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := service.bindNativeRepositoryCommand(t.Context(), scope, options, binding)
		if err != nil || string(replay) != string(first) {
			t.Fatalf("binding replay=%s error=%v", replay, err)
		}
		record, err := service.readProjectRecord(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if record.Settings.CheckoutRepository != binding.Repository || record.Settings.Revision != 2 {
			t.Fatalf("binding=%+v", record.Settings)
		}
		t.Run(string(project)+"/sync binding fence and rebind", func(t *testing.T) {
			checkoutNode := "R_checkout_" + string(project)
			checkoutSource := reconcileStep{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: checkoutNode, Owner: "example-org", Name: string(project), UpdatedAt: now}}}
			backend := &scriptedReconcileBackend{steps: []reconcileStep{checkoutSource, checkoutSource, checkoutSource}}
			service.config.ReconcileBackend = backend
			readOutcome := func() (projectstore.GitHubReconciliation, error) {
				t.Helper()
				var row projectstore.GitHubReconciliation
				err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.(projectstore.GitHubReconciliationReader).GitHubReconciliation(t.Context(), project)
					return err
				})
				return row, err
			}
			githubBinding := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "different-checkout"}, ExpectedRevision: 2, Repository: "example-org/sync-" + string(project)}
			githubOptions := nativeCommandOptions{OperationID: projectOperationID(scope, "POST", "/integration/repository")}
			_, err := service.bindNativeRepositoryCommand(t.Context(), scope, githubOptions, githubBinding)
			var refusal *nativeError
			if !errors.As(err, &refusal) || refusal.Code != "repository_already_bound" {
				t.Fatalf("different checkout binding was not refused: %v", err)
			}
			if row, err := readOutcome(); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("refused binding wrote an outcome: %+v, %v", row, err)
			}
			unchanged, err := service.readProjectRecord(t.Context(), scope)
			if err != nil || !reflect.DeepEqual(unchanged.Settings, record.Settings) {
				t.Fatalf("refused binding changed project settings: %+v, %v", unchanged.Settings, err)
			}

			githubBinding.IdempotencyKey, githubBinding.Repository = "sync-bind", binding.Repository
			response, err := service.bindNativeRepositoryCommand(t.Context(), scope, githubOptions, githubBinding)
			if err != nil {
				t.Fatal(err)
			}
			var result ProjectIntegration
			if err := json.Unmarshal(response, &result); err != nil || result.Repository != binding.Repository || result.CheckoutRepository != binding.Repository || result.Revision != 3 {
				t.Fatalf("sync binding=%s error=%v", response, err)
			}
			if row, err := readOutcome(); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("binding wrote an outcome before scheduled verification: %+v, %v", row, err)
			}
			replayed, err := service.bindNativeRepositoryCommand(t.Context(), scope, githubOptions, githubBinding)
			if err != nil || string(replayed) != string(response) {
				t.Fatalf("sync binding replay=%s error=%v", replayed, err)
			}

			if err := service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
				t.Fatal(err)
			}
			outcome, err := readOutcome()
			if err != nil || outcome.Repository != binding.Repository || outcome.NodeID != checkoutNode || outcome.LastSuccessfulSweepAt == nil || !outcome.LastSuccessfulSweepAt.Equal(now) || outcome.LastFailureAt != nil {
				t.Fatalf("scheduled checkout outcome=%+v error=%v", outcome, err)
			}

			githubBinding.IdempotencyKey, githubBinding.ExpectedRevision = "sync-rebind", 3
			response, err = service.bindNativeRepositoryCommand(t.Context(), scope, githubOptions, githubBinding)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(response, &result); err != nil || result.Repository != binding.Repository || result.CheckoutRepository != binding.Repository || result.Revision != 4 {
				t.Fatalf("sync rebind=%s error=%v", response, err)
			}
			if row, err := readOutcome(); err != nil || !reflect.DeepEqual(row, outcome) {
				t.Fatalf("rebind changed the current checkout outcome: %+v, %v", row, err)
			}

			githubBinding.IdempotencyKey, githubBinding.ExpectedRevision, githubBinding.Repository = "other-sync", 4, "example-org/other"
			_, err = service.bindNativeRepositoryCommand(t.Context(), scope, githubOptions, githubBinding)
			if !errors.As(err, &refusal) || refusal.Code != "repository_already_bound" {
				t.Fatalf("different existing sync binding was not refused: %v", err)
			}
			requests := backend.Requests()
			if len(requests) != 3 || requests[0].Repository.Name != string(project) || requests[1].Repository.Name != string(project) || requests[2].Repository.Name != string(project) || len(importer.requests) != 0 {
				t.Fatalf("provider requests did not follow binding and checkout identity: %+v", requests)
			}
		})
	}
}

func TestPostgresGitHubImportRequiresEntitlement(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC()
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
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
				var result apiErrorResponse
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
