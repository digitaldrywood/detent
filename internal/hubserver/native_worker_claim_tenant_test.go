package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
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

func TestNativeClaimTenantPersistsProviderReservationAtomicallyAndReplaysIt(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)}
	participant := &nativeRunEventParticipantFixture{}
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, Clock: clock.Now,
		NativeRunEvents: participant,
	})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, projectResponse, http.StatusOK)
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
	items := make([]tracker.NativeIssue, 0, 2)
	for _, idempotencyKey := range []string{"issue-first", "issue-second"} {
		response := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: idempotencyKey}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		items = append(items, issue)
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
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: "enrollment_runner", RunnerID: "runner_example", MachineID: "machine_example", TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", "runner_example", "machine_example", now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine_example", Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: "runner_example", MachineID: "machine_example", TokenID: "credential_runner", EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
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
	claimPath := base + "/claims"
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
	var refusal *nativeError
	if !errors.As(err, &refusal) || refusal.Code != "issue_contract_unsatisfied" {
		t.Fatalf("first bounded claim scan error=%v, want issue contract refusal after advancing cursor", err)
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
	if !errors.As(err, &refusal) || refusal.Code != "issue_contract_unsatisfied" {
		t.Fatalf("claim after query source change error=%v, want issue contract refusal", err)
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
