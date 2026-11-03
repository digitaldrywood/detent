package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresWorkerWorkflowTransitionPersistsFencedEventAndReplays(t *testing.T) {
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
	cursorKey := bytes.Repeat([]byte{0x31}, 32)
	cursorEnvelope, err := service.config.SecretKeys.Seal(cursorKey, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: cursorEnvelope, UpdatedAt: clock.Now()}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursorEnvelope.Version, RecordedAt: clock.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	service.localOrganization = string(organization.ID)
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "workflow-project", "name": "Example project", "states": nativeFixtureStates()})
	requireNativeStatus(t, projectResponse, http.StatusOK)
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
	issueResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "workflow-issue"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo", Labels: []string{"example-label"}})
	requireNativeStatus(t, issueResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, issueResponse, &issue)
	startedAt := clock.Now().UTC()
	const runnerID = "runner_workflow"
	const machineID = "machine_workflow"
	const credentialID = "credential_workflow"
	const enrollmentID = "enrollment_workflow"
	const secret = "example_runner_workflow_secret"
	tokenHash := apikey.HashToken(secret)
	expires := startedAt.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: startedAt, UpdatedAt: startedAt, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate}, CreatedAt: startedAt, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, machineID, startedAt); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: machineID, Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runnerID, MachineID: machineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate}, DisplayName: "Example runner", CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: startedAt, ExpiresAt: expires, LastHeartbeatAt: startedAt}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), runnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: startedAt})
	}); err != nil {
		t.Fatal(err)
	}

	secondResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "workflow-second-issue"}, Title: "Second example issue", Body: workItemRecordBody, State: "Todo", Labels: []string{"example-label"}})
	requireNativeStatus(t, secondResponse, http.StatusOK)
	var second tracker.NativeIssue
	decodeHubResponse(t, secondResponse, &second)
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: credentialID, Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runnerID, MachineID: machineID}, OrganizationID: organization.ID}}}
	acquire := func(item tracker.NativeIssue, session string) leasestore.Lease {
		t.Helper()
		var lease leasestore.Lease
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), project.ID, item.WorkItemID)
			if err != nil {
				return err
			}
			lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: machineID, SessionID: session, LeaseID: session + "-lease", TTL: 5 * time.Minute})
			if err != nil {
				return err
			}
			return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization.ID, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: clock.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	lease := acquire(issue, "workflow-first-session")
	secondLease := acquire(second, "workflow-second-session")
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteSearch(t.Context(), "issue:"+string(issue.WorkItemID), int64(issue.Revision))
	}); err != nil {
		t.Fatal(err)
	}
	originalRows := map[tracker.NativeWorkItemID]corestore.WorkItem{}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		for _, item := range []tracker.NativeIssue{issue, second} {
			row, err := r.WorkItem(t.Context(), project.ID, item.WorkItemID)
			if err != nil {
				return err
			}
			originalRows[item.WorkItemID] = row
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	originalDB, originalReader := service.database.db, service.database.reader
	t.Cleanup(func() { service.database.db = originalDB; service.database.reader = originalReader })
	service.database.db = nil
	service.database.reader = nil
	projectGetPath := strings.Replace(nativeBase, ":organization", string(organization.ID), 1)
	projectGetPath = strings.Replace(projectGetPath, ":project", string(project.ID), 1)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, projectGetPath+"/policy", testHubAdminToken, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(issue.WorkItemID), testHubAdminToken, nil), http.StatusOK)
	historyURL := itemsPath + "/" + string(issue.WorkItemID) + "/history?view=timeline"
	historyResponse := performHubAPIRequest(t, service, http.MethodGet, historyURL, testHubAdminToken, nil)
	requireNativeStatus(t, historyResponse, http.StatusOK)
	organizationItemURL := "/api/v2/organizations/" + string(organization.ID) + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, organizationItemURL, testHubAdminToken, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, projectGetPath+"/onboarding", testHubAdminToken, nil), http.StatusServiceUnavailable)
	for _, path := range []string{
		strings.Replace(itemsPath+"/"+string(issue.WorkItemID)+"/history?view=timeline", string(project.ID), "prj_foreign", 1),
		strings.Replace(itemsPath+"/"+string(issue.WorkItemID)+"/history?view=timeline", string(organization.ID), "org_foreign", 1),
	} {
		got := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
		if got.Code != http.StatusNotFound && got.Code != http.StatusForbidden && got.Code != http.StatusUnauthorized {
			t.Fatalf("cross-scope history status=%d", got.Code)
		}
	}
	transition := func(item tracker.NativeIssue, lease leasestore.Lease, key string) tracker.Transition {
		return tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: key, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, ExpectedRevision: item.Revision, State: "In Progress", Reason: "worker_progress"}
	}
	workflowPath := func(item tracker.NativeIssue) string { return itemsPath + "/" + string(item.WorkItemID) + "/workflow" }
	first := transition(issue, lease, "workflow-first")
	clock.Advance(runnerauth.HeartbeatTimeout + time.Second)
	if !clock.Now().After(startedAt.Add(runnerauth.HeartbeatTimeout)) || !lease.ExpiresAt.After(clock.Now()) {
		t.Fatal("held lease must be live after heartbeat freshness expires")
	}
	response := performHubAPIRequest(t, service, http.MethodPost, workflowPath(issue), secret, first)
	requireNativeStatus(t, response, http.StatusOK)
	var changed tracker.NativeIssue
	decodeHubResponse(t, response, &changed)
	if changed.State != "In Progress" || changed.Revision != issue.Revision+1 {
		t.Fatalf("transition state=%s revision=%d", changed.State, changed.Revision)
	}
	replay := performHubAPIRequest(t, service, http.MethodPost, workflowPath(issue), secret, first)
	requireNativeStatus(t, replay, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("receipt replay changed response")
	}
	for _, test := range []struct {
		name    string
		item    tracker.NativeIssue
		request tracker.Transition
	}{
		{"stale revision", issue, transition(issue, lease, "workflow-stale-revision")},
		{"stale fence", second, func() tracker.Transition {
			r := transition(second, secondLease, "workflow-stale-fence")
			r.FencingToken++
			return r
		}()},
		{"other item lease", second, transition(second, lease, "workflow-wrong-item")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := performHubAPIRequest(t, service, http.MethodPost, workflowPath(test.item), secret, test.request)
			if got.Code != http.StatusConflict && got.Code != http.StatusNotFound && got.Code != http.StatusForbidden {
				t.Fatalf("refusal status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
	for _, path := range []string{strings.Replace(workflowPath(second), string(project.ID), "prj_foreign", 1), strings.ReplaceAll(workflowPath(second), string(organization.ID), "org_foreign")} {
		got := performHubAPIRequest(t, service, http.MethodPost, path, secret, transition(second, secondLease, "workflow-cross-scope"))
		if got.Code != http.StatusNotFound && got.Code != http.StatusForbidden && got.Code != http.StatusUnauthorized {
			t.Fatalf("cross-scope status=%d", got.Code)
		}
	}
	sequential := transition(changed, lease, "workflow-sequential")
	sequential.State = "Todo"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflowPath(issue), secret, sequential), http.StatusOK)
	secondRequest := transition(second, secondLease, "workflow-second")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflowPath(second), secret, secondRequest), http.StatusOK)
	for _, test := range []struct {
		item    tracker.NativeIssue
		request tracker.Transition
	}{{issue, first}, {second, secondRequest}} {
		if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
			current, err := r.WorkItem(t.Context(), project.ID, test.item.WorkItemID)
			if err != nil {
				return err
			}
			wantRevision, wantState := int64(test.item.Revision+1), "In Progress"
			if test.item.WorkItemID == issue.WorkItemID {
				wantRevision++
				wantState = "Todo"
			}
			if current.Revision != wantRevision || current.WorkflowStateName == nil || *current.WorkflowStateName != wantState {
				t.Fatal("typed item differs")
			}
			original := originalRows[test.item.WorkItemID]
			equalPointer := func(a, b *string) bool {
				if a == nil || b == nil {
					return a == b
				}
				return *a == *b
			}
			if !equalPointer(current.BodyObjectKey, original.BodyObjectKey) || !equalPointer(current.IssueContractObjectKey, original.IssueContractObjectKey) || !equalPointer(current.ReportContextObjectKey, original.ReportContextObjectKey) {
				t.Fatal("transition changed existing content references")
			}

			version, err := r.CollaborationVersion(t.Context(), string(test.item.WorkItemID), current.Revision)
			if err != nil {
				return err
			}
			raw, err := eventstore.NewContent(service.config.RunDetails).CollaborationVersion(t.Context(), version)
			if err != nil {
				return err
			}
			var stored tracker.NativeIssue
			if json.Unmarshal(raw, &stored) != nil || stored.Body != test.item.Body || stored.State != wantState || !slices.Equal(stored.Labels, test.item.Labels) {
				t.Fatal("collaboration version changed body or state")
			}

			events, err := r.Events(t.Context(), project.ID, test.item.WorkItemID, 0, 100)
			if err != nil {
				return err
			}
			transitions := 0
			for _, event := range events {
				if event.Type == "workflow.transitioned" {
					transitions++
					if event.EventRevision <= int64(test.item.Revision) || event.EventRevision > current.Revision || event.FromState == event.ToState {
						t.Fatal("transition event facts differ")
					}
				}
			}
			if transitions != int(current.Revision-int64(test.item.Revision)) {
				t.Fatalf("transition events=%d", transitions)
			}
			_, err = r.Receipt(t.Context(), credentialID, nativeOperation(scope, "POST", "/work-items/"+string(test.item.WorkItemID)+"/workflow"), test.request.IdempotencyKey)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if untilExpiry := lease.ExpiresAt.Add(time.Second).Sub(clock.Now()); untilExpiry > 0 {
		clock.Advance(untilExpiry)
	}
	expiredLeaseRequest := transition(issue, lease, "workflow-expired-lease")
	expiredLeaseRequest.ExpectedRevision = issue.Revision + 2
	expiredLease := performHubAPIRequest(t, service, http.MethodPost, workflowPath(issue), secret, expiredLeaseRequest)
	requireNativeStatus(t, expiredLease, http.StatusConflict)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), credentialID, clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	revokedCredential := performHubAPIRequest(t, service, http.MethodPost, workflowPath(issue), secret, expiredLeaseRequest)
	requireNativeStatus(t, revokedCredential, http.StatusUnauthorized)
}
