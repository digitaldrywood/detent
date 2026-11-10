package hubserver

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLinkedIssueIntakeRequiresSelectedAccessAuthority(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: &memoryRunDetails{}})
	path := "/api/v2/organizations/org_example/projects/prj_example/work-items/wi_example/source-intake"
	request := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "intake"}}
	response := performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusUnauthorized)
}

func TestLinkedIssueSourceRecordUsesSelectedOwnersWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	organization := tracker.OrganizationID("org_security")
	projectID := tracker.ProjectID("prj_security")
	project := projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, States: nativeFixtureStates()}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := time.Now().UTC()
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider"}); err != nil {
			return err
		}
		free := normalizeHostedPlan(pilotHostedPlans().Plans[0])
		plan := billingstore.Plan{OrganizationID: organization, PlanReference: billingstore.PlanReference{ID: free.ID, Version: free.Version}, Name: free.Name, MonthlyUSDCents: free.MonthlyUSDCents, Allowances: free.Allowances, Features: free.Features}
		if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: plan.PlanReference}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.BillingRepository = repository
		cfg.TenantRepository = repository
		cfg.RunDetails = &memoryRunDetails{}
	})
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		created, err := w.ProjectRecord(t.Context(), projectID)
		if err != nil {
			return err
		}
		created.Core.Name, created.Core.Slug, created.Core.RequireDependencies = project.Core.Name, project.Core.Slug, project.Core.RequireDependencies
		created.States = project.States
		created.Settings.RepositoryEnabled = true
		created.Settings.Repository = "example/repository"
		if _, err := w.UpdateProjectRecord(t.Context(), created); err != nil {
			return err
		}
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: projectID, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: projectID})
	}); err != nil {
		t.Fatal(err)
	}
	const sourceURL = "https://github.com/example/repository/issues/12"
	created := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "selected-source-create"}, GitHubIssueURL: sourceURL, State: "Todo"})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	now := f.service.config.now()
	snapshot := tracker.GitHubIssueSnapshot{URL: sourceURL, Title: "Imported title", Body: "Imported body", Provenance: tracker.Provenance{Provider: "github", ExternalID: "node_12", AuthorID: "example-user", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
	request := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "selected-source-intake"}, Snapshot: snapshot}
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/source-intake", request)
	requireNativeStatus(t, response, http.StatusOK)
	var refreshed tracker.NativeIssue
	decodeHubResponse(t, response, &refreshed)
	if refreshed.Title != snapshot.Title || refreshed.Body != snapshot.Body || refreshed.LinkedSource == nil || refreshed.LinkedSource.Status != "complete" {
		t.Fatalf("source intake did not complete from selected owners: %+v", refreshed)
	}
	for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
		if !slices.Contains(refreshed.OmittedFields, field) {
			t.Fatalf("source intake response did not omit %s", field)
		}
	}
	replay := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/source-intake", request)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != response.Body.String() {
		t.Fatal("source intake replay changed its committed response")
	}
}
