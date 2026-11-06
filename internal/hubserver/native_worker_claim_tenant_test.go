package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativePreviewReadCounts struct {
	readTenant          int
	projectRecord       int
	projectPolicy       int
	candidatePage       int
	landingCapacityRead int
	claimPlanBatch      int
}

type nativePreviewCountingRepository struct {
	tenantstore.Repository
	counts     *nativePreviewReadCounts
	staleFirst bool
}

func (r nativePreviewCountingRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.counts.readTenant++
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(nativePreviewCountingReader{Reader: reader, counts: r.counts, staleFirst: r.staleFirst})
	})
}

type nativePreviewCountingReader struct {
	tenantstore.Reader
	counts     *nativePreviewReadCounts
	staleFirst bool
}

func (r nativePreviewCountingReader) ProjectRecord(ctx context.Context, project tracker.ProjectID) (projectstore.Record, error) {
	r.counts.projectRecord++
	return r.Reader.ProjectRecord(ctx, project)
}

func (r nativePreviewCountingReader) ProjectPolicy(ctx context.Context, project tracker.ProjectID) (policystore.Revision, error) {
	r.counts.projectPolicy++
	return r.Reader.ProjectPolicy(ctx, project)
}

func (r nativePreviewCountingReader) CandidatePage(ctx context.Context, query claimstore.CandidateQuery) (claimstore.CandidatePage, error) {
	r.counts.candidatePage++
	page, err := r.Reader.CandidatePage(ctx, query)
	if err == nil && r.staleFirst && len(page.Items) > 0 {
		page.Items[0].Revision++
	}
	return page, err
}

func (r nativePreviewCountingReader) LandingCapacityUsage(ctx context.Context, project tracker.ProjectID, now time.Time) (claimstore.LandingCapacityUsage, error) {
	r.counts.landingCapacityRead++
	return r.Reader.LandingCapacityUsage(ctx, project, now)
}

func (r nativePreviewCountingReader) ClaimPreviewPlanRows(ctx context.Context, project tracker.ProjectID, candidates []claimstore.Candidate) ([]tenantstore.ClaimPreviewPlanRow, error) {
	r.counts.claimPlanBatch++
	reader, ok := r.Reader.(tenantstore.ClaimPreviewPlanRowsReader)
	if !ok {
		return nil, corestore.ErrNotFound
	}
	return reader.ClaimPreviewPlanRows(ctx, project, candidates)
}

func TestNativeClaimUsesHeartbeatTimeAtWriteValidation(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, Clock: clock.Now,
		SecretKeys: secretTestKeys(t, "1", "1"),
	})
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "heartbeat-race-project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
	requireNativeStatus(t, projectResponse, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, projectResponse, &project)
	descriptor := hubTestPolicy()
	policyRevision, err := eventstore.NewContent(service.config.RunDetails).PrepareProjectPolicy(t.Context(), organization.ID, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = project.ID
	policyRevision.ApprovedBy = "test_hub_admin"
	policyRevision.ApprovedAt = clock.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}
	itemsPath := projectPath + "/" + string(project.ID) + "/work-items"
	issueResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "heartbeat-race-issue"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, issueResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, issueResponse, &issue)
	startedAt := clock.Now().UTC()
	const runnerID = "runner_heartbeat_race"
	const machineID = "machine_heartbeat_race"
	const credentialID = "credential_heartbeat_race"
	const enrollmentID = "enrollment_heartbeat_race"
	const secret = "example_runner_heartbeat_race_secret"
	tokenHash := apikey.HashToken(secret)
	expires := startedAt.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: startedAt, UpdatedAt: startedAt, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, CreatedAt: startedAt, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, machineID, startedAt); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: machineID, Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runnerID, MachineID: machineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: startedAt, ExpiresAt: expires, LastHeartbeatAt: startedAt}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), runnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: startedAt})
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: credentialID, Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runnerID, MachineID: machineID}, OrganizationID: organization.ID}}}
	request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: machineID, SessionID: "heartbeat-race-session", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
	plan, err := service.readNativeClaimPlan(t.Context(), scope, request, startedAt)
	if err != nil || !nativeClaimIssueContractAllowed(plan) {
		t.Fatalf("read claim plan: error=%v", err)
	}
	clock.Advance(time.Second)
	heartbeatPath := "/api/v2/organizations/" + string(organization.ID) + "/projects/" + string(project.ID) + "/machines/" + machineID + "/heartbeat"
	heartbeatResponse := performHubAPIRequest(t, service, http.MethodPost, heartbeatPath, secret, map[string]any{"display_name": "Example host", "capacity": 1, "version": "example", "os": "darwin", "architecture": "arm64", "protocol_major": tracker.NativeProtocolMajor})
	requireNativeStatus(t, heartbeatResponse, http.StatusOK)
	var identity runnerstore.Identity
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		identity, err = r.RunnerIdentityByCredential(t.Context(), credentialID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !identity.LastHeartbeatAt.After(startedAt) || !identity.LastHeartbeatAt.Before(clock.Now().Add(runnerauth.HeartbeatTimeout)) {
		t.Fatalf("heartbeat timestamp did not advance within the valid window: %s", identity.LastHeartbeatAt)
	}
	lease, err := service.claimNativeIssueTenantWithPlan(t.Context(), scope, request, 5*time.Minute, startedAt, plan, nil)
	if err != nil {
		t.Fatalf("claim rejected a fresh identity after heartbeat advanced during planning: %v", err)
	}
	if lease.LeaseID == "" {
		t.Fatal("claim returned an empty lease ID")
	}
	future := identity
	future.LastHeartbeatAt = clock.Now().Add(time.Second)
	if runnerIdentityCanClaim(future, scope, machineID, clock.Now()) {
		t.Fatal("claim accepted a heartbeat timestamp from the future")
	}
	expired := identity
	expired.LastHeartbeatAt = clock.Now().Add(-runnerauth.HeartbeatTimeout - time.Second)
	if runnerIdentityCanClaim(expired, scope, machineID, clock.Now()) {
		t.Fatal("claim accepted an expired heartbeat")
	}
}

