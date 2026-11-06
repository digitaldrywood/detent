package hubserver

import (
	"encoding/json"
	"errors"
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
			Files: []tracker.AttemptDiffFile{
				{Path: "example.go", Status: tracker.DiffStatusModified, Additions: index + 1, Patch: "+example"},
				{Path: ".env", Status: tracker.DiffStatusAdded, Patch: "+EXAMPLE_SECRET=private"},
				{Path: "large.go", Status: tracker.DiffStatusModified, Additions: 5, Patch: strings.Repeat("x", tracker.MaxDiffPatchBytes+512)},
			},
		}
		path := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/attempts/"+fixture.attempt.ID+"/diff", ":organization", string(organization)), ":project", string(project))
		response := performHubAPIRequest(t, service, http.MethodPost, path, workerToken, request)
		requireNativeStatus(t, response, http.StatusAccepted)
		var receipt tracker.AttemptDiffReceipt
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || !receipt.Accepted || receipt.FileCount != 3 {
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
		if len(diff.Files) != 3 || !diff.Files[1].Denied || diff.Files[1].Patch != "" {
			t.Fatalf("stored diff exposed a blocked patch: %+v", diff.Files)
		}
		if !diff.Truncated || !diff.Files[2].Truncated || len(diff.Files[2].Patch) != tracker.MaxDiffPatchBytes || diff.Files[2].Additions != 5 {
			t.Fatal("stored diff lost patch truncation or counts")
		}
		func() {
			legacy := service.database
			service.database = nil
			defer func() { service.database = legacy }()
			direct, err := service.readAttemptDiff(t.Context(), nativeScope{organization: organization, project: project}, string(fixture.item), fixture.attempt.ID, tracker.DiffSourceAttempt, 1)
			if err != nil || direct.ID != receipt.DiffID || len(direct.Files) != 3 || direct.Files[0].Patch != "+example" {
				t.Fatalf("diff without legacy database=%+v error=%v", direct, err)
			}
		}()
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

	for _, test := range []struct {
		name   string
		mutate func(*tracker.AttemptDiffRequest)
		status int
	}{
		{"earlier generation", func(r *tracker.AttemptDiffRequest) { r.Generation.Seq = 1 }, http.StatusConflict},
		{"wrong attempt", func(r *tracker.AttemptDiffRequest) { r.Producer.ID = fixtures[1].attempt.ID }, http.StatusConflict},
		{"missing lease", func(r *tracker.AttemptDiffRequest) { r.Producer.LeaseID = "" }, http.StatusUnprocessableEntity},
		{"zero generation", func(r *tracker.AttemptDiffRequest) { r.Generation.Seq = 0 }, http.StatusUnprocessableEntity},
		{"source mismatch", func(r *tracker.AttemptDiffRequest) {
			r.Generation.Source = tracker.DiffSourceWorkspace
			r.Generation.ID = "ws_example"
		}, http.StatusUnprocessableEntity},
		{"unsupported workspace", func(r *tracker.AttemptDiffRequest) {
			r.Producer.Kind = tracker.DiffSourceWorkspace
			r.Generation.Source = tracker.DiffSourceWorkspace
			r.Generation.ID = "ws_" + strings.Repeat("0", 32)
		}, http.StatusConflict},
		{"unknown status", func(r *tracker.AttemptDiffRequest) { r.Files[0].Status = "copied" }, http.StatusUnprocessableEntity},
		{"oversized SHA", func(r *tracker.AttemptDiffRequest) { r.HeadSHA = strings.Repeat("c", tracker.MaxDiffSHABytes+1) }, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := secondGeneration
			request.Generation.Seq = 3
			request.Files = append([]tracker.AttemptDiffFile(nil), secondGeneration.Files...)
			test.mutate(&request)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, firstPath, workerToken, request), test.status)
		})
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_other", Name: "Other project", Slug: "other-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: tokenID, ProjectID: "project_other"})
	}); err != nil {
		t.Fatal(err)
	}
	_, scopeErr := service.readAttemptDiff(t.Context(), nativeScope{organization: organization, project: "project_other"}, "", first.attempt.ID, tracker.DiffSourceAttempt, 1)
	if !errors.Is(scopeErr, corestore.ErrNotFound) {
		t.Fatalf("foreign project diff read error=%v", scopeErr)
	}
	cleanupContext := t.Context()
	for _, test := range []struct {
		name   string
		query  string
		args   []any
		status int
	}{
		{"missing project grant", "DELETE FROM token_grants WHERE organization_id=$1 AND token_id=$2 AND project_id=$3", []any{organization, tokenID, project}, http.StatusForbidden},
		{"missing claim", "UPDATE runner_identities SET operations=ARRAY['read','events']::text[] WHERE organization_id=$1 AND runner_id=$2", []any{organization, runnerID}, http.StatusForbidden},
		{"released lease", "UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id=$3", []any{service.config.now().UTC(), organization, first.lease.LeaseID}, http.StatusConflict},
		{"expired lease", "UPDATE leases SET acquired_at=$1,renewed_at=$1,expires_at=$2 WHERE organization_id=$3 AND lease_id=$4", []any{service.config.now().UTC().Add(-2 * time.Minute), service.config.now().UTC().Add(-time.Minute), organization, first.lease.LeaseID}, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(func() {
				if _, err := admin.ExecContext(cleanupContext, "INSERT INTO token_grants(organization_id,token_id,project_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", organization, tokenID, project); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(cleanupContext, "UPDATE runner_identities SET operations=ARRAY['read','claim','events']::text[] WHERE organization_id=$1 AND runner_id=$2", organization, runnerID); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(cleanupContext, "UPDATE leases SET acquired_at=$1,renewed_at=$2,expires_at=$3,released_at=NULL WHERE organization_id=$4 AND lease_id=$5", first.lease.AcquiredAt, first.lease.RenewedAt, first.lease.ExpiresAt, organization, first.lease.LeaseID); err != nil {
					t.Error(err)
				}
			})
			if _, err := admin.ExecContext(t.Context(), test.query, test.args...); err != nil {
				t.Fatal(err)
			}
			if test.name == "missing project grant" {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, firstPath, workerToken, nil), http.StatusForbidden)
			}
			request := secondGeneration
			request.Generation.Seq = 3
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, firstPath, workerToken, request), test.status)
		})
	}
	oversized := secondGeneration
	oversized.Generation.Seq = 3
	oversized.Files = make([]tracker.AttemptDiffFile, 21)
	for index := range oversized.Files {
		oversized.Files[index] = tracker.AttemptDiffFile{Path: "large" + strconv.Itoa(index) + ".go", Status: tracker.DiffStatusModified, Additions: 1, Patch: strings.Repeat("y", tracker.MaxDiffPatchBytes)}
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, firstPath, workerToken, oversized), http.StatusRequestEntityTooLarge)
	oversized.Files = tracker.StripDiffPatches(oversized.Files)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, firstPath, workerToken, oversized), http.StatusAccepted)
	var stripped tracker.AttemptDiff
	decodeHubResponse(t, performHubAPIRequest(t, service, http.MethodGet, firstPath, workerToken, nil), &stripped)
	if len(stripped.Files) != 21 || stripped.Files[0].Patch != "" || stripped.Files[0].Additions != 1 {
		t.Fatal("stored stripped diff lost file metadata")
	}

	for index, fixture := range fixtures {
		body := diffbody.New(string(organization), string(project), "diff_body_"+strconv.Itoa(index), 0, "+object")
		request := tracker.AttemptDiffRequest{
			Producer:   tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: fixture.attempt.ID, RunnerID: runnerID, LeaseID: tracker.LeaseID(fixture.lease.LeaseID), FencingToken: tracker.FencingToken(fixture.lease.FencingToken)},
			Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 4},
			BaseSHA:    strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
			Files: []tracker.AttemptDiffFile{{Path: "object.go", Status: tracker.DiffStatusModified, PatchBody: &body}},
		}
		path := strings.ReplaceAll(strings.ReplaceAll(nativeBase+"/attempts/"+fixture.attempt.ID+"/diff", ":organization", string(organization)), ":project", string(project))
		response := performHubAPIRequest(t, service, http.MethodPost, path, workerToken, request)
		requireNativeStatus(t, response, http.StatusAccepted)
		for _, check := range []struct {
			key  string
			want bool
		}{{body.Key, true}, {diffbody.New(string(organization), string(project), "diff_missing", 0, "missing").Key, false}, {diffbody.New(string(organization), "project_other", "diff_body_"+strconv.Itoa(index), 0, "+object").Key, false}} {
			func() {
				defer func() {
					if value := recover(); value != nil {
						t.Fatalf("body lookup used legacy storage: %v", value)
					}
				}()
				exists, err := service.manifestDiffBodyExists(t.Context(), check.key)
				if err != nil || exists != check.want {
					t.Fatalf("body referenced=%v want=%v error=%v", exists, check.want, err)
				}
			}()
		}
	}
	if _, err := admin.ExecContext(t.Context(), "UPDATE attempt_diffs SET body_references_indexed=false WHERE organization_id=$1 AND project_id=$2", organization, project); err != nil {
		t.Fatal(err)
	}
	if _, err := service.manifestDiffBodyExists(t.Context(), diffbody.New(string(organization), string(project), "diff_unknown", 0, "unknown").Key); err == nil {
		t.Fatal("unindexed historic manifests authorized orphan deletion")
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

func TestAttemptDiffReadsRequireTypedOwner(t *testing.T) {
	for _, operation := range []string{"attempt", "work-item"} {
		t.Run(operation, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("read used legacy storage: %v", value)
				}
			}()
			service := &Service{}
			var err error
			if operation == "attempt" {
				_, err = service.readAttemptDiff(t.Context(), nativeScope{}, "item", "attempt", "attempt", 1)
			} else {
				_, err = service.readWorkItemDiff(t.Context(), nativeScope{}, "item", "attempt")
			}
			var unavailable *nativeError
			if !errors.As(err, &unavailable) || unavailable.status != http.StatusServiceUnavailable {
				t.Fatalf("missing owner error = %v", err)
			}
		})
	}
}
