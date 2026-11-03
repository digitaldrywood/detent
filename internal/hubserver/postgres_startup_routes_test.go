package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubclient/recovery"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOpenCreatesListsAndClaimsNativeIssue(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Workspace = &WorkspaceConfig{Enabled: true}
	provider := cfg.Hosted.Provider
	if _, err := provider.CreateMembership(t.Context(), "user_owner", "provider_org_startup", "owner"); err != nil {
		t.Fatal(err)
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	const ownerToken = "example_owner_token"
	ownerHash := apikey.HashToken(ownerToken)
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
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "user_owner", ProjectID: projectID, CanWrite: true, ManageRunner: true}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_owner", ProjectID: projectID}); err != nil {
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
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_startup", ID: "enrollment_runner", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "credential_owner"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", "runner_example", "machine_example", now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: "machine_example", Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_startup", RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_runner", EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		if err := w.UpdateRunnerCapacityConfig(t.Context(), "runner_example", runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now}); err != nil {
			return err
		}
		return w.ReplaceRunnerProviderReports(t.Context(), "runner_example", []providercapacity.Report{capacity})
	}); err != nil {
		t.Fatal(err)
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
	recovered, err := recovery.Load(t.Context(), postgresRouteRecoveryReader{service: service, token: workerToken, organization: "org_startup", project: project.ID, inflate: true}, "org_startup", project.ID, issue.WorkItemID)
	if err != nil {
		t.Fatalf("PostgreSQL recovery load: %v", err)
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

	seedAttempt := func(session string, checkpoint *tracker.NativeCheckpoint) tracker.NativeRunData {
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
		leasePath := "/api/v2/organizations/org_startup/projects/" + url.PathEscape(string(project.ID)) + "/leases/" + url.PathEscape(string(attemptLease.ID))
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, leasePath+"/release", workerToken, tracker.NativeLeaseMutation{FencingToken: attemptLease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		return data
	}
	oldUnrelated := seedAttempt("session_old_unrelated", nil)
	sourceAttempt := seedAttempt("session_source", &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"})
	cleanCheckpoint := seedAttempt("session_clean_checkpoint", &tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"})
	landingAttempt := seedAttempt("session_landing", nil)

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
}

type postgresRouteRecoveryReader struct {
	service      *Service
	token        string
	organization tracker.OrganizationID
	project      tracker.ProjectID
	inflate      bool
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
	if r.inflate {
		result.Discussion = append(result.Discussion, tracker.NativeComment{Body: strings.Repeat("oversized recovery projection ", 50000)})
		scope := nativeScope{organization: r.organization, project: r.project, credential: apiCredential{ID: "credential_runner", Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner_example"}}}}
		if err := r.service.fitNativeRecovery(ctx, scope, path, &result, headers.Get("X-Detent-Recovery-Body") == "fragments"); err != nil {
			return tracker.NativeRecovery{}, err
		}
	}
	return result, nil
}

func (r postgresRouteRecoveryReader) Evidence(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.NativeRecoveryPage, error) {
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