func TestNativeClaimTenantPersistsProviderReservationAtomicallyAndReplaysIt(t *testing.T) {
	var logs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	participant := &nativeRunEventParticipantFixture{}
	details := &memoryRunDetails{}
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: details, Clock: clock.Now,
		NativeRunEvents: participant, SecretKeys: secretTestKeys(t, "1", "1"), Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
	requireNativeStatus(t, projectResponse, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, projectResponse, &project)
	descriptor := hubTestPolicy()
	runDetails := service.config.RunDetails
	policyRevision, err := eventstore.NewContent(runDetails).PrepareProjectPolicy(t.Context(), organization.ID, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = project.ID
	policyRevision.ApprovedBy = "test_hub_admin"
	policyRevision.ApprovedAt = service.config.now().UTC()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}
	itemsPath := projectPath + "/" + string(project.ID) + "/work-items"
	now := service.config.now().UTC()
	service.localOrganization = string(organization.ID)
	envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	items := make([]tracker.NativeIssue, 0, 2)
	for _, idempotencyKey := range []string{"issue-first", "issue-second"} {
		response := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: idempotencyKey}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		items = append(items, issue)
	}
	changeResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath+"/"+string(items[0].WorkItemID)+"/changes", testHubAdminToken, tracker.CreateChange{
		Mutation: tracker.Mutation{IdempotencyKey: "shared-change"}, Title: "Shared example change",
		LinkedIssues: []tracker.NativeWorkItemID{items[1].WorkItemID},
	})
	requireNativeStatus(t, changeResponse, http.StatusOK)
	var sharedChange tracker.ChangeRequest
	decodeHubResponse(t, changeResponse, &sharedChange)
	changePath := itemsPath + "/" + string(items[0].WorkItemID) + "/changes/" + sharedChange.ID
	for _, item := range items {
		if err := service.refreshBoardRecord(t.Context(), organization.ID, viewstore.BoardCandidate{ProjectID: project.ID, WorkItemID: item.WorkItemID}, now); err != nil {
			t.Fatal(err)
		}
	}
	draftScope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
	draftPage, err := service.readBoardRecords(t.Context(), draftScope, url.Values{"limit": {"200"}, "include": {"board"}})
	if err != nil || len(draftPage.Cards) != 2 {
		t.Fatalf("draft board cards=%d error=%v", len(draftPage.Cards), err)
	}
	for _, card := range draftPage.Cards {
		if card.Change == nil || card.Change.Status != "draft" {
			t.Fatalf("draft change status=%+v", card.Change)
		}
	}
	firstVersionResponse := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "shared-version-first"}, ChangeVersionInput: changeTestInput()})
	requireNativeStatus(t, firstVersionResponse, http.StatusOK)
	var firstVersion tracker.ChangeVersion
	decodeHubResponse(t, firstVersionResponse, &firstVersion)
	secondInput := changeTestInput()
	secondInput.HeadSHA = strings.Repeat("d", 40)
	secondVersionResponse := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "shared-version-second"}, ExpectedVersionID: firstVersion.ID, ChangeVersionInput: secondInput})
	requireNativeStatus(t, secondVersionResponse, http.StatusOK)
	var secondVersion tracker.ChangeVersion
	decodeHubResponse(t, secondVersionResponse, &secondVersion)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
		batchReader, ok := reader.(tenantstore.ClaimPreviewPlanRowsReader)
		if !ok {
			return corestore.ErrNotFound
		}
		batchCandidates := make([]claimstore.Candidate, 0, len(items))
		for _, issue := range items {
			item, err := reader.WorkItemByNativeID(t.Context(), issue.WorkItemID)
			if err != nil {
				return err
			}
			batchCandidates = append(batchCandidates, claimstore.Candidate{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: item.ID, Revision: item.Revision, State: valueOrEmpty(item.WorkflowStateName)})
		}
		rows, err := batchReader.ClaimPreviewPlanRows(t.Context(), project.ID, batchCandidates)
		if err != nil {
			return err
		}
		if len(rows) != 2 || rows[0].Change == nil || rows[1].Change == nil || rows[0].Change.ID != sharedChange.ID || rows[1].Change.ID != sharedChange.ID {
			return fmt.Errorf("linked change was not mapped to both candidate items")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	secret := "example_runner_secret"
	tokenHash := apikey.HashToken(secret)
	expires := now.Add(time.Hour)
	report := providercapacity.Report{Provider: "example-provider", Backend: "example-backend", AccountAlias: "example-account", SharedAccountAlias: "shared-account", Billing: "subscription", Models: []string{"example-model"}, MaxConcurrent: 1, Availability: "exhausted", ObservedAt: now}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: "credential_runner", Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: "credential_runner", ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: "enrollment_runner", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", "runner_example", "machine_example", now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine_example", Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_runner", EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		if err := w.UpdateRunnerCapacityConfig(t.Context(), "runner_example", runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: now}); err != nil {
			return err
		}
		return w.ReplaceRunnerProviderReports(t.Context(), "runner_example", []providercapacity.Report{report})
	}); err != nil {
		t.Fatal(err)
	}
	base := projectPath + "/" + string(project.ID)
	requirement := providercapacity.Requirement{Role: "code", Backend: report.Backend, Model: report.Models[0]}
	request := tracker.NativeClaim{LandingCapacity: 1, PolicyID: descriptor.ID, WorkItemID: items[0].WorkItemID, MachineID: "machine_example", SessionID: "session_first", TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability, tracker.NativeProviderCapacityCapability}, ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: items[0].WorkItemID, Revision: items[0].Revision, Requirement: requirement}}}
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: "credential_runner", Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner_example", MachineID: "machine_example"}, OrganizationID: organization.ID}}}
	plan, err := service.readNativeClaimPlan(t.Context(), scope, request, now)
	if err != nil || !nativeClaimIssueContractAllowed(plan) {
		t.Fatalf("read claim plan = %+v, error %v", plan, err)
	}
	staleEvidenceSequence := plan.changeRecord
	staleEvidenceSequence.EvidenceSequence++
	if sameNativeClaimChangeRecord(plan.changeRecord, staleEvidenceSequence) {
		t.Fatal("claim writer fence accepted a changed evidence sequence")
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		plan.changePolicy, err = readCurrentChangePolicyRows(t.Context(), reader, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var oldVersionRow eventstore.Version
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		oldVersionRow, err = reader.Version(t.Context(), sharedChange.ID, firstVersion.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	withMissingDetail := func(key string, run func() error) error {
		details.mu.Lock()
		payload := bytes.Clone(details.data[key])
		delete(details.data, key)
		details.mu.Unlock()
		err := run()
		details.mu.Lock()
		details.data[key] = payload
		details.mu.Unlock()
		return err
	}
	mergePlan := plan
	mergePlan.candidate.State = "Merging"
	if err := withMissingDetail(oldVersionRow.PayloadObjectKey, func() error {
		_, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, mergePlan, &descriptor)
		return err
	}); err != nil {
		t.Fatalf("claim hydration read missing historical version payload: %v", err)
	}
	if err := withMissingDetail(plan.changeVersion.PayloadObjectKey, func() error {
		_, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, mergePlan, &descriptor)
		return err
	}); err == nil {
		t.Fatal("claim hydration accepted a missing current version payload")
	}
	for _, item := range items {
		if err := service.refreshBoardRecord(t.Context(), organization.ID, viewstore.BoardCandidate{ProjectID: project.ID, WorkItemID: item.WorkItemID}, now); err != nil {
			t.Fatal(err)
		}
	}
	baselineBoard, err := service.readBoardRecords(t.Context(), scope, url.Values{"limit": {"200"}, "include": {"board"}})
	if err != nil || len(baselineBoard.Cards) != 2 {
		t.Fatalf("baseline board: cards=%d error=%v", len(baselineBoard.Cards), err)
	}
	fullDetail, err := service.selectedChangeDetail(t.Context(), scope, string(items[0].WorkItemID), sharedChange.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range baselineBoard.Cards {
		if card.Change == nil || card.Change.Status != fullDetail.Summary.Status {
			t.Fatalf("current board summary differs from full history: %+v vs %+v", card.Change, fullDetail.Summary)
		}
	}
	if err := withMissingDetail(oldVersionRow.PayloadObjectKey, func() error {
		currentBoard, err := service.readBoardRecords(t.Context(), scope, url.Values{"limit": {"200"}, "include": {"board"}})
		if err != nil {
			return err
		}
		if len(currentBoard.Cards) != 2 {
			return fmt.Errorf("current board cards=%d", len(currentBoard.Cards))
		}
		for index, card := range currentBoard.Cards {
			if card.Issue.WorkItemID != baselineBoard.Cards[index].Issue.WorkItemID || card.Change == nil || card.Change.Status != baselineBoard.Cards[index].Change.Status {
				return fmt.Errorf("board list changed order or current status at %d", index)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("board list read missing historical payload: %v", err)
	}
	if err := withMissingDetail(plan.changeVersion.PayloadObjectKey, func() error {
		_, err := service.readBoardRecords(t.Context(), scope, url.Values{"limit": {"200"}, "include": {"board"}})
		return err
	}); err == nil {
		t.Fatal("board list accepted missing current version payload")
	}
	claimPath := base + "/claims"
	batchDetails := &boardDetailBatchCounter{DetailStore: service.config.RunDetails}
	service.config.RunDetails = batchDetails
	counts := &nativePreviewReadCounts{}
	service.config.TenantRepository = nativePreviewCountingRepository{Repository: repository, counts: counts}
	previewClaim := request
	previewClaim.WorkItemID = ""
	previewClaim.SessionID = ""
	previewClaim.ProviderCandidates = nil
	sharedPreviewResponse := performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, tracker.NativeCapacityPreview{NativeClaim: previewClaim, Limit: 2})
	requireNativeStatus(t, sharedPreviewResponse, http.StatusOK)
	if batchDetails.batches.Load() != 1 || batchDetails.scalar.Load() != 0 {
		t.Fatalf("preview allocation lookups: batch=%d scalar=%d", batchDetails.batches.Load(), batchDetails.scalar.Load())
	}
	var sharedPreview tracker.NativeCapacityPage
	decodeHubResponse(t, sharedPreviewResponse, &sharedPreview)
	if len(sharedPreview.Items) != 2 || !slices.Contains([]tracker.NativeWorkItemID{items[0].WorkItemID, items[1].WorkItemID}, sharedPreview.Items[0].WorkItemID) || !slices.Contains([]tracker.NativeWorkItemID{items[0].WorkItemID, items[1].WorkItemID}, sharedPreview.Items[1].WorkItemID) || sharedPreview.Items[0].WorkItemID == sharedPreview.Items[1].WorkItemID {
		t.Fatalf("two-candidate preview order = %+v", sharedPreview.Items)
	}
	twoCandidateCounts := *counts
	*counts = nativePreviewReadCounts{}
	sharedPreviewResponse = performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, tracker.NativeCapacityPreview{NativeClaim: previewClaim, Limit: 1})
	requireNativeStatus(t, sharedPreviewResponse, http.StatusOK)
	var singlePreview tracker.NativeCapacityPage
	decodeHubResponse(t, sharedPreviewResponse, &singlePreview)
	if len(singlePreview.Items) != 1 || singlePreview.Items[0].WorkItemID != sharedPreview.Items[0].WorkItemID {
		t.Fatalf("single-candidate preview changed first result: one=%+v two=%+v", singlePreview.Items, sharedPreview.Items)
	}
	if twoCandidateCounts != *counts {
		t.Fatalf("preview shared read counts changed with candidate count: two=%+v one=%+v", twoCandidateCounts, *counts)
	}
	if twoCandidateCounts.claimPlanBatch != 1 {
		t.Fatalf("two-candidate preview batch read calls = %d, want 1", twoCandidateCounts.claimPlanBatch)
	}
	for _, test := range []struct {
		name       string
		breakSQL   string
		restoreSQL string
		wantItems  int
	}{
		{"linked_source", "UPDATE issues SET native_source_key='missing-source' WHERE organization_id=$1 AND native_id=$2", "UPDATE issues SET native_source_key=NULL WHERE organization_id=$1 AND native_id=$2", 1},
		{"all_linked_sources", "UPDATE issues SET native_source_key=native_id WHERE organization_id=$1 AND project_id=(SELECT project_id FROM issues WHERE organization_id=$1 AND native_id=$2)", "UPDATE issues SET native_source_key=NULL WHERE organization_id=$1 AND project_id=(SELECT project_id FROM issues WHERE organization_id=$1 AND native_id=$2)", 0},
		{"item_version", "UPDATE issues SET revision=revision+100 WHERE organization_id=$1 AND native_id=$2", "UPDATE issues SET revision=revision-100 WHERE organization_id=$1 AND native_id=$2", 1},
	} {
		t.Run("preview_missing_"+test.name, func(t *testing.T) {
			if _, err := admin.ExecContext(t.Context(), test.breakSQL, organization.ID, items[0].WorkItemID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				deadline, _ := t.Deadline()
				ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
				defer cancel()
				if _, err := admin.ExecContext(ctx, test.restoreSQL, organization.ID, items[0].WorkItemID); err != nil {
					t.Error(err)
				}
			})
			for _, sessionID := range []string{"", "example-preview-session"} {
				claim := previewClaim
				claim.SessionID = sessionID
				logs.Reset()
				response := performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, tracker.NativeCapacityPreview{NativeClaim: claim, Limit: 2})
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.NativeCapacityPage
				decodeHubResponse(t, response, &page)
				if len(page.Items) != test.wantItems || test.wantItems == 1 && page.Items[0].WorkItemID != items[1].WorkItemID || page.Next == 0 {
					t.Fatalf("incomplete candidate affected valid results or cursor: %+v", page)
				}
				if !strings.Contains(logs.String(), "skip incomplete preview candidate") || !strings.Contains(logs.String(), string(items[0].WorkItemID)) {
					t.Fatalf("missing candidate diagnostic: %s", logs.String())
				}
			}
			if _, err := service.readNativeClaimPlan(t.Context(), scope, request, now); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("claim plan accepted missing related data: %v", err)
			}
		})
	}
	service.config.TenantRepository = nativePreviewCountingRepository{Repository: repository, counts: counts, staleFirst: true}
	stalePreviewResponse := performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, tracker.NativeCapacityPreview{NativeClaim: previewClaim, Limit: 2})
	requireNativeStatus(t, stalePreviewResponse, http.StatusOK)
	var stalePreview tracker.NativeCapacityPage
	decodeHubResponse(t, stalePreviewResponse, &stalePreview)
	if len(stalePreview.Items) != 1 || stalePreview.Items[0].WorkItemID != sharedPreview.Items[1].WorkItemID {
		t.Fatalf("preview did not skip stale first candidate: %+v", stalePreview.Items)
	}
	service.config.TenantRepository = repository
	previewResponse := performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, tracker.NativeCapacityPreview{NativeClaim: request, Limit: 1})
	requireNativeStatus(t, previewResponse, http.StatusOK)
	var preview tracker.NativeCapacityPage
	decodeHubResponse(t, previewResponse, &preview)
	if len(preview.Items) != 1 || preview.Items[0].WorkItemID != items[0].WorkItemID {
		t.Fatalf("typed preview returned candidates %+v", preview.Items)
	}
	pagedClaim := request
	pagedClaim.WorkItemID = ""
	pagedClaim.ProviderCandidates = nil
	pagedClaim.Capabilities = []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}
	pagedRequest := tracker.NativeCapacityPreview{NativeClaim: pagedClaim, Limit: 1}
	firstPageResponse := performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, pagedRequest)
	requireNativeStatus(t, firstPageResponse, http.StatusOK)
	var firstPage tracker.NativeCapacityPage
	decodeHubResponse(t, firstPageResponse, &firstPage)
	if len(firstPage.Items) != 1 || firstPage.Next <= 0 {
		t.Fatalf("first typed preview page = %+v", firstPage)
	}
	pagedRequest.After = firstPage.Next
	secondPageResponse := performHubAPIRequest(t, service, http.MethodPost, claimPath+"/preview", secret, pagedRequest)
	requireNativeStatus(t, secondPageResponse, http.StatusOK)
	var secondPage tracker.NativeCapacityPage
	decodeHubResponse(t, secondPageResponse, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].WorkItemID == firstPage.Items[0].WorkItemID {
		t.Fatalf("second typed preview page = %+v after %+v", secondPage, firstPage)
	}
	response := performHubAPIRequest(t, service, http.MethodPost, claimPath, secret, request)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.ProviderReservation == nil || lease.ProviderReservation.Requirement != requirement || lease.ProviderReservation.Report.SharedAccountAlias != report.SharedAccountAlias {
		t.Fatalf("claim provider reservation=%+v", lease.ProviderReservation)
	}
	replay := performHubAPIRequest(t, service, http.MethodPost, claimPath, secret, request)
	requireNativeStatus(t, replay, http.StatusOK)
	var replayed tracker.NativeLease
	decodeHubResponse(t, replay, &replayed)
	if replayed.ID != lease.ID || replayed.ProviderReservation == nil || replayed.ProviderReservation.Requirement != lease.ProviderReservation.Requirement || replayed.ProviderReservation.Report.AccountAlias != lease.ProviderReservation.Report.AccountAlias || replayed.ProviderReservation.Report.SharedAccountAlias != lease.ProviderReservation.Report.SharedAccountAlias {
		t.Fatalf("claim replay changed lease/reservation: first=%+v replay=%+v", lease, replayed)
	}
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	eventPath := base + "/work-items/" + string(items[0].WorkItemID) + "/events"
	started := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: "native-run-start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: descriptor.ID, Sequence: 1,
			Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: report.Backend, Model: report.Models[0]}},
	}
	startedAt := clock.value.Truncate(time.Second).Add(123456789 * time.Nanosecond)
	started.OccurredAt = startedAt
	clock.value = startedAt.Add(2 * time.Second)
	finishedAt := clock.value
	finished := started
	finished.IdempotencyKey = "native-run-finish"
	finished.Type = "run.finished"
	finished.OccurredAt = time.Time{}
	finished.Data.Sequence = 2
	finished.Data.Outcome = "succeeded"
	finished.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "example-model", Input: 10, Output: 3, BillingMode: "metered", CostEstimate: 0.01}}
	finished.PriorEvents = []tracker.NativeRunEvent{started}
	finishResponse := performHubAPIRequest(t, service, http.MethodPost, eventPath, secret, finished)
	requireNativeStatus(t, finishResponse, http.StatusOK)
	var startedAttempt leasestore.Attempt
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		startedAttempt, err = r.Attempt(t.Context(), string(started.Data.AttemptID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var terminalAttempt leasestore.Attempt
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		terminalAttempt, err = r.Attempt(t.Context(), string(started.Data.AttemptID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if terminalAttempt.Status != "succeeded" || terminalAttempt.Sequence != 2 || !terminalAttempt.StartedAt.Equal(startedAt) || !startedAttempt.StartedAt.Equal(startedAt) {
		t.Fatalf("finished attempt = %+v, started %+v", terminalAttempt, startedAttempt)
	}
	corrected := finished
	corrected.IdempotencyKey = "native-run-usage-correction"
	corrected.Data.Sequence = 3
	corrected.Data.Usage = append([]tracker.NativeUsage(nil), finished.Data.Usage...)
	corrected.Data.Usage[0].Input = 14
	corrected.PriorEvents = nil
	clock.value = finishedAt.Add(time.Minute)
	correctionResponse := performHubAPIRequest(t, service, http.MethodPost, eventPath, secret, corrected)
	requireNativeStatus(t, correctionResponse, http.StatusOK)
	var correctedAttempt leasestore.Attempt
	var costRevision billingstore.CostRevision
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		correctedAttempt, err = r.Attempt(t.Context(), string(started.Data.AttemptID))
		if err != nil {
			return err
		}
		costRevision, err = r.BillingCostRevision(t.Context(), billingstore.CostSource{Provider: "codex", ProviderAccount: "runner", SourceID: string(started.Data.AttemptID) + ":example-model", From: terminalAttempt.StartedAt, To: finishedAt})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if correctedAttempt.Status != "succeeded" || correctedAttempt.Sequence != 3 || !correctedAttempt.UpdatedAt.Equal(terminalAttempt.UpdatedAt) || correctedAttempt.PayloadObjectKey == terminalAttempt.PayloadObjectKey || costRevision.Revision != 3 {
		t.Fatalf("usage correction changed attempt lifecycle or missed cost receipt: before=%+v after=%+v cost=%+v", terminalAttempt, correctedAttempt, costRevision)
	}
	blockedResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue-blocked"}, Title: "Example ineligible issue", Body: "detent:no-dispatch", State: "Todo"})
	requireNativeStatus(t, blockedResponse, http.StatusOK)
	var blockedIssue tracker.NativeIssue
	decodeHubResponse(t, blockedResponse, &blockedIssue)
	blockedItems := make([]tracker.NativeIssue, 100)
	blockedItems[0] = blockedIssue
	for index := 1; index < len(blockedItems); index++ {
		response := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("issue-blocked-%03d", index)}, Title: "Example ineligible issue", Body: "detent:no-dispatch", State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &blockedItems[index])
	}
	priority := int64(0)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		for index, blocked := range blockedItems {
			if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: organization.ID, ProjectID: project.ID, WorkItemID: blocked.WorkItemID, Scope: string(project.ID), State: "Todo", Rank: fmt.Sprintf("blocked-%03d", index), Priority: &priority, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unqualifiedRequest := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: "machine_example", SessionID: "session_unqualified", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	_, err = service.claimNativeIssueTenant(t.Context(), scope, unqualifiedRequest, 5*time.Minute)
	if !errors.Is(err, ErrNoClaimableWork) {
		t.Fatalf("first bounded claim scan error=%v, want no claimable work after advancing cursor", err)
	}
	var cursor claimstore.SelectionCursor
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		cursor, err = r.SelectionCursor(t.Context(), "runner_example", project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cursor.Position.WorkItemID == "" {
		t.Fatal("first bounded claim scan did not persist its position")
	}
	unqualified, err := service.claimNativeIssueTenant(t.Context(), scope, unqualifiedRequest, 5*time.Minute)
	if err != nil {
		t.Fatalf("unqualified claim did not advance past the first 100 ineligible candidates: %v", err)
	}
	var expectedIssueID int64
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(t.Context(), project.ID, items[1].WorkItemID)
		if err != nil {
			return err
		}
		expectedIssueID = item.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if unqualified.IssueID != expectedIssueID {
		t.Fatalf("unqualified claim selected issue %d, want eligible item after the first 100 candidates %d", unqualified.IssueID, expectedIssueID)
	}
	wrapRequest := unqualifiedRequest
	wrapRequest.SessionID = "session_wrap"
	if _, err := service.claimNativeIssueTenant(t.Context(), scope, wrapRequest, 5*time.Minute); !errors.Is(err, ErrNoClaimableWork) {
		t.Fatalf("claim after cursor end error=%v, want no claimable work", err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		cursor, err = r.SelectionCursor(t.Context(), "runner_example", project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cursor.Position.WorkItemID != "" {
		t.Fatalf("exhausted claim cursor did not wrap: %+v", cursor.Position)
	}
	originalSourceRevision := cursor.SourceRevision
	changedSourceRequest := unqualifiedRequest
	changedSourceRequest.SessionID = "session_changed_query"
	changedSourceRequest.WorkflowStates = []string{"Todo"}
	_, err = service.claimNativeIssueTenant(t.Context(), scope, changedSourceRequest, 5*time.Minute)
	if !errors.Is(err, ErrNoClaimableWork) {
		t.Fatalf("claim after query source change error=%v, want no claimable work", err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		cursor, err = r.SelectionCursor(t.Context(), "runner_example", project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cursor.SourceRevision == originalSourceRevision || cursor.Position.WorkItemID == "" {
		t.Fatalf("claim cursor did not reset and advance for changed query: %+v", cursor)
	}
	results := make(chan bool, 2)
	errorsFound := make(chan error, 2)
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	for index := range 2 {
		candidate := cursor
		candidate.Position.Rank += fmt.Sprintf("-cas-%d", index)
		go func(selection claimstore.SelectionCursor) {
			ready <- struct{}{}
			<-start
			var written bool
			err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				written, err = w.PutSelectionCursor(t.Context(), selection, selection.Revision)
				return err
			})
			if err != nil {
				errorsFound <- err
				return
			}
			results <- written
		}(candidate)
	}
	<-ready
	<-ready
	close(start)
	twins := 0
	for range 2 {
		select {
		case err := <-errorsFound:
			t.Fatalf("concurrent cursor compare-and-swap: %v", err)
		case written := <-results:
			if written {
				twins++
			}
		}
	}
	if twins != 1 {
		t.Fatalf("concurrent cursor compare-and-swap winners=%d, want exactly one", twins)
	}
	secondProject := projectstore.Record{Core: corestore.Project{OrganizationID: organization.ID, ID: "prj_cursor_second", Name: "Cursor example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), secondProject); err != nil {
			return err
		}
		second := claimstore.SelectionCursor{OrganizationID: organization.ID, RunnerID: "runner_example", ProjectID: secondProject.Core.ID, SourceRevision: strings.Repeat("c", 64)}
		written, err := w.PutSelectionCursor(t.Context(), second, 0)
		if err != nil {
			return err
		}
		if !written {
			return errors.New("second project selection cursor was not created")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		first, err := r.SelectionCursor(t.Context(), "runner_example", project.ID)
		if err != nil {
			return err
		}
		second, err := r.SelectionCursor(t.Context(), "runner_example", secondProject.Core.ID)
		if err != nil {
			return err
		}
		if first.ProjectID == second.ProjectID || first.Revision == 0 || second.Revision != 1 {
			return fmt.Errorf("project selection cursors were not independent: first=%+v second=%+v", first, second)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: unqualified.LeaseID, FencingToken: int64(unqualified.FencingToken)}, "test_release")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitResponse := performHubAPIRequest(t, service, http.MethodGet, claimPath+"/wait", secret, nil)
	requireNativeStatus(t, waitResponse, http.StatusOK)
	second := request
	second.WorkItemID = items[1].WorkItemID
	second.SessionID = "session_second"
	second.ProviderCandidates = nil
	second.Capabilities = []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, claimPath, secret, second), http.StatusConflict)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(t.Context(), project.ID, items[1].WorkItemID)
		if err != nil {
			return err
		}
		if _, err := r.ActiveLeaseForIssue(t.Context(), item.ID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("provider refusal left active issue lease, result error=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.ProviderReservation(t.Context(), tracker.LeaseID(lease.ID))
		if err != nil {
			return err
		}
		if stored.Reservation.Requirement != requirement || stored.Reservation.Report.AccountAlias != report.AccountAlias {
			t.Fatalf("stored provider reservation=%+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeClaimPendingLandingRequiresLatestMatchingVersionReceipt(t *testing.T) {
	base := nativeClaimPlan{
		changeRecord:        eventstore.Change{ID: "change_example", CurrentVersionID: new("version_example")},
		changeVersion:       eventstore.Version{ID: "version_example", HeadSHA: "head_example"},
		latestAttemptFound:  true,
		latestRunEventFound: true,
		latestRunEvent:      eventstore.Event{LandingWaiting: true, LandingChangeID: "change_example", LandingVersionID: "version_example", LandingHeadSHA: "head_example"},
	}
	for _, test := range []struct {
		name   string
		mutate func(*nativeClaimPlan)
		want   bool
	}{
		{name: "exact current landing receipt", want: true},
		{name: "not waiting", mutate: func(plan *nativeClaimPlan) { plan.latestRunEvent.LandingWaiting = false }},
		{name: "different change", mutate: func(plan *nativeClaimPlan) { plan.latestRunEvent.LandingChangeID = "change_other" }},
		{name: "different version", mutate: func(plan *nativeClaimPlan) { plan.latestRunEvent.LandingVersionID = "version_other" }},
		{name: "different head", mutate: func(plan *nativeClaimPlan) { plan.latestRunEvent.LandingHeadSHA = "head_other" }},
		{name: "historical version", mutate: func(plan *nativeClaimPlan) { plan.changeRecord.CurrentVersionID = new("version_new") }},
		{name: "missing attempt", mutate: func(plan *nativeClaimPlan) { plan.latestAttemptFound = false }},
		{name: "missing event", mutate: func(plan *nativeClaimPlan) { plan.latestRunEventFound = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := base
			currentVersion := *base.changeRecord.CurrentVersionID
			plan.changeRecord.CurrentVersionID = &currentVersion
			if test.mutate != nil {
				test.mutate(&plan)
			}
			if got := nativeClaimPendingLanding(plan); got != test.want {
				t.Fatalf("pending landing = %t, want %t", got, test.want)
			}
		})
	}
}

func TestTenantClaimUsesConcurrencyNotSubscriptionQuotaState(t *testing.T) {
	repository := projectRecordPostgres(t)
	organization := tracker.OrganizationID("org_provider_claim")
	project := tracker.ProjectID("prj_provider_claim")
	now := time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)
	secret := "example_runner_secret"
	tokenHash := apikey.HashToken(secret)
	report := providercapacity.Report{
		Provider: "example-provider", Backend: "example-backend", AccountAlias: "example-account", SharedAccountAlias: "shared-account",
		Billing: "subscription", Models: []string{"example-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now,
	}
	requirement := providercapacity.Requirement{Role: "code", Backend: report.Backend, Model: report.Models[0]}
	var active leasestore.Lease
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "example", Local: true, CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_example"}); err != nil {
			return err
		}
		expires := now.Add(time.Hour)
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_runner", Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", ProjectAccess: "selected", Kind: "service", CreatedAt: now.Add(-time.Minute), UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Profile: "native", CreatedAt: now}); err != nil {
			return err
		}
		for index, id := range []tracker.NativeWorkItemID{"wi_active", "wi_candidate"} {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: id, Number: int64(index + 1), Title: "Example issue", CreatedAt: now, NativeCreatedAt: now}); err != nil {
				return err
			}
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_runner", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "organization", Operations: []string{runnerauth.Claim}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_example", Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		identity := runnerstore.Identity{OrganizationID: organization, RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_runner", EnrollmentID: "enrollment_runner", Scope: "organization", State: "active", Operations: []string{runnerauth.Claim}, CapacityLimit: 2, ReportedCapacity: 2, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}
		if err := w.CreateRunnerIdentity(t.Context(), identity); err != nil {
			return err
		}
		if err := w.ReplaceRunnerProviderReports(t.Context(), identity.RunnerID, []providercapacity.Report{report}); err != nil {
			return err
		}
		item, err := w.WorkItem(t.Context(), project, "wi_active")
		if err != nil {
			return err
		}
		active, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_example", SessionID: "session_active", LeaseID: "lease_active", TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: active.LeaseID, RunnerID: identity.RunnerID, AssignedAt: now}); err != nil {
			return err
		}
		return w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: organization, LeaseID: tracker.LeaseID(active.LeaseID), Reservation: providercapacity.Reservation{Requirement: requirement, Report: report, Reason: "Current provider account capacity is available"}})
	}); err != nil {
		t.Fatal(err)
	}
	request := tracker.NativeClaim{ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: "wi_candidate", Revision: 1, Requirement: requirement}}}
	selectReservation := func() (providercapacity.Reservation, error) {
		var selected providercapacity.Reservation
		err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			identity, err := w.RunnerIdentity(t.Context(), "runner_example")
			if err != nil {
				return err
			}
			selected, err = selectTenantProviderReservation(t.Context(), w, identity, request, "wi_candidate", now)
			return err
		})
		return selected, err
	}
	if _, err := selectReservation(); !isProviderWait(err) {
		t.Fatalf("selection with a full shared account returned error %v", err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: active.LeaseID, FencingToken: active.FencingToken}, "released")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stale := report
	stale.ObservedAt = now.Add(-3 * providercapacity.MaxAge)
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerProviderReports(t.Context(), "runner_example", []providercapacity.Report{stale})
	}); err != nil {
		t.Fatal(err)
	}
	if selected, err := selectReservation(); err != nil || selected.Requirement != requirement {
		t.Fatalf("selection with stale subscription evidence = %+v, error %v", selected, err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerProviderReports(t.Context(), "runner_example", []providercapacity.Report{report})
	}); err != nil {
		t.Fatal(err)
	}
	selected, err := selectReservation()
	if err != nil || selected.Requirement != requirement || selected.Report.SharedAccountAlias != report.SharedAccountAlias {
		t.Fatalf("selection after active reservation release = %+v, error %v", selected, err)
	}
	for _, availability := range []string{"unknown", "exhausted"} {
		t.Run(availability, func(t *testing.T) {
			current := report
			current.Availability = availability
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerProviderReports(t.Context(), "runner_example", []providercapacity.Report{current})
			}); err != nil {
				t.Fatal(err)
			}
			selected, err := selectReservation()
			if err != nil || selected.Requirement != requirement {
				t.Fatalf("selection with %s subscription evidence = %+v, error %v", availability, selected, err)
			}
		})
	}
}

