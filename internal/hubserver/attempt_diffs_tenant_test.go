package hubserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptDiffExpiredManifestDropsOnlyArchivedBodies(t *testing.T) {
	files := []tracker.AttemptDiffFile{
		{Path: "example.go", Status: tracker.DiffStatusModified, Patch: "+inline", PatchBody: &diffbody.Reference{Key: "example/archive", SHA256: strings.Repeat("a", 64), Bytes: 7}},
		{Path: "expired.go", Status: tracker.DiffStatusModified, PatchExpired: true},
	}
	got := expireAttemptDiffBodies(files, true)
	if got[0].Patch != "+inline" || got[0].PatchBody != nil || !got[0].PatchExpired || !got[1].PatchExpired {
		t.Fatalf("expired manifest files=%+v", got)
	}
}

func TestAttemptDiffPostgresHTTPStoresAndReadsTwoFencedAttempts(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES('org_startup','collaboration_bytes',0)`); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.db != nil || service.database.reader != nil {
		t.Fatal("PostgreSQL Hub created a legacy SQLite connection")
	}

	const organization tracker.OrganizationID = "org_startup"
	const project tracker.ProjectID = "project_example"
	const workerToken = "example_runner_token"
	const tokenID = "credential_runner"
	const runnerID = "runner_example"
	const machineID tracker.MachineID = "machine_example"
	now := service.config.now().UTC()
	expires := now.Add(time.Hour)
	workerHash := apikey.HashToken(workerToken)
	type attemptFixture struct {
		item    tracker.NativeWorkItemID
		attempt leasestore.Attempt
		lease   leasestore.Lease
	}
	fixtures := make([]attemptFixture, 0, 2)
	var emptyItem tracker.NativeWorkItemID
	repository := service.config.TenantRepository
	var initialArchiveBytes int64
	var initialCollaborationBytes int64
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: tokenID, Name: "Example runner", Hash: workerHash, Fingerprint: tokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: tokenID, ProjectID: project}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_runner", RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", runnerID, machineID, now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machineID), Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: machineID, TokenID: tokenID, EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Revision: 1, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		for index := range 2 {
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID("wi_example_" + strconv.Itoa(index)), Number: int64(index + 1), Revision: 1, Title: "Example issue " + strconv.Itoa(index), ActorKind: new("human"), ActorPrincipalID: new("test_hub_admin"), WorkflowStateName: new("Todo"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
			if err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(machineID), SessionID: "session_example_" + strconv.Itoa(index), LeaseID: "lease_example_" + strconv.Itoa(index), TTL: time.Hour})
			if err != nil {
				return err
			}
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
				return err
			}
			attempt, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: newNativeID("attempt"), Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: newNativeID("run"), Sequence: 1, Status: "running", StartedAt: now, DispatchGeneration: 1, PayloadObjectKey: "example/start"})
			if err != nil {
				return err
			}
			fixtures = append(fixtures, attemptFixture{item: item.NativeID, attempt: attempt, lease: lease})
		}
		item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: "wi_empty", Number: 3, Revision: 1, Title: "Example empty issue", ActorKind: new("human"), ActorPrincipalID: new("test_hub_admin"), WorkflowStateName: new("Todo"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
		if err != nil {
			return err
		}
		emptyItem = item.NativeID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	for _, item := range append([]tracker.NativeWorkItemID{fixtures[0].item, fixtures[1].item}, emptyItem) {
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		initialArchiveBytes, err = r.ArchiveBytes(t.Context())
		if err != nil {
			return err
		}
		initialCollaborationBytes, err = r.CollaborationBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for index, fixture := range fixtures {
		request := tracker.AttemptDiffRequest{
			Producer:   tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: fixture.attempt.ID, RunnerID: runnerID, LeaseID: tracker.LeaseID(fixture.lease.LeaseID), FencingToken: tracker.FencingToken(fixture.lease.FencingToken)},
			Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1},
			BaseSHA:    strings.Repeat("a", 40),
			HeadSHA:    strings.Repeat("b", 40),
			Files:      []tracker.AttemptDiffFile{{Path: "example.go", Status: tracker.DiffStatusModified, Additions: index + 1, Patch: "+example"}},
		}
		path := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/attempts/"+fixture.attempt.ID+"/diff", ":organization", string(organization)), ":project", string(project))
		response := performHubAPIRequest(t, service, http.MethodPost, path, workerToken, request)
		requireNativeStatus(t, response, http.StatusAccepted)
		var receipt tracker.AttemptDiffReceipt
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || !receipt.Accepted || receipt.FileCount != 1 {
			t.Fatalf("attempt diff receipt=%+v error=%v body=%s", receipt, err, response.Body.String())
		}
		var archiveBytesAfterWrite int64
		var collaborationBytesAfterWrite int64
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			archiveBytesAfterWrite, err = r.ArchiveBytes(t.Context())
			if err != nil {
				return err
			}
			collaborationBytesAfterWrite, err = r.CollaborationBytes(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if archiveBytesAfterWrite <= initialArchiveBytes {
			t.Fatalf("diff archive bytes did not increase: before=%d after=%d", initialArchiveBytes, archiveBytesAfterWrite)
		}
		if collaborationBytesAfterWrite <= initialCollaborationBytes {
			t.Fatalf("diff collaboration bytes did not increase: before=%d after=%d", initialCollaborationBytes, collaborationBytesAfterWrite)
		}
		if replay := performHubAPIRequest(t, service, http.MethodPost, path, workerToken, request); replay.Code != http.StatusConflict {
			t.Fatalf("same-generation replay status=%d body=%s", replay.Code, replay.Body.String())
		}
		var archiveBytesAfterReplay int64
		var collaborationBytesAfterReplay int64
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			archiveBytesAfterReplay, err = r.ArchiveBytes(t.Context())
			if err != nil {
				return err
			}
			collaborationBytesAfterReplay, err = r.CollaborationBytes(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if archiveBytesAfterReplay != archiveBytesAfterWrite {
			t.Fatalf("replay changed diff archive bytes: before=%d after=%d", archiveBytesAfterWrite, archiveBytesAfterReplay)
		}
		if collaborationBytesAfterReplay != collaborationBytesAfterWrite {
			t.Fatalf("replay changed diff collaboration bytes: before=%d after=%d", collaborationBytesAfterWrite, collaborationBytesAfterReplay)
		}
		initialArchiveBytes = archiveBytesAfterWrite
		initialCollaborationBytes = collaborationBytesAfterWrite
		read := performHubAPIRequest(t, service, http.MethodGet, path, workerToken, nil)
		requireNativeStatus(t, read, http.StatusOK)
		var diff tracker.AttemptDiff
		if err := json.Unmarshal(read.Body.Bytes(), &diff); err != nil || diff.ID != receipt.DiffID || diff.Files[0].Additions != index+1 || diff.Files[0].Patch != "+example" || diff.HeadSHA != strings.Repeat("b", 40) {
			t.Fatalf("attempt diff read=%+v error=%v body=%s", diff, err, read.Body.String())
		}
		itemPath := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/work-items/"+string(fixture.item)+"/diff", ":organization", string(organization)), ":project", string(project))
		itemRead := performHubAPIRequest(t, service, http.MethodGet, itemPath, workerToken, nil)
		requireNativeStatus(t, itemRead, http.StatusOK)
		var itemDiff tracker.WorkItemDiff
		if err := json.Unmarshal(itemRead.Body.Bytes(), &itemDiff); err != nil || itemDiff.Diff == nil || itemDiff.Diff.ID != receipt.DiffID {
			t.Fatalf("work item diff=%+v error=%v body=%s", itemDiff, err, itemRead.Body.String())
		}
	}
	first := fixtures[0]
	secondGeneration := tracker.AttemptDiffRequest{
		Producer:   tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: first.attempt.ID, RunnerID: runnerID, LeaseID: tracker.LeaseID(first.lease.LeaseID), FencingToken: tracker.FencingToken(first.lease.FencingToken)},
		Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2},
		BaseSHA:    strings.Repeat("b", 40),
		HeadSHA:    strings.Repeat("c", 40),
		Files:      []tracker.AttemptDiffFile{{Path: "next.go", Status: tracker.DiffStatusModified, Additions: 9, Patch: "+next"}},
	}
	firstPath := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/attempts/"+first.attempt.ID+"/diff", ":organization", string(organization)), ":project", string(project))
	secondResponse := performHubAPIRequest(t, service, http.MethodPost, firstPath, workerToken, secondGeneration)
	requireNativeStatus(t, secondResponse, http.StatusAccepted)
	var secondReceipt tracker.AttemptDiffReceipt
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &secondReceipt); err != nil || !secondReceipt.Accepted {
		t.Fatalf("second generation receipt=%+v error=%v body=%s", secondReceipt, err, secondResponse.Body.String())
	}
	latestRead := performHubAPIRequest(t, service, http.MethodGet, firstPath, workerToken, nil)
	requireNativeStatus(t, latestRead, http.StatusOK)
	var latestDiff tracker.AttemptDiff
	if err := json.Unmarshal(latestRead.Body.Bytes(), &latestDiff); err != nil || latestDiff.ID != secondReceipt.DiffID || latestDiff.Generation.Seq != 2 {
		t.Fatalf("latest diff=%+v error=%v body=%s", latestDiff, err, latestRead.Body.String())
	}
	firstRead := performHubAPIRequest(t, service, http.MethodGet, firstPath+"?at=1", workerToken, nil)
	requireNativeStatus(t, firstRead, http.StatusOK)
	var firstDiff tracker.AttemptDiff
	if err := json.Unmarshal(firstRead.Body.Bytes(), &firstDiff); err != nil || firstDiff.ID == latestDiff.ID || firstDiff.Generation.Seq != 1 {
		t.Fatalf("explicit generation diff=%+v error=%v body=%s", firstDiff, err, firstRead.Body.String())
	}
	emptyItemPath := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/work-items/"+string(emptyItem)+"/diff", ":organization", string(organization)), ":project", string(project))
	emptyRead := performHubAPIRequest(t, service, http.MethodGet, emptyItemPath, workerToken, nil)
	requireNativeStatus(t, emptyRead, http.StatusOK)
	var emptyDiff tracker.WorkItemDiff
	if err := json.Unmarshal(emptyRead.Body.Bytes(), &emptyDiff); err != nil || emptyDiff.Diff != nil {
		t.Fatalf("empty work item diff=%+v error=%v body=%s", emptyDiff, err, emptyRead.Body.String())
	}
	missingItemPath := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/work-items/wi_missing/diff", ":organization", string(organization)), ":project", string(project))
	if response := performHubAPIRequest(t, service, http.MethodGet, missingItemPath, workerToken, nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing work item diff status=%d body=%s", response.Code, response.Body.String())
	}
	wrongPath := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/attempts/"+fixtures[1].attempt.ID+"/diff", ":organization", string(organization)), ":project", string(project))
	staleFence := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: fixtures[1].attempt.ID, RunnerID: runnerID, LeaseID: tracker.LeaseID(fixtures[1].lease.LeaseID), FencingToken: tracker.FencingToken(fixtures[1].lease.FencingToken + 1)}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2}}
	if response := performHubAPIRequest(t, service, http.MethodPost, wrongPath, workerToken, staleFence); response.Code != http.StatusConflict {
		t.Fatalf("stale fencing token status=%d body=%s", response.Code, response.Body.String())
	}
	wrongProducer := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: fixtures[1].attempt.ID, RunnerID: "runner_other", LeaseID: tracker.LeaseID(fixtures[1].lease.LeaseID), FencingToken: tracker.FencingToken(fixtures[1].lease.FencingToken)}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2}}
	if response := performHubAPIRequest(t, service, http.MethodPost, wrongPath, workerToken, wrongProducer); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("producer identity mismatch status=%d body=%s", response.Code, response.Body.String())
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), tokenID, service.config.now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if response := performHubAPIRequest(t, service, http.MethodGet, firstPath, workerToken, nil); response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
		t.Fatalf("revoked runner diff read status=%d body=%s", response.Code, response.Body.String())
	}
}
