package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func humanWorkflowFixtureStates() []tracker.NativeState {
	states := nativeFixtureStates()
	states[2].Transitions = append(states[2].Transitions, "Rework", "Cancelled")
	states[0].Transitions = append(states[0].Transitions, "Rework", "Cancelled")
	return append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true})
}

func verifyPostgresHumanWorkflow(t *testing.T, service *Service, admin *sql.DB, project tracker.NativeProject, claim tracker.NativeClaim, owner, reporter, worker string, clock *atomic.Int64) {
	t.Helper()
	repository := service.config.TenantRepository
	base := "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID)
	create := func(token, key, state string) tracker.NativeIssue {
		t.Helper()
		response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items", token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example human workflow", Type: "bug", Body: workItemRecordBody, State: state})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	transition := func(issue tracker.NativeIssue, key, state string) tracker.Transition {
		return tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"}
	}
	workflow := func(issue tracker.NativeIssue) string {
		return base + "/work-items/" + string(issue.WorkItemID) + "/workflow"
	}
	issue := create(owner, "human-completed", "Done")
	var oldLease leasestore.Lease
	var oldAttempt leasestore.Attempt
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		oldLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(claim.MachineID), SessionID: "human-completed-session", LeaseID: "human-completed-lease", TTL: time.Minute})
		if err != nil {
			return err
		}
		fence := leasestore.Fence{LeaseID: oldLease.LeaseID, FencingToken: oldLease.FencingToken}
		oldAttempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "human-completed-attempt", RunID: "human-completed-run", Fence: fence, Sequence: 1, Status: "running", StartedAt: service.config.now(), PayloadObjectKey: "example/attempt"})
		if err != nil {
			return err
		}
		oldAttempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: oldAttempt.ID, RunID: oldAttempt.RunID, Fence: fence, Sequence: 2, Status: "succeeded", PayloadObjectKey: "example/completed"})
		if err != nil {
			return err
		}
		_, err = w.Release(t.Context(), fence, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if oldAttempt.WorkItemRevision != int64(issue.Revision) {
		t.Fatal("succeeded attempt must cover the pre-command current revision")
	}
	dispatchCursor := service.notifications.dispatchCursor(dispatchNotificationKey(project.OrganizationID))
	request := transition(issue, "human-rework", "Rework")
	response := performHubAPIRequest(t, service, http.MethodPost, workflow(issue), owner, request)
	requireNativeStatus(t, response, http.StatusOK)
	if service.notifications.dispatchCursor(dispatchNotificationKey(project.OrganizationID)) == dispatchCursor {
		t.Fatal("human Rework did not wake dispatch waiters")
	}
	var changed tracker.NativeIssue
	decodeHubResponse(t, response, &changed)
	if changed.State != "Rework" || changed.Revision != issue.Revision+1 {
		t.Fatalf("human Rework state=%s revision=%d", changed.State, changed.Revision)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(issue), owner, request), http.StatusOK)
	stale := transition(issue, "human-stale", "Rework")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(issue), owner, stale), http.StatusConflict)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(changed.Revision) || stringValue(row.WorkflowStateName) != "Rework" {
			return fmt.Errorf("current human intent was not stored")
		}
		version, err := r.CollaborationVersion(t.Context(), string(issue.WorkItemID), row.Revision)
		if err != nil {
			return err
		}
		raw, err := eventstore.NewContent(service.config.RunDetails).CollaborationVersion(t.Context(), version)
		if err != nil {
			return err
		}
		var stored tracker.NativeIssue
		if err := json.Unmarshal(raw, &stored); err != nil {
			return err
		}
		if stored.State != "Rework" || stored.Revision != changed.Revision {
			return fmt.Errorf("current collaboration version differs")
		}
		events, err := r.Events(t.Context(), project.ID, issue.WorkItemID, 0, 100)
		if err != nil {
			return err
		}
		found := false
		for _, event := range events {
			if event.Type == "workflow.transitioned" && event.EventRevision == row.Revision && event.FromState == "Done" && event.ToState == "Rework" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("current human collaboration event missing")
		}
		attempt, err := r.LatestAttempt(t.Context(), row.ID)
		if err != nil {
			return err
		}
		if attempt.ID != oldAttempt.ID || attempt.Status != "succeeded" || attempt.Sequence != oldAttempt.Sequence {
			return fmt.Errorf("historical succeeded attempt was changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	verifyPostgresHumanTenantIsolation(t, service, project, issue, owner, clock)
	next := claim
	next.ProviderCandidates = append([]tracker.NativeCapacityCandidate(nil), claim.ProviderCandidates...)
	next.WorkItemID, next.SessionID = issue.WorkItemID, "human-rework-session"
	next.ProviderCandidates[0].WorkItemID, next.ProviderCandidates[0].Revision = changed.WorkItemID, changed.Revision
	claimed := performHubAPIRequest(t, service, http.MethodPost, base+"/claims", worker, next)
	requireNativeStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimed, &lease)
	if lease.WorkItemID != issue.WorkItemID || lease.ID == tracker.LeaseID(oldLease.LeaseID) || lease.FencingToken <= tracker.FencingToken(oldLease.FencingToken) {
		t.Fatal("Rework did not claim a new fenced execution")
	}
	currentAttemptID := newNativeID("attempt")
	started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "human-current-start"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: currentAttemptID, RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: claim.PolicyID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example-backend", Model: "example-model"}}}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, started), http.StatusOK)
	for _, state := range []string{"Todo", "Cancelled"} {
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, transition(changed, "human-active-"+state, state)), http.StatusConflict)
	}
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Lease(t.Context(), string(lease.ID))
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), currentAttemptID)
		if err != nil {
			return err
		}
		if current.ReleasedAt != nil || attempt.Status != "running" || attempt.Sequence != 1 {
			return fmt.Errorf("human refusal changed current execution")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	for _, test := range []struct {
		name  string
		lease tracker.LeaseID
		fence tracker.FencingToken
	}{
		{"released", tracker.LeaseID(oldLease.LeaseID), tracker.FencingToken(oldLease.FencingToken)},
		{"partial", "", tracker.FencingToken(oldLease.FencingToken)},
	} {
		supplied := transition(changed, "human-stale-fence-"+test.name, "Todo")
		supplied.LeaseID, supplied.FencingToken = test.lease, test.fence
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, supplied), http.StatusConflict)
	}
	const revokedToken = "example_revoked_human_token"
	now := service.config.now()
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: project.OrganizationID, ID: "credential_human_revoked", Name: "Example human", Hash: apikey.HashToken(revokedToken), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_owner", MembershipID: "membership_user_owner", KeyScope: "admin", ProjectAccess: "all", Kind: "personal"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, status, err := service.authenticateAccessHash(t.Context(), apikey.HashToken(revokedToken), "", ""); err != nil || status != http.StatusOK {
		t.Fatalf("authenticate before revocation: %d %v", status, err)
	}
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_human_revoked", service.config.now())
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), revokedToken, transition(changed, "human-revoked", "Todo")), http.StatusUnauthorized)
	expired := create(owner, "human-expired", "Done")
	var expiredLease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), project.ID, expired.WorkItemID)
		if err != nil {
			return err
		}
		expiredLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(claim.MachineID), SessionID: "human-expired-session", LeaseID: "human-expired-lease", TTL: time.Minute})
		if err != nil {
			return err
		}
		_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "human-expired-attempt", RunID: "human-expired-run", Fence: leasestore.Fence{LeaseID: expiredLease.LeaseID, FencingToken: expiredLease.FencingToken}, Sequence: 1, Status: "running", PayloadObjectKey: "example/expired"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=acquired_at-interval '3 minutes',renewed_at=renewed_at-interval '2 minutes',expires_at=expires_at-interval '2 minutes' WHERE organization_id=$1 AND lease_id=$2`, project.OrganizationID, expiredLease.LeaseID); err != nil {
		t.Fatal(err)
	}
	expiredRequest := transition(expired, "human-expired-cancel", "Cancelled")
	expiredRequest.LeaseID, expiredRequest.FencingToken = tracker.LeaseID(expiredLease.LeaseID), tracker.FencingToken(expiredLease.FencingToken)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(expired), owner, expiredRequest), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), expiredLease.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), "human-expired-attempt")
		if err != nil {
			return err
		}
		if lease.ReleasedAt == nil || lease.ReleaseReason != "expired" || attempt.Status != "cancelled" {
			return fmt.Errorf("expired cancellation did not use fenced lease owner")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	verifyPostgresHumanReceiptExpiry(t, service, project.OrganizationID, base, owner, clock)
	own := create(reporter, "human-reporter-own", "Triage")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(own), reporter, transition(own, "human-own-cancel", "Cancelled")), http.StatusOK)
	denied := performHubAPIRequest(t, service, http.MethodPost, workflow(changed), reporter, transition(changed, "human-foreign-cancel", "Cancelled"))
	if denied.Code != http.StatusNotFound && denied.Code != http.StatusForbidden {
		t.Fatalf("foreign reporter cancel=%d %s", denied.Code, denied.Body.String())
	}
}

func verifyPostgresHumanTenantIsolation(t *testing.T, first *Service, project tracker.NativeProject, original tracker.NativeIssue, firstToken string, clock *atomic.Int64) {
	t.Helper()
	const organization tracker.OrganizationID = "org_human_other"
	const token = "example_other_human_token"
	repository := first.config.TenantRepository
	seedStartupOrganization(t, repository, organization)
	cfg := first.config
	hosted := *cfg.Hosted
	hosted.OrganizationID, hosted.WorkOSOrganizationID = string(organization), "provider_org_human_other"
	cfg.Hosted = &hosted
	if _, err := cfg.Hosted.Provider.CreateMembership(t.Context(), "user_human_other", "provider_org_human_other", "owner"); err != nil {
		t.Fatal(err)
	}
	now := cfg.now()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project.ID, Name: "Other example project", Slug: "other-example", Profile: "native"}, States: humanWorkflowFixtureStates()}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_human_other", Name: "Other owner", Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_human_other", MembershipID: "membership_user_human_other", KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_human_other", Email: "other@example.test", MembershipID: "membership_user_human_other", Role: "owner", Active: true, PrincipalID: "credential_human_other", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_human_other", ProjectID: project.ID, CanWrite: true}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_human_other", ProjectID: project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	base := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID) + "/work-items"
	created := performHubAPIRequest(t, second, http.MethodPost, base, token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other-human-create"}, Title: "Other human issue", Type: "bug", Body: workItemRecordBody, State: "Done"})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "other-human-rework"}, ExpectedRevision: issue.Revision, State: "Rework", Reason: "user_requested"}
	requireNativeStatus(t, performHubAPIRequest(t, second, http.MethodPost, base+"/"+string(issue.WorkItemID)+"/workflow", token, request), http.StatusOK)
	verifyPostgresHumanReceiptExpiry(t, second, organization, "/api/v2/organizations/"+string(organization)+"/projects/"+string(project.ID), token, clock)
	for _, test := range []struct {
		service     *Service
		path, token string
	}{
		{first, base + "/" + string(original.WorkItemID) + "/workflow", firstToken},
		{second, "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID) + "/work-items/" + string(original.WorkItemID) + "/workflow", token},
	} {
		response := performHubAPIRequest(t, test.service, http.MethodPost, test.path, test.token, request)
		if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound && response.Code != http.StatusUnauthorized {
			t.Fatalf("wrong-tenant transition=%d %s", response.Code, response.Body.String())
		}
	}
}

func verifyPostgresHumanReceiptExpiry(t *testing.T, service *Service, organization tracker.OrganizationID, base, token string, clock *atomic.Int64) {
	t.Helper()
	start := service.config.now().UTC().Truncate(time.Microsecond)
	clock.Store(start.UnixNano())
	defer clock.Store(0)
	cfg := service.config
	created := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items", token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-expiry-create"}, Title: "Example receipt expiry", Type: "bug", Body: workItemRecordBody, State: "Done"})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	path := base + "/work-items/" + string(issue.WorkItemID) + "/workflow"
	request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-expiry-rework"}, ExpectedRevision: issue.Revision, State: "Rework", Reason: "user_requested"}
	first := performHubAPIRequest(t, service, http.MethodPost, path, token, request)
	requireNativeStatus(t, first, http.StatusOK)
	var changed tracker.NativeIssue
	decodeHubResponse(t, first, &changed)
	clock.Store(start.Add(receiptstore.ReplayRetention - time.Nanosecond).UnixNano())
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, token, request), http.StatusOK)
	newerRequest := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-expiry-newer"}, ExpectedRevision: changed.Revision, State: "Todo", Reason: "user_requested"}
	newerResponse := performHubAPIRequest(t, service, http.MethodPost, path, token, newerRequest)
	requireNativeStatus(t, newerResponse, http.StatusOK)
	var newer tracker.NativeIssue
	decodeHubResponse(t, newerResponse, &newer)
	var receipt eventstore.Receipt
	var before int64
	if err := cfg.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.(receiptstore.RetentionReader).ReceiptExpiryPage(t.Context(), start.Add(receiptstore.ReplayRetention+time.Nanosecond), receiptstore.MaxRetentionBatch)
		if err != nil {
			return err
		}
		for _, row := range page {
			if row.CommandKey == request.IdempotencyKey {
				receipt = row
			}
		}
		before, err = r.ArchiveBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(receiptstore.ReplayRetention).UnixNano())
	expired := performHubAPIRequest(t, service, http.MethodPost, path, token, request)
	requireNativeStatus(t, expired, http.StatusGone)
	var after int64
	if receipt.CommandKey == "" || !receipt.CreatedAt.Equal(start) || receipt.ResponseReplayUntilNs != start.Add(receiptstore.ReplayRetention).UnixNano() {
		t.Fatalf("receipt lifecycle metadata = %+v", receipt)
	}
	if err := cfg.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Receipt(t.Context(), receipt.ActorID, receipt.Operation, receipt.CommandKey)
		if err != nil {
			return err
		}
		if !stored.ResponseExpired || stored.RequestHash != receipt.RequestHash || stored.PayloadSHA256 != receipt.PayloadSHA256 {
			return fmt.Errorf("expiry lost tombstone identity")
		}
		row, err := r.WorkItem(t.Context(), newer.ProjectID, newer.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(newer.Revision) || stringValue(row.WorkflowStateName) != "Todo" {
			return fmt.Errorf("expiry overwrote newer workflow intent")
		}
		after, err = r.ArchiveBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after > before {
		t.Fatalf("expiry increased archive charge: %d -> %d", before, after)
	}
	if err := cfg.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		changed, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, start.Add(receiptstore.ReplayRetention+time.Nanosecond))
		if changed {
			return fmt.Errorf("owner sweep expired tombstone twice")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, token, request), http.StatusGone)
	if err := cfg.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		charged, err := r.ArchiveBytes(t.Context())
		if err == nil && charged != after {
			return fmt.Errorf("replay or sweep changed archive charge: %d -> %d", after, charged)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