func TestNativeClaimContractRefusalDoesNotFailCandidateScan(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	var logs bytes.Buffer
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, Clock: clock.Now, SecretKeys: secretTestKeys(t, "1", "1"),
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	for _, name := range []string{"Example organization one", "Example organization two"} {
		t.Run(name, func(t *testing.T) {
			organization, err := service.createNativeOrganizationFor(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			seedPostgresHubAdmin(t, repository, service, organization)
			projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
			projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "contract-scan-project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
			requireNativeStatus(t, projectResponse, http.StatusCreated)
			var project tracker.NativeProject
			decodeHubResponse(t, projectResponse, &project)
			descriptor := hubTestPolicy()
			policyRevision, err := eventstore.NewContent(service.config.RunDetails).PrepareProjectPolicy(t.Context(), organization.ID, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			policyRevision.ProjectID = project.ID
			policyRevision.ApprovedBy = "test_hub_admin"
			policyRevision.ApprovedAt = clock.Now().UTC()
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
			}); err != nil {
				t.Fatal(err)
			}
			itemsPath := projectPath + "/" + string(project.ID) + "/work-items"
			issueResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "contract-scan-issue"}, Title: "Example issue", Body: "Missing required sections\n\n<!-- detent:no-dispatch -->", State: "Todo"})
			requireNativeStatus(t, issueResponse, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, issueResponse, &issue)
			startedAt := clock.Now().UTC()
			runnerID := "runner_contract_scan_" + string(organization.ID)
			machineID := tracker.MachineID("machine_contract_scan_" + string(organization.ID))
			credentialID := "credential_contract_scan_" + string(organization.ID)
			enrollmentID := "enrollment_contract_scan_" + string(organization.ID)
			secret := "example_runner_contract_scan_secret_" + string(organization.ID)
			tokenHash := apikey.HashToken(secret)
			expires := startedAt.Add(time.Hour)
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: startedAt, UpdatedAt: startedAt, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, CreatedAt: startedAt, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, machineID, startedAt); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: string(machineID), Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runnerID, MachineID: machineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: startedAt, ExpiresAt: expires, LastHeartbeatAt: startedAt}); err != nil {
					return err
				}
				return w.UpdateRunnerCapacityConfig(t.Context(), runnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: startedAt})
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: credentialID, Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runnerID, MachineID: machineID}, OrganizationID: organization.ID}}}
			request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: machineID, SessionID: "contract-scan-session", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}

			targeted := request
			_, err = service.claimNativeIssueTenant(t.Context(), scope, targeted, 5*time.Minute)
			var refusal *nativeError
			if !errors.As(err, &refusal) || refusal.Code != "issue_contract_unsatisfied" {
				t.Fatalf("targeted invalid claim error = %v", err)
			}
			request.WorkItemID = ""
			logs.Reset()
			_, err = service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
			if !errors.Is(err, ErrNoClaimableWork) {
				t.Errorf("invalid-only scan error = %v, want no claimable work", err)
			}
			for _, field := range []string{string(organization.ID), string(project.ID), string(issue.WorkItemID), `"runner_id":"[redacted]"`, "issue_contract_unsatisfied"} {
				if !strings.Contains(logs.String(), field) {
					t.Errorf("refusal evidence missing %q: %s", field, logs.String())
				}
			}
			validResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "valid-issue"}, Title: "Example valid issue", Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, validResponse, http.StatusOK)
			var valid tracker.NativeIssue
			decodeHubResponse(t, validResponse, &valid)
			request.WorkflowStates = []string{"Todo"}
			lease, err := service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
			if err != nil {
				t.Fatalf("mixed scan: %v", err)
			}
			if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(t.Context(), project.ID, valid.WorkItemID)
				if err != nil {
					return err
				}
				if lease.IssueID != item.ID {
					t.Errorf("claimed issue %d, want valid issue %d", lease.IssueID, item.ID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
