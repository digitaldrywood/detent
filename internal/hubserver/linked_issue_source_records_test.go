package hubserver

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
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
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	organization := tracker.OrganizationID("org_security")
	projectID := tracker.ProjectID("prj_security")
	project := projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true}, States: append(nativeFixtureStates(), tracker.NativeState{Name: "Cancelled", Terminal: true, Transitions: []string{"Cancelled"}})}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := time.Now().UTC()
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "example-organization", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider", BootstrapSubject: "user_owner", Deployment: "origin", PublicURL: "https://cloud.example"}); err != nil {
			return err
		}
		free := hostedplans.NormalizePlan(hostedplans.PilotPlans().Plans[0])
		plan := billingstore.Plan{OrganizationID: organization, PlanReference: billingstore.PlanReference{ID: free.ID, Version: free.Version}, Name: free.Name, MonthlyUSDCents: free.MonthlyUSDCents, Allowances: free.Allowances, Features: free.Features}
		if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: plan.PlanReference}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.Hosted.PublicURL = "https://cloud.example"
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
	f.service.outbox.stop()
	backend := &recordingOutboxBackend{}
	f.service.config.OutboxBackend = backend
	secondURL := "https://github.com/example/repository/issues/13"
	secondResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "second-linked-create"}, GitHubIssueURL: secondURL, State: "Todo"})
	requireNativeStatus(t, secondResponse, http.StatusOK)
	var second tracker.NativeIssue
	decodeHubResponse(t, secondResponse, &second)
	secondSnapshot := snapshot
	secondSnapshot.URL, secondSnapshot.Provenance.ExternalID = secondURL, "node_13"
	secondResponse = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(second.WorkItemID)+"/source-intake", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "second-linked-intake"}, Snapshot: secondSnapshot})
	requireNativeStatus(t, secondResponse, http.StatusOK)
	decodeHubResponse(t, secondResponse, &second)
	for index, selected := range []tracker.NativeIssue{refreshed, second} {
		path := f.base + "/work-items/" + string(selected.WorkItemID)
		transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "cancel-linked-" + strconv.Itoa(index)}, ExpectedRevision: selected.Revision, State: "Cancelled", Reason: "user_requested"}
		cancelled := f.request(t, owner, http.MethodPost, path+"/workflow", transition)
		requireNativeStatus(t, cancelled, http.StatusOK)
		var current tracker.NativeIssue
		decodeHubResponse(t, cancelled, &current)
		if current.State != "Cancelled" || !current.Terminal {
			t.Fatalf("linked cancellation did not commit: %+v", current)
		}
		replayed := f.request(t, owner, http.MethodPost, path+"/workflow", transition)
		requireNativeStatus(t, replayed, http.StatusOK)
		if replayed.Body.String() != cancelled.Body.String() {
			t.Fatal("linked cancellation replay changed its response")
		}
		transition.IdempotencyKey += "-unchanged"
		transition.ExpectedRevision = current.Revision
		requireNativeStatus(t, f.request(t, owner, http.MethodPost, path+"/workflow", transition), http.StatusOK)
		var key string
		if err := admin.QueryRowContext(t.Context(), "SELECT command_key FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, projectID, selected.WorkItemID).Scan(&key); err != nil {
			t.Fatal(err)
		}
		var outbound projectstore.GitHubOutbound
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			outbound, err = r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), key)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := eventstore.NewContent(f.service.runDetailStore()).Receipt(t.Context(), outbound.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var desired WorkpadDesired
		if err := json.Unmarshal(raw, &desired); err != nil {
			t.Fatal(err)
		}
		expected := "Cancelled in Detent: https://cloud.example/example-organization/example-project/issues/" + strconv.Itoa(selected.Number)
		if desired.Body != expected || !desired.CloseSource || desired.StateReason != "not_planned" || outbound.Repository != "example/repository" || outbound.IssueNumber != int64(12+index) {
			t.Fatalf("linked cancellation summary/routing = %+v, %+v", desired, outbound)
		}
		requireNativeStatus(t, f.request(t, owner, http.MethodPost, path+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "private-linked-" + strconv.Itoa(index)}, Body: "Private Cloud discussion"}), http.StatusOK)
		if worked, err := f.service.ProcessOutbox(t.Context()); err != nil || !worked {
			t.Fatalf("linked cancellation did not publish: %t, %v", worked, err)
		}
	}
	var count int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", organization, projectID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("replay/private discussion queued duplicate cancellation writes: %d, %v", count, err)
	}
	if backend.effects != 2 {
		t.Fatalf("linked cancellation effects = %d, want 2", backend.effects)
	}
}
