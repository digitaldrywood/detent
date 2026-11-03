package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAttemptEvidencePreflightChecksRunningFencedOwner(t *testing.T) {
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
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "evidence-project", "name": "Example project", "states": nativeFixtureStates()})
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
	issueResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "evidence-issue"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo", Labels: []string{"example-label"}})
	requireNativeStatus(t, issueResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, issueResponse, &issue)
	startedAt := clock.Now().UTC()
	const runnerID = "runner_evidence"
	const machineID = "machine_evidence"
	const credentialID = "credential_evidence"
	const enrollmentID = "enrollment_evidence"
	const secret = "example_runner_evidence_secret"
	tokenHash := apikey.HashToken(secret)
	expires := startedAt.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: startedAt, UpdatedAt: startedAt, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events}, CreatedAt: startedAt, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, machineID, startedAt); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: machineID, Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runnerID, MachineID: machineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events}, DisplayName: "Example runner", CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: startedAt, ExpiresAt: expires, LastHeartbeatAt: startedAt}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), runnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: startedAt})
	}); err != nil {
		t.Fatal(err)
	}

	secondResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "evidence-second-issue"}, Title: "Second example issue", Body: workItemRecordBody, State: "Todo", Labels: []string{"example-label"}})
	requireNativeStatus(t, secondResponse, http.StatusOK)
	var second tracker.NativeIssue
	decodeHubResponse(t, secondResponse, &second)
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
	lease := acquire(issue, "evidence-first-session")
	secondLease := acquire(second, "evidence-second-session")

	payload, err := service.config.RunDetails.StoreDetail(t.Context(), string(organization.ID), []byte(`{"example":"attempt payload"}`))
	if err != nil {
		t.Fatal(err)
	}
	for index, value := range []struct {
		lease   leasestore.Lease
		attempt string
	}{{lease, "attempt_evidence_first"}, {secondLease, "attempt_evidence_second"}} {
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization.ID, ProjectID: project.ID, LeaseID: tracker.LeaseID(value.lease.LeaseID), PolicyID: descriptor.ID, BoundAt: clock.Now()}); err != nil {
				return err
			}
			_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: value.attempt, RunID: value.attempt + "-run", Fence: leasestore.Fence{LeaseID: value.lease.LeaseID, FencingToken: value.lease.FencingToken}, Sequence: 1, Status: "running", PayloadObjectKey: payload.Key})
			return err
		}); err != nil {
			t.Fatalf("attempt %d: %v", index, err)
		}
	}
	service.config.Hosted = &HostedConfig{OrganizationID: string(organization.ID), SharedEntry: &HostedSharedEntry{}}
	service.registerCloudAttachmentRoutes(service.echo)
	originalDB, originalReader := service.database.db, service.database.reader
	t.Cleanup(func() { service.database.db = originalDB; service.database.reader = originalReader })
	service.database.db, service.database.reader = nil, nil
	input := attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: "evidence-preflight", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, WorkItemID: issue.WorkItemID, AttemptID: "attempt_evidence_first", Caption: "Example evidence"}
	path := projectPath + "/" + string(project.ID) + "/attempts/" + input.AttemptID + "/evidence/check"
	request := func(path string, input attachment.EvidenceRequest) int {
		t.Helper()
		response := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"size": 0, "evidence": input})
		if response.Code == http.StatusOK {
			var out struct {
				Principal string `json:"principal_id"`
			}
			if json.Unmarshal(response.Body.Bytes(), &out) != nil || out.Principal != credentialID {
				t.Fatal("preflight principal differs")
			}
		}
		return response.Code
	}
	if status := request(path, input); status != http.StatusOK {
		t.Fatalf("preflight status=%d", status)
	}
	for _, test := range []struct {
		name  string
		input attachment.EvidenceRequest
		path  string
	}{
		{"stale fence", func() attachment.EvidenceRequest { v := input; v.FencingToken++; return v }(), path},
		{"other item", func() attachment.EvidenceRequest { v := input; v.WorkItemID = second.WorkItemID; return v }(), path},
		{"other attempt", func() attachment.EvidenceRequest { v := input; v.AttemptID = "attempt_evidence_second"; return v }(), strings.Replace(path, input.AttemptID, "attempt_evidence_second", 1)},
		{"foreign project", input, strings.Replace(path, string(project.ID), "prj_foreign", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := request(test.path, test.input); status != http.StatusConflict && status != http.StatusNotFound && status != http.StatusForbidden {
				t.Fatalf("refusal status=%d", status)
			}
		})
	}
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	if status := request(path, input); status != http.StatusServiceUnavailable {
		t.Fatalf("missing accounting owner status=%d", status)
	}
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{})
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "released")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status := request(path, input); status != http.StatusConflict {
		t.Fatalf("released lease status=%d", status)
	}
}
