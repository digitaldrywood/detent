package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubclient/recovery"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOpenCreatesListsAndClaimsNativeIssue(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	var humanWorkflowClock atomic.Int64
	cfg.now = func() time.Time {
		if value := humanWorkflowClock.Load(); value != 0 {
			return time.Unix(0, value).UTC()
		}
		return time.Now().UTC()
	}
	cfg.Postgres.Pool.MaxConnections = 4
	cfg.Postgres.Pool.ServingConnections = 2
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Workspace = &WorkspaceConfig{Enabled: true}
	provider := cfg.Hosted.Provider
	if _, err := provider.CreateMembership(t.Context(), "user_owner", "provider_org_startup", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateMembership(t.Context(), "user_reporter", "provider_org_startup", "reporter"); err != nil {
		t.Fatal(err)
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES('org_startup','collaboration_bytes',0) ON CONFLICT (organization_id,name) DO NOTHING`); err != nil {
		t.Fatalf("seed PostgreSQL collaboration byte counter: %v", err)
	}
	const ownerToken = "example_owner_token"
	ownerHash := apikey.HashToken(ownerToken)
	const reporterToken = "example_reporter_token"
	reporterHash := apikey.HashToken(reporterToken)
	now := time.Now().UTC()
	cursorKey := bytes.Repeat([]byte{0x5a}, 32)
	serverID := "hub_" + strings.Repeat("a", 32)
	cursorEnvelope, err := cfg.SecretKeys.Seal(cursorKey, accessstore.SecretAAD("org_startup", "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.postgres.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: "org_startup", ID: serverID, CursorKey: cursorKey}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: "credential_owner", Name: "Example owner", Hash: ownerHash, Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_owner", MembershipID: "membership_user_owner", KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_startup", UserID: "user_owner", Email: "owner@example.test", MembershipID: "membership_user_owner", Role: "owner", Active: true, PrincipalID: "credential_owner", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		projectID := tracker.ProjectID("project_example")
		states := append(humanWorkflowFixtureStates(), tracker.NativeState{Name: "Triage", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native"}, States: states}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "user_owner", ProjectID: projectID, CanWrite: true, ManageRunner: true}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_owner", ProjectID: projectID}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: "credential_reporter", Name: "Example reporter", Hash: reporterHash, Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_reporter", MembershipID: "membership_user_reporter", KeyScope: "write", ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_startup", UserID: "user_reporter", Email: "reporter@example.test", MembershipID: "membership_user_reporter", Role: "reporter", Active: true, PrincipalID: "credential_reporter", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "user_reporter", ProjectID: projectID, CanWrite: true}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_reporter", ProjectID: projectID}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Envelope: cursorEnvelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursorEnvelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open PostgreSQL Hub = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.db != nil || service.config.TenantRepository != service.database.postgres || service.config.AccessRepository != service.database.postgres {
		t.Fatal("Open did not bind native routes to PostgreSQL")
	}
	ownerCredential, ownerStatus, ownerErr := service.authenticateAccessHash(t.Context(), ownerHash, "", "")
	if ownerErr != nil || ownerStatus != http.StatusOK || ownerCredential.HostedRole != "owner" || ownerCredential.Hosted == nil {
		t.Fatalf("authenticate seeded owner = status %d, role %q, error %v", ownerStatus, ownerCredential.HostedRole, ownerErr)
	}
	reporterCredential, reporterStatus, reporterErr := service.authenticateAccessHash(t.Context(), reporterHash, "", "")
	if reporterErr != nil || reporterStatus != http.StatusOK || reporterCredential.HostedRole != "reporter" || reporterCredential.Hosted == nil {
		t.Fatalf("authenticate seeded reporter = status %d, role %q, error %v", reporterStatus, reporterCredential.HostedRole, reporterErr)
	}

	project := tracker.NativeProject{ID: "project_example", OrganizationID: "org_startup"}
	policy := hubTestPolicy()
	policyRevision, err := eventstore.NewContent(service.config.RunDetails).PrepareProjectPolicy(t.Context(), "org_startup", policy)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = project.ID
	policyRevision.ApprovedBy = "credential_owner"
	policyRevision.ApprovedAt = service.config.now().UTC()
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}

	itemsPath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/work-items"
	recoveryBody := "## Acceptance criteria\n" + strings.Repeat("Recovery fragment. ", 7000) + "\n## Must not break\nKeep tenant isolation.\n## How we know it worked\nRecovery loads.\n"
	createResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, ownerToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue-create"}, Title: "Example issue", Body: recoveryBody, State: "Todo"})
	requireNativeStatus(t, createResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, createResponse, &issue)
	recoveryPath := itemsPath + "/" + url.PathEscape(string(issue.WorkItemID)) + "/source-recovery"
	recoveryResponse := performHubAPIRequest(t, service, http.MethodGet, recoveryPath, ownerToken, nil)
	requireNativeStatus(t, recoveryResponse, http.StatusOK)
	var sourceRecovery tracker.NativeSourceRecovery
	decodeHubResponse(t, recoveryResponse, &sourceRecovery)
	if sourceRecovery.WorkItemID != issue.WorkItemID || sourceRecovery.Revision != issue.Revision || sourceRecovery.Destinations == nil {
		t.Fatalf("PostgreSQL source recovery = %+v", sourceRecovery)
	}
	unauthorizedRecovery := performHubAPIRequest(t, service, http.MethodGet, recoveryPath, "example-invalid-token", nil)
	requireNativeStatus(t, unauthorizedRecovery, http.StatusUnauthorized)
	commentBody := strings.Repeat("discussion fragment. ", 2600)
	for index := 0; index < 22; index++ {
		comment := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("recovery-fragment-%d", index)}, Body: commentBody}
		commentPath := itemsPath + "/" + url.PathEscape(string(issue.WorkItemID)) + "/comments"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commentPath, ownerToken, comment), http.StatusOK)
	}
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatalf("refresh PostgreSQL board projection: %v", err)
	}
	listResponse := performHubAPIRequest(t, service, http.MethodGet, itemsPath, ownerToken, nil)
	requireNativeStatus(t, listResponse, http.StatusOK)
	var page tracker.NativeIssuePage
	decodeHubResponse(t, listResponse, &page)
	if len(page.Items) != 1 || page.Items[0].WorkItemID != issue.WorkItemID {
		t.Fatalf("PostgreSQL issue list = %+v, created %+v", page.Items, issue)
	}
	reporterCreateResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, reporterToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reporter-issue-create"}, Title: "Reporter issue", Body: reporterBugSections, Type: "bug", State: "Triage"})
	requireNativeStatus(t, reporterCreateResponse, http.StatusOK)
	var reporterIssue tracker.NativeIssue
	decodeHubResponse(t, reporterCreateResponse, &reporterIssue)
	secondBody := "## Acceptance criteria\nThe second work item remains isolated.\n## Must not break\nDo not expose the first work item.\n## How we know it worked\nSource recovery returns the second ID.\n"
	secondIssueResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, ownerToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "second-issue-create"}, Title: "Example second issue", Body: secondBody, State: "Todo"})
	requireNativeStatus(t, secondIssueResponse, http.StatusOK)
	var secondIssue tracker.NativeIssue
	decodeHubResponse(t, secondIssueResponse, &secondIssue)
	secondRecoveryResponse := performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+url.PathEscape(string(secondIssue.WorkItemID))+"/source-recovery", ownerToken, nil)
	requireNativeStatus(t, secondRecoveryResponse, http.StatusOK)
	var secondRecovery tracker.NativeSourceRecovery
	decodeHubResponse(t, secondRecoveryResponse, &secondRecovery)
	if secondRecovery.WorkItemID != secondIssue.WorkItemID || secondRecovery.WorkItemID == sourceRecovery.WorkItemID {
		t.Fatalf("PostgreSQL source recovery crossed work items: first=%+v second=%+v", sourceRecovery, secondRecovery)
	}
	workspacePath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/workspaces"
	workspaceResponse := performHubAPIRequest(t, service, http.MethodPost, workspacePath, ownerToken, map[string]any{})
	requireNativeStatus(t, workspaceResponse, http.StatusServiceUnavailable)
	var workspaceError map[string]any
	decodeHubResponse(t, workspaceResponse, &workspaceError)
	if workspaceError["code"] != "storage_owner_unavailable" {
		t.Fatalf("PostgreSQL workspace response = %+v", workspaceError)
	}

	workerToken := "example_runner_token"
	workerHash := apikey.HashToken(workerToken)
	expires := now.Add(time.Hour)
	capacity := providercapacity.Report{Provider: "example-provider", Backend: "example-backend", AccountAlias: "example-account", SharedAccountAlias: "example-shared-account", Billing: "subscription", Models: []string{"example-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: "credential_runner", Name: "Example runner", Hash: workerHash, Fingerprint: tokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_runner", ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_startup", ID: "enrollment_runner", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "credential_owner"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", "runner_example", "machine_example", now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: "machine_example", Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_startup", RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_runner", EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		if err := w.UpdateRunnerCapacityConfig(t.Context(), "runner_example", runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now}); err != nil {
			return err
		}
		if err := w.ReplaceRunnerProviderReports(t.Context(), "runner_example", []providercapacity.Report{capacity}); err != nil {
			return err
		}
		if err := w.ReplaceRunnerLocalChecks(t.Context(), "runner_example", project.ID, runnerauth.LocalChecks{Checkout: "passed", Configuration: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}, ObservedAt: now, CheckoutAt: now, ConfigurationAt: now, DoctorAt: now, ProviderAt: now}); err != nil {
			return err
		}
		return w.SaveProjectOnboardingProgress(t.Context(), project.ID, onboarding.Progress{Revision: 1, Repository: "existing", Doctor: true, Provider: true, Artifacts: "local", UpdatedAt: formatHubTime(now)}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	setupResponse := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/org_startup/projects/"+url.PathEscape(string(project.ID))+"/onboarding", workerToken, nil)
	requireNativeStatus(t, setupResponse, http.StatusOK)
	var setup onboarding.Project
	decodeHubResponse(t, setupResponse, &setup)
	if setup.Progress.Revision != 1 || setup.Progress.Repository != "existing" || !setup.Progress.Doctor || !setup.Progress.Provider || setup.Progress.Artifacts != "local" || setup.Progress.UpdatedAt == "" {
		t.Fatalf("PostgreSQL worker onboarding progress = %+v", setup.Progress)
	}
	browserSetupResponse := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/org_startup/projects/"+url.PathEscape(string(project.ID))+"/onboarding", ownerToken, nil)
	requireNativeStatus(t, browserSetupResponse, http.StatusOK)
	var browserSetup onboarding.Project
	decodeHubResponse(t, browserSetupResponse, &browserSetup)
	if len(browserSetup.Runners) != 1 || browserSetup.Runners[0].Runner.RunnerID != "runner_example" || browserSetup.Runners[0].Runner.ConnectionHealth != "online" || browserSetup.Runners[0].LocalChecks == nil || !browserSetup.Runners[0].LocalChecks.Passed() || !slices.Equal(browserSetup.Runners[0].LocalChecks.ProviderKinds, []string{"codex"}) || browserSetup.Policy == nil || browserSetup.Policy.Policy.ID != policy.ID {
		t.Fatalf("typed browser onboarding lost live runner/policy proofs: %+v", browserSetup)
	}
	capabilitiesRequest := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/capabilities", workerToken, nil)
	requireNativeStatus(t, capabilitiesRequest, http.StatusOK)
	var capabilities nativeCapabilitiesResponse
	decodeHubResponse(t, capabilitiesRequest, &capabilities)
	if capabilities.ServerID != serverID || capabilities.Version != service.config.Version || !slices.Contains(capabilities.ProtocolMajors, 2) {
		t.Fatalf("PostgreSQL capabilities = %+v", capabilities)
	}
	claim := tracker.NativeClaim{LandingCapacity: 1, PolicyID: policy.ID, WorkItemID: issue.WorkItemID, MachineID: "machine_example", SessionID: "session_example", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability, tracker.NativeProviderCapacityCapability}, ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: "code", Backend: capacity.Backend, Model: capacity.Models[0]}}}}
	claimResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/org_startup/projects/"+url.PathEscape(string(project.ID))+"/claims", workerToken, claim)
	requireNativeStatus(t, claimResponse, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimResponse, &lease)
	if lease.WorkItemID != issue.WorkItemID || lease.MachineID != "machine_example" || lease.ID == "" {
		t.Fatalf("PostgreSQL claim response = %+v", lease)
	}
	bodyFragmentSeen, fragmentRequests := false, 0
	recoveryReader := postgresRouteRecoveryReader{service: service, token: workerToken, organization: "org_startup", project: project.ID, bodyFragmentSeen: &bodyFragmentSeen, fragmentRequests: &fragmentRequests}
	recovered, err := recovery.Load(t.Context(), recoveryReader, "org_startup", project.ID, issue.WorkItemID)
	if err != nil {
		t.Fatalf("PostgreSQL recovery load: %v", err)
	}
	if !bodyFragmentSeen || fragmentRequests == 0 {
		t.Fatalf("PostgreSQL recovery handler did not serve issue body fragments: seen=%t requests=%d", bodyFragmentSeen, fragmentRequests)
	}
	if recovered.Issue.OrganizationID != "org_startup" || recovered.Issue.ProjectID != project.ID || recovered.Issue.WorkItemID != issue.WorkItemID || recovered.Issue.Body != recoveryBody {
		t.Fatalf("PostgreSQL recovery detail = %+v", recovered.Issue)
	}
	if recovered.SourceVersionID != "" || recovered.InstanceSuccesses != nil || recovered.Read == nil || recovered.Read.IssueBody == nil || !slices.Contains(recovered.Read.Omitted, "source_version_id") || !slices.Contains(recovered.Read.Omitted, "instance_successes") {
		t.Fatalf("PostgreSQL recovery unsupported fields were not explicitly omitted: %+v", recovered)
	}
	var storedLease leasestore.Lease
	if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		storedLease, err = r.Lease(t.Context(), string(lease.ID))
		return err
	}); err != nil || storedLease.LeaseID != string(lease.ID) {
		t.Fatalf("PostgreSQL claimed lease = %+v, error %v", storedLease, err)
	}
	validateResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/org_startup/projects/"+url.PathEscape(string(project.ID))+"/leases/"+url.PathEscape(string(lease.ID))+"/validate", workerToken, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken})
	requireNativeStatus(t, validateResponse, http.StatusOK)
	var validatedRunner runnerauth.Runner
	decodeHubResponse(t, validateResponse, &validatedRunner)
	if validatedRunner.RunnerID != "runner_example" || validatedRunner.MachineID != "machine_example" || validatedRunner.OrganizationID != "org_startup" {
		t.Fatalf("PostgreSQL validated runner = %+v", validatedRunner)
	}
	workerCredential, status, err := service.authenticateAccessHash(t.Context(), workerHash, "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate diagnostic runner: status=%d error=%v", status, err)
	}
	diagnosticAt := now.Add(time.Second)
	diagnosticView := runnerauth.ProjectConfiguration{ProjectID: string(project.ID), ObservedAt: diagnosticAt, Diagnostics: &runnerauth.ProjectDiagnostics{
		ObservedAt: diagnosticAt, RuntimeObservedAt: diagnosticAt, Source: "runner_runtime_and_durable_attempt_owners", Truncated: true,
		RunningBuild: &runnerauth.BuildEvidence{Version: "v0.177.4", Commit: "abcdef1234567", Source: "development", OS: "darwin", Architecture: "arm64", ObservedAt: diagnosticAt},
		Records:      []runnerauth.DiagnosticAttempt{{Key: "local:17", IssueID: string(issue.WorkItemID), LocalAttemptID: 17, NativeAttemptID: "attempt_example", Generation: 2, Fence: 3, Stage: "implementing", Membership: []string{"running", "native_claim", "durable_active"}, HostOperation: "append.run.observed", OperationPending: true, OperationStartedAt: diagnosticAt.Add(-time.Second), OperationObservedAt: diagnosticAt, LatestErrorCode: "invalid_request", LatestError: "private_customer_payload", DeferredAt: diagnosticAt.Add(-2 * time.Second), RetryAt: diagnosticAt.Add(time.Second), RetryAttempt: 2, RecoveryOwner: "orchestrator.deferred_completion", LeaseRenewedAt: diagnosticAt, LeaseExpiresAt: diagnosticAt.Add(time.Minute), Unavailable: map[string]string{"native_attempt_id": "unrecorded"}}},
		Admissions:   []runnerauth.DiagnosticAdmission{{IssueID: string(issue.WorkItemID), WorkItemRevision: int64(issue.Revision), ObservedAt: diagnosticAt, Result: "skipped", Predicate: "project_capacity_full", RepositoryReservations: []string{"repository_example"}, HostLandingOccupancy: 1, ProviderRequirement: &providercapacity.Requirement{Role: "code", Backend: capacity.Backend, Model: capacity.Models[0]}, Unavailable: map[string]string{}}},
		Counts:       map[string]int{"running": 1, "native_claim": 1}, Unavailable: map[string]string{"running_build": "unavailable"},
	}}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		writer, ok := w.(runnerstore.Writer)
		if !ok {
			return fmt.Errorf("runner diagnostic writer unavailable")
		}
		return storeRunnerProjectObservationTenant(t.Context(), nativeScope{organization: "org_startup", project: project.ID, credential: workerCredential}, diagnosticView, diagnosticAt.Add(time.Second), writer)
	}); err != nil {
		t.Fatalf("store typed PostgreSQL runner diagnostics: %v", err)
	}
	diagnosticsPath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/runner-diagnostics?runner_id=runner_example&limit=1"
	diagnosticsResponse := performHubAPIRequest(t, service, http.MethodGet, diagnosticsPath, workerToken, nil)
	requireNativeStatus(t, diagnosticsResponse, http.StatusOK)
	var diagnosticsPage runnerauth.DiagnosticPage
	decodeHubResponse(t, diagnosticsResponse, &diagnosticsPage)
	if diagnosticsPage.Status != "available" || diagnosticsPage.ReceivedAt != diagnosticAt.Add(time.Second) || diagnosticsPage.RunningBuild == nil || diagnosticsPage.RunningBuild.Commit != "abcdef1234567" || diagnosticsPage.Counts["running"] != 1 || !diagnosticsPage.Truncated || len(diagnosticsPage.Admissions) != 1 || diagnosticsPage.Admissions[0].ProviderRequirement == nil || diagnosticsPage.Admissions[0].ProviderRequirement.Model != capacity.Models[0] || diagnosticsPage.NextCursor == "" {
		t.Fatalf("PostgreSQL typed diagnostic page = %+v", diagnosticsPage)
	}
	secondDiagnosticsPath := diagnosticsPath + "&cursor=" + url.QueryEscape(diagnosticsPage.NextCursor)
	secondDiagnosticsResponse := performHubAPIRequest(t, service, http.MethodGet, secondDiagnosticsPath, workerToken, nil)
	requireNativeStatus(t, secondDiagnosticsResponse, http.StatusOK)
	decodeHubResponse(t, secondDiagnosticsResponse, &diagnosticsPage)
	if len(diagnosticsPage.Records) != 1 || diagnosticsPage.Records[0].OperationStartedAt != diagnosticAt.Add(-time.Second) || diagnosticsPage.Records[0].OperationObservedAt != diagnosticAt || diagnosticsPage.Records[0].LatestError != "[private error text omitted]" || diagnosticsPage.Records[0].RetryAttempt != 2 || diagnosticsPage.Records[0].LeaseExpiresAt != diagnosticAt.Add(time.Minute) || diagnosticsPage.Records[0].Unavailable["native_attempt_id"] != "unrecorded" {
		t.Fatalf("PostgreSQL typed diagnostic attempt page = %+v", diagnosticsPage)
	}
	updatedDiagnosticAt := diagnosticAt.Add(2 * time.Second)
	diagnosticView.ObservedAt = updatedDiagnosticAt
	diagnosticView.Diagnostics.ObservedAt = updatedDiagnosticAt
	diagnosticView.Diagnostics.RuntimeObservedAt = updatedDiagnosticAt
	diagnosticView.Diagnostics.Records = append(diagnosticView.Diagnostics.Records, runnerauth.DiagnosticAttempt{Key: "local:18", IssueID: string(reporterIssue.WorkItemID), LocalAttemptID: 18, NativeAttemptID: "attempt_reporter", Generation: 1, Fence: 1, Stage: "implementing", Membership: []string{"running"}, LatestErrorCode: "invalid_request", LatestError: "reporter_private_payload"})
	diagnosticView.Diagnostics.Admissions = append(diagnosticView.Diagnostics.Admissions, runnerauth.DiagnosticAdmission{IssueID: string(reporterIssue.WorkItemID), WorkItemRevision: int64(reporterIssue.Revision), ObservedAt: updatedDiagnosticAt, Result: "skipped", Predicate: "project_capacity_full", RepositoryReservations: []string{"reporter_repository"}, HostLandingOccupancy: 1, Unavailable: map[string]string{}})
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		writer, ok := w.(runnerstore.Writer)
		if !ok {
			return fmt.Errorf("runner diagnostic writer unavailable")
		}
		return storeRunnerProjectObservationTenant(t.Context(), nativeScope{organization: "org_startup", project: project.ID, credential: workerCredential}, diagnosticView, updatedDiagnosticAt.Add(time.Second), writer)
	}); err != nil {
		t.Fatalf("store reporter PostgreSQL runner diagnostics: %v", err)
	}
	reporterDiagnosticsPath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/runner-diagnostics?runner_id=runner_example&limit=10"
	reporterDiagnosticsResponse := performHubAPIRequest(t, service, http.MethodGet, reporterDiagnosticsPath, reporterToken, nil)
	requireNativeStatus(t, reporterDiagnosticsResponse, http.StatusOK)
	diagnosticsPage = runnerauth.DiagnosticPage{}
	decodeHubResponse(t, reporterDiagnosticsResponse, &diagnosticsPage)
	if diagnosticsPage.Counts != nil || len(diagnosticsPage.Records) != 1 || diagnosticsPage.Records[0].IssueID != string(reporterIssue.WorkItemID) || diagnosticsPage.Records[0].LatestError != "[private error text omitted]" || len(diagnosticsPage.Admissions) != 1 || diagnosticsPage.Admissions[0].IssueID != string(reporterIssue.WorkItemID) || len(diagnosticsPage.Admissions[0].RepositoryReservations) != 0 {
		t.Fatalf("PostgreSQL reporter diagnostic page exposed another issue or private fields: %+v", diagnosticsPage)
	}
	hiddenReporterResponse := performHubAPIRequest(t, service, http.MethodGet, reporterDiagnosticsPath+"&issue_id="+url.QueryEscape(string(issue.WorkItemID)), reporterToken, nil)
	requireNativeCode(t, hiddenReporterResponse, http.StatusNotFound, "not_found")
	leasePath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/leases/" + url.PathEscape(string(lease.ID))
	renewResponse := performHubAPIRequest(t, service, http.MethodPost, leasePath+"/renew", workerToken, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 300})
	requireNativeStatus(t, renewResponse, http.StatusOK)
	var renewedLease tracker.NativeLease
	decodeHubResponse(t, renewResponse, &renewedLease)
	if renewedLease.ID != lease.ID || renewedLease.FencingToken != lease.FencingToken {
		t.Fatalf("PostgreSQL renewed lease = %+v", renewedLease)
	}
	releaseResponse := performHubAPIRequest(t, service, http.MethodPost, leasePath+"/release", workerToken, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"})
	requireNativeStatus(t, releaseResponse, http.StatusNoContent)
	var releasedLease leasestore.Lease
	if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		releasedLease, err = r.Lease(t.Context(), string(lease.ID))
		return err
	}); err != nil {
		t.Fatalf("read PostgreSQL released lease: %v", err)
	}
	if releasedLease.ReleasedAt == nil || releasedLease.ReleaseReason != "released" {
		t.Fatalf("PostgreSQL released lease = %+v", releasedLease)
	}

	t.Run("human workflow after success", func(t *testing.T) {
		verifyPostgresHumanWorkflow(t, service, admin, project, claim, ownerToken, reporterToken, workerToken, &humanWorkflowClock)
	})

	seedAttempt := func(session string, checkpoint *tracker.NativeCheckpoint, retainLease ...bool) tracker.NativeRunData {
		t.Helper()
		request := claim
		request.SessionID = session
		claimResponse := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/org_startup/projects/"+url.PathEscape(string(project.ID))+"/claims", workerToken, request)
		requireNativeStatus(t, claimResponse, http.StatusOK)
		var attemptLease tracker.NativeLease
		decodeHubResponse(t, claimResponse, &attemptLease)
		attemptID, runID := newNativeID("attempt"), newNativeID("run")
		data := tracker.NativeRunData{AttemptID: attemptID, RunID: runID, LeaseID: attemptLease.ID, FencingToken: attemptLease.FencingToken, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example-backend", Model: "example-model"}}
		path := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/work-items/" + url.PathEscape(string(issue.WorkItemID)) + "/events"
		started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start_" + attemptID}, Type: "run.started", SchemaVersion: 1, Data: data}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, started), http.StatusOK)
		if checkpoint != nil {
			data.Sequence = 2
			data.Handoff = checkpoint
			checkpointEvent := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "checkpoint_" + attemptID}, Type: "run.checkpointed", SchemaVersion: 1, Data: data}
			checkpointResponse := performHubAPIRequest(t, service, http.MethodPost, path, workerToken, checkpointEvent)
			if checkpointResponse.Code != http.StatusOK {
				t.Fatalf("checkpoint handoff %+v: %s", checkpoint, checkpointResponse.Body.String())
			}
		}
		data.Sequence++
		data.Handoff = nil
		data.Outcome = "failed"
		finished := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "finish_" + attemptID}, Type: "run.finished", SchemaVersion: 1, Data: data}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, finished), http.StatusOK)
		if len(retainLease) == 0 || !retainLease[0] {
			leasePath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/leases/" + url.PathEscape(string(attemptLease.ID))
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, leasePath+"/release", workerToken, tracker.NativeLeaseMutation{FencingToken: attemptLease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		}
		return data
	}
	oldUnrelated := seedAttempt("session_old_unrelated", nil)
	sourceAttempt := seedAttempt("session_source", &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"})
	cleanCheckpoint := seedAttempt("session_clean_checkpoint", &tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"})
	landingAttempt := seedAttempt("session_landing", nil, true)
	progressPath := itemsPath + "/" + url.PathEscape(string(issue.WorkItemID)) + "/progress?policy=" + url.QueryEscape(policy.ID) + "&instance=machine_example"
	progressResponse := performHubAPIRequest(t, service, http.MethodGet, progressPath, ownerToken, nil)
	requireNativeStatus(t, progressResponse, http.StatusOK)
	var progress nativeProgressView
	decodeHubResponse(t, progressResponse, &progress)
	if !progress.Request.NeedsCheck() || len(progress.Request.Attempts) == 0 || progress.Request.InstanceVersion != "example" || progress.Record != nil {
		t.Fatalf("PostgreSQL native progress request has check=%t attempts=%d version=%q record=%t", progress.Request.NeedsCheck(), len(progress.Request.Attempts), progress.Request.InstanceVersion, progress.Record != nil)
	}
	progressRequestID, err := progress.Request.ID()
	if err != nil {
		t.Fatal(err)
	}
	currentWorkItem, _, err := service.readWorkItemRecord(t.Context(), nativeScope{organization: "org_startup", project: project.ID, credential: ownerCredential}, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	progressPost := progresscheck.Publication{
		Mutation:         tracker.Mutation{IdempotencyKey: "progress_publication_example", LeaseID: landingAttempt.LeaseID, FencingToken: landingAttempt.FencingToken},
		ExpectedRevision: tracker.Revision(currentWorkItem.Revision), RequestID: progressRequestID,
		PolicyID: policy.ID, Instance: "machine_example",
		Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: progress.Request.AvailableSteps[0], Reason: "The failure evidence identifies unfinished implementation work", Diagnosis: progress.Request.Attempts[0].ID + " shows the attempt ended before the required result was produced", Instruction: "Complete the named step and retain its verification receipt", Convergence: "The next attempt can confirm the required result", Evidence: []string{progress.Request.Attempts[0].ID}},
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: "machine_other", Hostname: "other-host", Capacity: 1, Version: "other"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		policy   string
		instance string
	}{
		{name: "reject policy outside lease", policy: "policy_other", instance: "machine_example"},
		{name: "reject instance outside lease", policy: policy.ID, instance: "machine_other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := itemsPath + "/" + string(issue.WorkItemID) + "/progress"
			response := performHubAPIRequest(t, service, http.MethodGet, path+"?"+url.Values{"policy": {test.policy}, "instance": {test.instance}}.Encode(), workerToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var view nativeProgressView
			decodeHubResponse(t, response, &view)
			input := progressPost
			input.PolicyID, input.Instance = test.policy, test.instance
			input.RequestID, err = view.Request.ID()
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, input), http.StatusConflict)
			row, _, err := service.readWorkItemRecord(t.Context(), nativeScope{organization: "org_startup", project: project.ID, credential: ownerCredential}, string(issue.WorkItemID))
			if err != nil || row.EventSequence != currentWorkItem.EventSequence {
				t.Fatalf("rejected execution binding changed issue evidence: sequence=%d want=%d error=%v", row.EventSequence, currentWorkItem.EventSequence, err)
			}
		})
	}
	progressPostResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath+"/"+url.PathEscape(string(issue.WorkItemID))+"/progress", workerToken, progressPost)
	requireNativeStatus(t, progressPostResponse, http.StatusOK)
	var publishedProgress progresscheck.Record
	decodeHubResponse(t, progressPostResponse, &publishedProgress)
	if publishedProgress.RequestID != progressRequestID || publishedProgress.CommentID == "" {
		t.Fatalf("PostgreSQL published progress record = %+v", publishedProgress)
	}
	progressReadResponse := performHubAPIRequest(t, service, http.MethodGet, progressPath, ownerToken, nil)
	requireNativeStatus(t, progressReadResponse, http.StatusOK)
	decodeHubResponse(t, progressReadResponse, &progress)
	if progress.Record == nil || progress.Record.RequestID != progressRequestID {
		t.Fatalf("PostgreSQL progress receipt was not retained for its request: %+v", progress)
	}
	workerCredential, status, err = service.authenticateAccessHash(t.Context(), workerHash, "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate progress worker: status=%d error=%v", status, err)
	}
	progressOwner := nativeProgressOwner{service: service, scope: nativeScope{organization: "org_startup", project: project.ID, credential: workerCredential}, request: progress.Request, leaseID: landingAttempt.LeaseID, fencingToken: landingAttempt.FencingToken, expectedRevision: progressPost.ExpectedRevision}
	for _, test := range []struct {
		name    string
		mutate  string
		restore string
		owner   bool
	}{
		{name: "receipt rejects revoked credential", mutate: `UPDATE api_tokens SET revoked_at=now() WHERE organization_id='org_startup' AND id='credential_runner'`, restore: `UPDATE api_tokens SET revoked_at=NULL WHERE organization_id='org_startup' AND id='credential_runner'`},
		{name: "receipt rejects revoked project grant", mutate: `DELETE FROM token_grants WHERE organization_id='org_startup' AND token_id='credential_runner' AND project_id='project_example'`, restore: `INSERT INTO token_grants(organization_id,token_id,project_id) VALUES('org_startup','credential_runner','project_example')`},
		{name: "receipt rejects disabled runner", mutate: `UPDATE runner_identities SET state='disabled' WHERE organization_id='org_startup' AND runner_id='runner_example'`, restore: `UPDATE runner_identities SET state='active' WHERE organization_id='org_startup' AND runner_id='runner_example'`},
		{name: "receipt rejects revoked membership", mutate: `UPDATE hosted_members SET active=false WHERE organization_id='org_startup' AND user_id='user_owner'`, restore: `UPDATE hosted_members SET active=true WHERE organization_id='org_startup' AND user_id='user_owner'`, owner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := admin.ExecContext(t.Context(), test.mutate); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
				defer cancel()
				if _, err := admin.ExecContext(ctx, test.restore); err != nil {
					t.Error(err)
				}
			})
			owner := progressOwner
			if test.owner {
				owner.scope.credential = ownerCredential
			}
			if _, found, err := owner.Load(t.Context(), string(project.ID), string(issue.WorkItemID), progressRequestID); err == nil || found {
				t.Fatalf("revoked authority replayed progress receipt: found=%t error=%v", found, err)
			}
		})
	}
	t.Run("receipt cannot mask denied publication", func(t *testing.T) {
		original := service.config.TenantRepository
		service.config.TenantRepository = &postgresProgressReceiptRepository{Repository: original, hideReceipt: true}
		defer func() { service.config.TenantRepository = original }()
		candidate := publishedProgress
		candidate.CommentID = ""
		if _, err := progressOwner.Publish(t.Context(), candidate); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("denied publication replayed another publisher's receipt: %v", err)
		}
	})
	t.Run("publication preserves previous machine evidence", func(t *testing.T) {
		path := itemsPath + "/" + string(issue.WorkItemID)
		response := performHubAPIRequest(t, service, http.MethodPost, path+"/comments", workerToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "machine-evidence", LeaseID: landingAttempt.LeaseID, FencingToken: landingAttempt.FencingToken}, Body: "The machine diagnosis identifies the missing implementation result"})
		requireNativeStatus(t, response, http.StatusOK)
		var evidence tracker.NativeComment
		decodeHubResponse(t, response, &evidence)
		if evidence.Actor.Kind != "runner" {
			t.Fatalf("fixture comment actor=%q", evidence.Actor.Kind)
		}
		read := performHubAPIRequest(t, service, http.MethodGet, progressPath, workerToken, nil)
		requireNativeStatus(t, read, http.StatusOK)
		var view nativeProgressView
		decodeHubResponse(t, read, &view)
		if !slices.ContainsFunc(view.Request.Comments, func(comment progresscheck.Comment) bool { return comment.ID == evidence.ID }) {
			t.Fatal("request omitted the machine evidence before publication")
		}
		input := progressPost
		input.RequestID, err = view.Request.ID()
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/progress", workerToken, input), http.StatusOK)
		}
		read = performHubAPIRequest(t, service, http.MethodGet, progressPath, workerToken, nil)
		requireNativeStatus(t, read, http.StatusOK)
		decodeHubResponse(t, read, &view)
		id, err := view.Request.ID()
		if err != nil || id != input.RequestID || view.Record == nil || !slices.ContainsFunc(view.Request.Comments, func(comment progresscheck.Comment) bool { return comment.ID == evidence.ID }) {
			t.Fatalf("publication displaced machine evidence: request=%s expected=%s record=%t error=%v", id, input.RequestID, view.Record != nil, err)
		}
	})
	t.Run("system defect uses originating fenced issue", func(t *testing.T) {
		path := itemsPath + "/" + string(issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/comments", ownerToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "defect-evidence"}, Body: "The protocol failure persists and requires a system defect"}), http.StatusOK)
		response := performHubAPIRequest(t, service, http.MethodGet, progressPath, workerToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var view nativeProgressView
		decodeHubResponse(t, response, &view)
		input := progressPost
		input.RequestID, err = view.Request.ID()
		if err != nil {
			t.Fatal(err)
		}
		input.Decision = progresscheck.Decision{Outcome: progresscheck.SystemDefect, Reason: "The retained failure identifies a protocol defect", Diagnosis: view.Request.Attempts[0].ID + " identifies the protocol refusal that prevents the required result", Convergence: "Repairing the protocol permits the required result", Evidence: []string{view.Request.Attempts[0].ID}, DefectKey: "example-protocol-refusal", DefectTitle: "Repair example protocol refusal", DefectBody: issueContractTestSections}
		var retained progresscheck.Record
		for range 2 {
			response = performHubAPIRequest(t, service, http.MethodPost, path+"/progress", workerToken, input)
			requireNativeStatus(t, response, http.StatusOK)
			var record progresscheck.Record
			decodeHubResponse(t, response, &record)
			if record.DefectIssueID == "" || record.CommentID == "" || retained.DefectIssueID != "" && (record.DefectIssueID != retained.DefectIssueID || record.CommentID != retained.CommentID) {
				t.Fatalf("system defect publication was not retained: %+v", record)
			}
			retained = record
		}
		response = performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+retained.DefectIssueID, ownerToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var defect tracker.NativeIssue
		decodeHubResponse(t, response, &defect)
		if defect.State != "Backlog" || defect.WorkItemID == issue.WorkItemID {
			t.Fatalf("system defect issue=%+v", defect)
		}
		response = performHubAPIRequest(t, service, http.MethodGet, progressPath, workerToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &view)
		if view.Record == nil || view.Record.DefectIssueID != retained.DefectIssueID {
			t.Fatal("GET lost the system defect decision")
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.Relationships(t.Context(), project.ID, issue.WorkItemID, corestore.RelationshipCursor{}, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(page.Items, func(edge corestore.Relationship) bool {
				return string(edge.RelatedWorkItemID) == retained.DefectIssueID && edge.Type == "dependency" && edge.Direction == "blocked_by"
			}) {
				return errors.New("originating issue lost its system defect dependency")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	content := eventstore.NewContent(service.config.RunDetails)
	changeValue := tracker.ChangeRequest{ID: "change_recovery", OrganizationID: "org_startup", ProjectID: project.ID, WorkItemID: issue.WorkItemID, Title: "Example change", Revision: 1, CreatedAt: now, UpdatedAt: now}
	changeRaw, err := json.Marshal(changeValue)
	if err != nil {
		t.Fatal(err)
	}
	changeRecord, err := content.PrepareChange(t.Context(), "org_startup", changeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateChange(t.Context(), changeRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	versionValue := tracker.ChangeVersion{ID: "version_recovery", ChangeID: "change_recovery", Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://example.test/repo", HeadSHA: strings.Repeat("b", 40), PolicyID: policy.ID, AttemptID: landingAttempt.AttemptID}, Policy: policy, Actor: tracker.Actor{Kind: "human", PrincipalID: "user_owner"}, CreatedAt: now}
	versionRaw, err := json.Marshal(versionValue)
	if err != nil {
		t.Fatal(err)
	}
	versionRecord, err := content.PrepareVersion(t.Context(), "org_startup", versionRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateVersion(t.Context(), versionRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	landing := tracker.NativeLandingReceipt{VersionID: versionValue.ID, MergeSHA: strings.Repeat("a", 40), BaseRef: "main", Landed: true, ObservedAt: now}
	landingRecord, err := content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: "org_startup", AttemptID: landingAttempt.AttemptID, VersionID: versionValue.ID, Repository: versionValue.Repository, BaseRef: landing.BaseRef}, landing)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateLandingReceipt(t.Context(), landingRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var unrelatedRunEvent eventstore.Event
	if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		unrelatedRunEvent, err = r.LatestRunEvent(t.Context(), project.ID, oldUnrelated.AttemptID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	details := service.config.RunDetails.(*memoryRunDetails)
	details.mu.Lock()
	delete(details.data, unrelatedRunEvent.PayloadObjectKey)
	details.mu.Unlock()
	recovered, err = recovery.Load(t.Context(), postgresRouteRecoveryReader{service: service, token: workerToken, organization: "org_startup", project: project.ID}, "org_startup", project.ID, issue.WorkItemID)
	if err != nil {
		t.Fatalf("PostgreSQL recovery selected attempts: %v", err)
	}
	if recovered.SourceAttemptID != sourceAttempt.AttemptID || len(recovered.Attempts) != 3 {
		t.Fatalf("PostgreSQL recovery selected source/attempts = %q / %+v", recovered.SourceAttemptID, recovered.Attempts)
	}
	selectedAttempts := make(map[string]tracker.NativeAttempt, len(recovered.Attempts))
	for _, attempt := range recovered.Attempts {
		selectedAttempts[attempt.AttemptID] = attempt
	}
	if selectedAttempts[sourceAttempt.AttemptID].Checkpoint == nil || !selectedAttempts[sourceAttempt.AttemptID].Checkpoint.PreservesSource() || selectedAttempts[cleanCheckpoint.AttemptID].Checkpoint == nil || selectedAttempts[cleanCheckpoint.AttemptID].Checkpoint.PreservesSource() || selectedAttempts[landingAttempt.AttemptID].Runtime == nil || selectedAttempts[landingAttempt.AttemptID].Runtime.Landing == nil || selectedAttempts[landingAttempt.AttemptID].Runtime.Landing.MergeSHA != landing.MergeSHA {
		t.Fatalf("PostgreSQL recovery lost checkpoint or independent landing receipt: %+v", selectedAttempts)
	}
	t.Run("linked issue cannot recover another owner's source", func(t *testing.T) {
		input := changeTestInput()
		input.PolicyID = policy.ID
		bundle := changeTestSource(t, &input)
		value := tracker.ChangeVersion{ID: "version_source_owner", ChangeID: "change_source_owner", Number: 1, ChangeVersionInput: input, Policy: policy, Actor: tracker.Actor{Kind: "human", PrincipalID: "user_owner"}, CreatedAt: now}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareVersion(t.Context(), "org_startup", raw)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := content.PrepareSourceBundle(t.Context(), "org_startup", bundle)
		if err != nil {
			t.Fatal(err)
		}
		version.SourceObjectKey, version.SourceSHA256, version.SourceBytes, version.SourceContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
		changeValue := tracker.ChangeRequest{ID: value.ChangeID, OrganizationID: "org_startup", ProjectID: project.ID, WorkItemID: issue.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{issue.WorkItemID, secondIssue.WorkItemID}, Title: "Owned source", Revision: 1, CreatedAt: now, UpdatedAt: now}
		raw, err = json.Marshal(changeValue)
		if err != nil {
			t.Fatal(err)
		}
		change, err := content.PrepareChange(t.Context(), "org_startup", raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			change, err = w.CreateChange(t.Context(), change)
			if err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), version); err != nil {
				return err
			}
			change.CurrentVersionID = &value.ID
			if _, err := w.UpdateChange(t.Context(), change); err != nil {
				return err
			}
			for _, id := range changeValue.LinkedIssues {
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: "org_startup", ProjectID: project.ID, WorkItemID: id, ChangeID: value.ChangeID}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		owned := performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(issue.WorkItemID)+"/source-recovery", ownerToken, nil)
		requireNativeStatus(t, owned, http.StatusOK)
		var view sourceRecoveryView
		decodeHubResponse(t, owned, &view)
		if !view.Available || view.VersionID != value.ID {
			t.Fatalf("owned verified source unavailable: %+v", view)
		}
		foreign := performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(secondIssue.WorkItemID)+"/source-recovery", ownerToken, nil)
		requireNativeStatus(t, foreign, http.StatusNotFound)
	})
	t.Run("release before validation cannot commit progress", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		defer func() { cancel(); workers.Wait() }()
		held, identityReached := make(chan struct{}), make(chan struct{})
		released, validated := make(chan error, 1), make(chan error, 1)
		marker, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: "org_startup", ActorID: "progress-release-race", Operation: "progress", CommandKey: "release-race", RequestHash: strings.Repeat("a", 64), CreatedAt: now}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			released <- service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RunnerForLease(ctx, string(landingAttempt.LeaseID)); err != nil {
					return err
				}
				close(held)
				select {
				case <-identityReached:
				case <-ctx.Done():
					return ctx.Err()
				}
				_, err := w.Release(ctx, leasestore.Fence{LeaseID: string(landingAttempt.LeaseID), FencingToken: int64(landingAttempt.FencingToken)}, "completed")
				return err
			})
		})
		select {
		case <-held:
		case err := <-released:
			t.Fatalf("hold release identity: %v", err)
		}
		workers.Go(func() {
			validated <- service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				writer := &postgresProgressIdentityWriter{Writer: w, reached: identityReached}
				if err := progressOwner.validateProgressLeaseWriter(ctx, writer); err != nil {
					return err
				}
				_, err := w.PutReceipt(ctx, marker)
				return err
			})
		})
		validationErr := <-validated
		if !errors.Is(validationErr, tracker.ErrStaleFencingToken) {
			t.Fatalf("released authority committed progress: %v", validationErr)
		}
		if err := <-released; err != nil {
			t.Fatalf("release lease: %v", err)
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.Receipt(t.Context(), marker.ActorID, marker.Operation, marker.CommandKey)
			if !errors.Is(err, corestore.ErrNotFound) {
				return fmt.Errorf("released progress receipt retained: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("source destinations use bounded project pages", func(t *testing.T) {
		var base runnerstore.Identity
		if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			base, err = r.RunnerIdentity(t.Context(), "runner_example")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		otherProject := tracker.ProjectID("project_recovery_other")
		if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: otherProject, Name: "Other recovery project", Slug: "other-recovery", Profile: "native"}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index := range runnerstore.SourceRecoveryPageLimit + 2 {
			id := fmt.Sprintf("runner_recovery_%02d", index)
			assignedProject := project.ID
			if index == runnerstore.SourceRecoveryPageLimit+1 {
				assignedProject = otherProject
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				identity := base
				identity.RunnerID, identity.TokenID, identity.EnrollmentID = id, id+"_token", id+"_enrollment"
				identity.MachineID = tracker.MachineID(id + "_machine")
				identity.OS, identity.IsolationTier = "linux", isolation.NativeTrusted
				identity.ProjectIDs = []tracker.ProjectID{assignedProject}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: identity.TokenID, Name: "Recovery " + id, Hash: apikey.HashToken(identity.TokenID), Fingerprint: tokenFingerprint(apikey.HashToken(identity.TokenID)), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: identity.TokenID, ProjectID: assignedProject}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_startup", ID: identity.EnrollmentID, RunnerID: id, MachineID: identity.MachineID, TokenHash: apikey.HashToken(identity.EnrollmentID), Scope: "projects", ProjectIDs: identity.ProjectIDs, Operations: identity.Operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "credential_owner"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), identity.EnrollmentID, id, identity.MachineID, now); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: string(identity.MachineID), Hostname: id, Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), identity); err != nil {
					return err
				}
				if err := w.UpdateRunnerCapacityConfig(t.Context(), id, *base.CapacityConfig); err != nil {
					return err
				}
				if err := w.ReplaceRunnerBackendIsolation(t.Context(), id, isolation.Report{"example-backend": {isolation.NativeTrusted}}); err != nil {
					return err
				}
				report := capacity
				report.SharedAccountAlias = ""
				return w.ReplaceRunnerProviderReports(t.Context(), id, []providercapacity.Report{report})
			}); err != nil {
				t.Fatalf("seed recovery runner %s: %v", id, err)
			}
		}
		original := service.config.TenantRepository
		pageCalls := 0
		service.config.TenantRepository = postgresSourceRecoveryRepository{Repository: original, pageCalls: &pageCalls}
		defer func() { service.config.TenantRepository = original }()
		seen := map[string]bool{}
		after := ""
		for range 3 {
			path := itemsPath + "/" + string(issue.WorkItemID) + "/source-recovery?destinations_after=" + url.QueryEscape(after)
			response := performHubAPIRequest(t, service, http.MethodGet, path, ownerToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var view sourceRecoveryView
			decodeHubResponse(t, response, &view)
			if len(view.Destinations) > runnerstore.SourceRecoveryPageLimit {
				t.Fatalf("destination page exceeded bound: %d", len(view.Destinations))
			}
			for _, destination := range view.Destinations {
				if seen[destination.ID] || destination.ID == fmt.Sprintf("runner_recovery_%02d", runnerstore.SourceRecoveryPageLimit+1) {
					t.Fatalf("duplicate or unauthorized destination: %s", destination.ID)
				}
				seen[destination.ID] = true
			}
			if view.DestinationsNext == "" {
				break
			}
			if view.DestinationsNext <= after {
				t.Fatal("destination cursor did not advance")
			}
			after = view.DestinationsNext
		}
		if len(seen) != runnerstore.SourceRecoveryPageLimit+1 || pageCalls != 2 {
			t.Fatalf("project destination pagination: runners=%d pages=%d", len(seen), pageCalls)
		}
		secondary, _, err := service.readWorkItemRecord(t.Context(), nativeScope{organization: "org_startup", project: project.ID, credential: ownerCredential}, string(secondIssue.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		var hostTimeBeforeRenewal time.Time
		if err := original.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: secondary.ID, ExpectedRevision: secondary.Revision, MachineID: "runner_recovery_00_machine", SessionID: "session_recovery_capacity", LeaseID: "lease_recovery_capacity", TTL: time.Minute})
			if err != nil {
				return err
			}
			hostTimeBeforeRenewal = lease.RenewedAt.Add(-time.Millisecond)
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: "org_startup", RunnerID: "runner_recovery_00", LeaseID: lease.LeaseID, AssignedAt: now}); err != nil {
				return err
			}
			report := capacity
			report.SharedAccountAlias = ""
			return w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: "org_startup", LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "implement", Backend: "example-backend", Model: "example-model"}, Report: report, Reason: "Retained capacity fixture"}})
		}); err != nil {
			t.Fatal(err)
		}
		if err := original.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.(runnerstore.SourceRecoveryReader).ProjectSourceRecoveryPage(t.Context(), project.ID, "runner_recovery_", 1, hostTimeBeforeRenewal)
			if err != nil {
				return err
			}
			if len(page.Items) != 1 {
				return errors.New("missing occupied recovery candidate")
			}
			candidate := page.Items[0]
			if candidate.Record.Identity.RunnerID != "runner_recovery_00" || candidate.RunnerUsed != 1 || candidate.ProjectUsed != 1 || candidate.HostUsed != 1 || len(candidate.Providers) != 1 || candidate.Providers[0].Used != 1 || candidate.Providers[0].Available() {
				return fmt.Errorf("scoped recovery capacity lost active reservations: runner=%s runnerUsed=%d projectUsed=%d hostUsed=%d providers=%+v", candidate.Record.Identity.RunnerID, candidate.RunnerUsed, candidate.ProjectUsed, candidate.HostUsed, candidate.Providers)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

type postgresSourceRecoveryRepository struct {
	tenantstore.Repository
	pageCalls *int
}

func (r postgresSourceRecoveryRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(postgresSourceRecoveryReader{Reader: reader, pageCalls: r.pageCalls})
	})
}

type postgresSourceRecoveryReader struct {
	tenantstore.Reader
	pageCalls *int
}

func (r postgresSourceRecoveryReader) ProjectSourceRecoveryPage(ctx context.Context, project tracker.ProjectID, after string, limit int, now time.Time) (runnerstore.SourceRecoveryPage, error) {
	*r.pageCalls++
	if project != "project_example" || limit > runnerstore.SourceRecoveryPageLimit {
		return runnerstore.SourceRecoveryPage{}, errors.New("unbounded or unscoped recovery query")
	}
	return r.Reader.(runnerstore.SourceRecoveryReader).ProjectSourceRecoveryPage(ctx, project, after, limit, now)
}

func (r postgresSourceRecoveryReader) FleetRecords(context.Context, int) ([]runnerstore.FleetRecord, error) {
	return nil, errors.New("source recovery must not scan the fleet")
}

func (r postgresSourceRecoveryReader) FleetLeases(context.Context, time.Time, int) ([]runnerstore.FleetLease, error) {
	return nil, errors.New("source recovery must not scan fleet leases")
}

func (r postgresSourceRecoveryReader) FleetNames(context.Context, int) ([]runnerstore.FleetName, error) {
	return nil, errors.New("source recovery must not scan fleet names")
}

func (r postgresSourceRecoveryReader) FleetProjectCount(context.Context, int) (int, error) {
	return 0, errors.New("source recovery must not count fleet projects")
}

func (r postgresSourceRecoveryReader) CheckpointAttemptPage(ctx context.Context, issue int64, cursor leasestore.AttemptCursor, limit int) (leasestore.AttemptPage, error) {
	return r.Reader.(leasestore.CheckpointAttemptReader).CheckpointAttemptPage(ctx, issue, cursor, limit)
}

type postgresProgressIdentityWriter struct {
	tenantstore.Writer
	reached chan struct{}
	once    sync.Once
}

func (w *postgresProgressIdentityWriter) RunnerIdentityByCredential(ctx context.Context, id string) (runnerstore.Identity, error) {
	w.once.Do(func() { close(w.reached) })
	return w.Writer.RunnerIdentityByCredential(ctx, id)
}

func (w *postgresProgressIdentityWriter) RunnerForLease(ctx context.Context, id string) (runnerstore.Identity, error) {
	w.once.Do(func() { close(w.reached) })
	return w.Writer.RunnerForLease(ctx, id)
}

func (w *postgresProgressIdentityWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
}

type postgresProgressReceiptRepository struct {
	tenantstore.Repository
	mu          sync.Mutex
	hideReceipt bool
}

func (r *postgresProgressReceiptRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(postgresProgressReceiptReader{Reader: reader, repository: r})
	})
}

func (r *postgresProgressReceiptRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(writer tenantstore.Writer) error {
		return write(postgresProgressDeniedWriter{Writer: writer})
	})
}

type postgresProgressReceiptReader struct {
	tenantstore.Reader
	repository *postgresProgressReceiptRepository
}

func (r postgresProgressReceiptReader) Receipt(ctx context.Context, actor, operation, key string) (eventstore.Receipt, error) {
	if actor == nativeProgressReceiptActor {
		r.repository.mu.Lock()
		hide := r.repository.hideReceipt
		r.repository.hideReceipt = false
		r.repository.mu.Unlock()
		if hide {
			return eventstore.Receipt{}, corestore.ErrNotFound
		}
	}
	return r.Reader.Receipt(ctx, actor, operation, key)
}

func (r postgresProgressReceiptReader) CheckpointAttemptPage(ctx context.Context, issue int64, cursor leasestore.AttemptCursor, limit int) (leasestore.AttemptPage, error) {
	return r.Reader.(leasestore.CheckpointAttemptReader).CheckpointAttemptPage(ctx, issue, cursor, limit)
}

type postgresProgressDeniedWriter struct {
	tenantstore.Writer
}

func (w postgresProgressDeniedWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
}

func (w postgresProgressDeniedWriter) PutReceipt(ctx context.Context, receipt eventstore.Receipt) (eventstore.Receipt, error) {
	if receipt.ActorID == nativeProgressReceiptActor {
		return eventstore.Receipt{}, accessstore.ErrDenied
	}
	return w.Writer.PutReceipt(ctx, receipt)
}

type postgresRouteRecoveryReader struct {
	service          *Service
	token            string
	organization     tracker.OrganizationID
	project          tracker.ProjectID
	bodyFragmentSeen *bool
	fragmentRequests *int
}

func (r postgresRouteRecoveryReader) Detail(ctx context.Context, id tracker.NativeWorkItemID, headers http.Header) (tracker.NativeRecovery, error) {
	path := "/api/v2/organizations/" + url.PathEscape(string(r.organization)) + "/projects/" + url.PathEscape(string(r.project)) + "/work-items/" + url.PathEscape(string(id))
	request := httptest.NewRequest(http.MethodGet, path+"?view=recovery", nil)
	request.Header.Set("Authorization", "Bearer "+r.token)
	request = request.WithContext(ctx)
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response := httptest.NewRecorder()
	r.service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return tracker.NativeRecovery{}, fmt.Errorf("recovery detail status %d", response.Code)
	}
	var result tracker.NativeRecovery
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		return result, err
	}
	if r.bodyFragmentSeen != nil && result.Read != nil && result.Read.IssueBody != nil {
		*r.bodyFragmentSeen = true
	}
	return result, nil
}

func (r postgresRouteRecoveryReader) Evidence(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.NativeRecoveryPage, error) {
	if r.fragmentRequests != nil {
		*r.fragmentRequests = *r.fragmentRequests + 1
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(r.organization)) + "/projects/" + url.PathEscape(string(r.project)) + "/work-items/" + url.PathEscape(string(id))
	query := url.Values{"view": {"recovery"}, "limit": {"1"}, "cursor": {cursor}}
	request := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	request.Header.Set("Authorization", "Bearer "+r.token)
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	r.service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return tracker.NativeRecoveryPage{}, fmt.Errorf("recovery evidence status %d", response.Code)
	}
	var result tracker.NativeRecovery
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		return tracker.NativeRecoveryPage{}, err
	}
	if result.Continuation == nil {
		return tracker.NativeRecoveryPage{}, fmt.Errorf("recovery evidence continuation is missing")
	}
	return *result.Continuation, nil
}
