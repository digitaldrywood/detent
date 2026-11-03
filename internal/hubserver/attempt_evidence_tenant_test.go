package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
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
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	var operationLog bytes.Buffer
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, Clock: clock.Now, Logger: slog.New(slog.NewTextHandler(&operationLog, nil)),
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

	clock.Advance(secondLease.RenewedAt.Sub(clock.Now()) + time.Second)
	var firstAttemptID, secondAttemptID string
	for index, value := range []struct {
		lease leasestore.Lease
		item  tracker.NativeWorkItemID
	}{{lease, issue.WorkItemID}, {secondLease, second.WorkItemID}} {
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization.ID, ProjectID: project.ID, LeaseID: tracker.LeaseID(value.lease.LeaseID), PolicyID: descriptor.ID, BoundAt: value.lease.AcquiredAt})
		}); err != nil {
			t.Fatal(err)
		}
		started := nativeStartedEvent(tracker.NativeLease{ID: tracker.LeaseID(value.lease.LeaseID), WorkItemID: value.item, MachineID: machineID, SessionID: value.lease.SessionID, FencingToken: tracker.FencingToken(value.lease.FencingToken), PolicyID: descriptor.ID})
		started.IdempotencyKey = "start-" + value.lease.SessionID
		response := performHubAPIRequest(t, service, http.MethodPost, itemsPath+"/"+string(value.item)+"/events", secret, started)
		requireNativeStatus(t, response, http.StatusOK)
		if index == 0 {
			firstAttemptID = started.Data.AttemptID
		} else {
			secondAttemptID = started.Data.AttemptID
		}
	}
	service.config.Hosted = &HostedConfig{OrganizationID: string(organization.ID), SharedEntry: &HostedSharedEntry{}}
	service.registerCloudAttachmentRoutes(service.echo)
	if service.outbox != nil {
		service.outbox.stop()
	}
	originalDB, originalReader := service.database.db, service.database.reader
	t.Cleanup(func() { service.database.db = originalDB; service.database.reader = originalReader })
	service.database.db, service.database.reader = nil, nil
	input := attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: "evidence-preflight", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, WorkItemID: issue.WorkItemID, AttemptID: firstAttemptID, Caption: "Example evidence"}
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
		{"other attempt", func() attachment.EvidenceRequest { v := input; v.AttemptID = secondAttemptID; return v }(), strings.Replace(path, input.AttemptID, secondAttemptID, 1)},
		{"foreign project", input, strings.Replace(path, string(project.ID), "prj_foreign", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := request(test.path, test.input); status != http.StatusConflict && status != http.StatusNotFound && status != http.StatusForbidden {
				t.Fatalf("refusal status=%d", status)
			}
		})
	}
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes'`, organization.ID); err != nil {
		t.Fatal(err)
	}
	if status := request(path, input); status != http.StatusServiceUnavailable {
		t.Fatalf("missing accounting owner status=%d", status)
	}
	if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_usage_counters (organization_id,name,value) VALUES ($1,'collaboration_bytes',0)", organization.ID); err != nil {
		t.Fatal(err)
	}
	credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization.ID, tokenHash, "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("credential status=%d error=%v", status, err)
	}
	if err := service.checkAttachmentEvidenceTenant(t.Context(), nativeScope{organization: organization.ID, project: project.ID, credential: credential}, input); err != nil {
		t.Fatalf("typed accounting preflight: %v", err)
	}
	planRef := billingstore.PlanReference{ID: "example-attachments", Version: 1}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization.ID, PlanReference: planRef, Name: "Example attachments", Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 1 << 20}}); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization.ID, Base: planRef}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	service.database.hostedOrganization = organization.ID
	service.database.billingRepository = repository
	service.config.BillingRepository = repository
	service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{Base: PlanReference{ID: planRef.ID, Version: planRef.Version}, WindowSeconds: 60, RetentionWindows: 10}})
	positiveCheck := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"size": 100, "evidence": input})
	requireNativeStatus(t, positiveCheck, http.StatusOK)
	quotaCheck := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"size": attachment.MaxBytes, "evidence": input})
	requireNativeStatus(t, quotaCheck, http.StatusTooManyRequests)
	metadataPath := projectPath + "/" + string(project.ID) + "/attachment-metadata"
	var attachmentID string
	for _, name := range []string{"first.png", "second.png"} {
		upload := attachment.UploadRequest{Metadata: attachment.Metadata{Name: name, ContentType: "image/png", Size: 100, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1}, RequestID: "upload-" + name}
		preparedResponse := performHubAPIRequest(t, service, http.MethodPost, metadataPath+"/check", secret, map[string]any{"upload": upload})
		requireNativeStatus(t, preparedResponse, http.StatusOK)
		var prepared attachment.Metadata
		decodeHubResponse(t, preparedResponse, &prepared)
		if prepared.ID == "" || prepared.ProjectID != string(project.ID) || prepared.Uploader != credentialID {
			t.Fatal("prepared attachment scope differs")
		}
		replay := performHubAPIRequest(t, service, http.MethodPost, metadataPath+"/check", secret, map[string]any{"upload": upload})
		requireNativeStatus(t, replay, http.StatusOK)
		if !bytes.Equal(preparedResponse.Body.Bytes(), replay.Body.Bytes()) {
			t.Fatal("prepare replay changed attachment identity")
		}
		attachmentID = prepared.ID
		upload.Metadata = prepared
		recorded := performHubAPIRequest(t, service, http.MethodPost, metadataPath, secret, upload)
		requireNativeStatus(t, recorded, http.StatusCreated)
		replayed := performHubAPIRequest(t, service, http.MethodPost, metadataPath, secret, upload)
		requireNativeStatus(t, replayed, http.StatusCreated)
		if !bytes.Equal(recorded.Body.Bytes(), replayed.Body.Bytes()) {
			t.Fatal("record replay changed attachment metadata")
		}
		read := performHubAPIRequest(t, service, http.MethodGet, metadataPath+"/"+prepared.ID, secret, nil)
		requireNativeStatus(t, read, http.StatusOK)
		foreignRead := performHubAPIRequest(t, service, http.MethodGet, strings.Replace(metadataPath, string(project.ID), "prj_foreign", 1)+"/"+prepared.ID, secret, nil)
		if foreignRead.Code != http.StatusNotFound && foreignRead.Code != http.StatusUnauthorized && foreignRead.Code != http.StatusForbidden {
			t.Fatalf("foreign attachment read status=%d", foreignRead.Code)
		}
		conflict := upload
		conflict.SHA256 = strings.Repeat("c", 64)
		conflicting := performHubAPIRequest(t, service, http.MethodPost, metadataPath, secret, conflict)
		requireNativeStatus(t, conflicting, http.StatusConflict)
		if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.Attachment(t.Context(), project.ID, prepared.ID)
			if err != nil {
				return err
			}
			if row.Metadata.SHA256 != prepared.SHA256 || row.Metadata.Size != prepared.Size {
				t.Fatal("stored attachment content identity differs")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	collision := attachment.UploadRequest{Metadata: attachment.Metadata{ID: attachmentID, Name: "collision.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("d", 64), Width: 1, Height: 1}}
	collisionResponse := performHubAPIRequest(t, service, http.MethodPost, metadataPath, secret, collision)
	if collisionResponse.Code == http.StatusCreated {
		t.Fatal("conflicting attachment ID was accepted")
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(t.Context())
		if err == nil && used != 200 {
			t.Fatalf("replayed uploads charged %d bytes, want 200", used)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	evidenceInput := input
	evidenceInput.IdempotencyKey = "publish-attempt-evidence"
	evidenceUpload := attachment.UploadRequest{Metadata: attachment.Metadata{Name: "evidence.png", ContentType: "image/png", Size: 20, SHA256: strings.Repeat("b", 64), Width: 1, Height: 1}, RequestID: evidenceInput.IdempotencyKey, Evidence: &evidenceInput}
	preparedEvidence := performHubAPIRequest(t, service, http.MethodPost, path, secret, map[string]any{"upload": evidenceUpload})
	requireNativeStatus(t, preparedEvidence, http.StatusOK)
	decodeHubResponse(t, preparedEvidence, &evidenceUpload.Metadata)
	evidencePath := strings.TrimSuffix(path, "/check")
	for _, requestID := range []string{"", "another-evidence-mutation"} {
		mismatched := evidenceUpload
		mismatched.RequestID = requestID
		refused := performHubAPIRequest(t, service, http.MethodPost, evidencePath, secret, mismatched)
		requireNativeStatus(t, refused, http.StatusUnprocessableEntity)
	}
	publishedEvidence := performHubAPIRequest(t, service, http.MethodPost, evidencePath, secret, evidenceUpload)
	if publishedEvidence.Code != http.StatusCreated {
		t.Log(operationLog.String())
	}
	requireNativeStatus(t, publishedEvidence, http.StatusCreated)
	var evidenceMetadata attachment.Metadata
	decodeHubResponse(t, publishedEvidence, &evidenceMetadata)
	if evidenceMetadata.WorkItemID != string(issue.WorkItemID) || evidenceMetadata.CommentID == "" {
		t.Fatal("attempt evidence publication lacks its comment")
	}
	var evidenceUsage int64
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.Attachment(t.Context(), project.ID, evidenceMetadata.ID)
		if err != nil {
			return err
		}
		if record.EvidenceAttemptID != input.AttemptID {
			t.Fatal("evidence producer differs")
		}
		if _, err := r.Comment(t.Context(), project.ID, issue.WorkItemID, evidenceMetadata.CommentID); err != nil {
			return err
		}
		evidenceUsage, err = r.CollaborationBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	readEvidenceAttempt := performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(issue.WorkItemID)+"/attempts/"+input.AttemptID, secret, nil)
	requireNativeStatus(t, readEvidenceAttempt, http.StatusOK)
	var evidenceAttempt tracker.NativeAttempt
	decodeHubResponse(t, readEvidenceAttempt, &evidenceAttempt)
	if len(evidenceAttempt.Evidence) != 1 || evidenceAttempt.Evidence[0].AttachmentID != evidenceMetadata.ID || !strings.Contains(evidenceAttempt.Evidence[0].Reference, evidenceMetadata.ID) {
		t.Fatal("attempt read omitted published evidence")
	}
	readRuntime := performHubAPIRequest(t, service, http.MethodGet, itemsPath+"/"+string(issue.WorkItemID)+"/runtime?native_attempt_id="+input.AttemptID, secret, nil)
	requireNativeStatus(t, readRuntime, http.StatusOK)
	var runtime tracker.NativeRuntimeEvidence
	decodeHubResponse(t, readRuntime, &runtime)
	if runtime.Attempt == nil || len(runtime.Attempt.Evidence) != 1 || runtime.Attempt.Evidence[0].AttachmentID != evidenceMetadata.ID {
		t.Fatal("runtime evidence omitted the published attachment")
	}
	evidenceReplay := performHubAPIRequest(t, service, http.MethodPost, evidencePath, secret, evidenceUpload)
	requireNativeStatus(t, evidenceReplay, http.StatusCreated)
	if !bytes.Equal(evidenceReplay.Body.Bytes(), publishedEvidence.Body.Bytes()) {
		t.Fatal("attempt evidence replay changed publication")
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(t.Context())
		if err == nil && used != evidenceUsage {
			t.Fatal("attempt evidence replay charged twice")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	commentInput := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "publish-screenshot", LeaseID: input.LeaseID, FencingToken: input.FencingToken}, Body: "Example screenshot ![image](attachment:" + attachmentID + ")"}
	commentPath := itemsPath + "/" + string(issue.WorkItemID) + "/comments"
	commentResponse := performHubAPIRequest(t, service, http.MethodPost, commentPath, secret, commentInput)
	requireNativeStatus(t, commentResponse, http.StatusOK)
	var published tracker.NativeComment
	decodeHubResponse(t, commentResponse, &published)
	commentReplay := performHubAPIRequest(t, service, http.MethodPost, commentPath, secret, commentInput)
	requireNativeStatus(t, commentReplay, http.StatusOK)
	if !bytes.Equal(commentResponse.Body.Bytes(), commentReplay.Body.Bytes()) {
		t.Fatal("comment replay changed publication")
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		references, err := r.AttachmentReferences(t.Context(), project.ID, attachmentID, 201)
		if err != nil {
			return err
		}
		if len(references) != 1 || references[0].WorkItemID != issue.WorkItemID || references[0].CommentID != published.ID {
			t.Fatal("screenshot reference was not published atomically")
		}
		version, err := r.CollaborationVersion(t.Context(), published.ID, 1)
		if err != nil {
			return err
		}
		if version.WorkItemID != issue.WorkItemID {
			t.Fatal("comment version has wrong owner")
		}
		used, err := r.CollaborationBytes(t.Context())
		if err == nil && used <= 200 {
			t.Fatal("comment publication did not charge persisted bytes")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "released")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status := request(path, input); status != http.StatusConflict {
		t.Fatalf("released lease status=%d", status)
	}
	commentInput.IdempotencyKey = "released-screenshot"
	releasedComment := performHubAPIRequest(t, service, http.MethodPost, commentPath, secret, commentInput)
	requireNativeStatus(t, releasedComment, http.StatusConflict)
}
