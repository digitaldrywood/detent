package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeRunEventParticipantFixture struct {
	reject error
	apply  int
	lease  int
}

func withClosedSQLite(t *testing.T, service *Service, organization tracker.OrganizationID, call func() error) error {
	t.Helper()
	reader := service.database.reader
	startSpritePool := service.startSpritePoolForQueue
	key := spriteWakeKey{organization: organization}
	service.database.reader = nil
	service.startSpritePoolForQueue = func(nativeScope) {}
	service.spriteWakeMu.Lock()
	service.spriteWakes[key] = &spriteLifecyclePass{}
	service.spriteWakeMu.Unlock()
	defer func() {
		service.database.reader = reader
		service.startSpritePoolForQueue = startSpritePool
		service.spriteWakeMu.Lock()
		delete(service.spriteWakes, key)
		service.spriteWakeMu.Unlock()
	}()
	return call()
}

func (p *nativeRunEventParticipantFixture) ValidateNativeRunEvent(_ context.Context, _ tenantstore.Reader, authority *NativeRunEventAuthority) error {
	authority.DispatchGeneration = 7
	return nil
}

func (p *nativeRunEventParticipantFixture) ApplyNativeRunEvent(context.Context, tenantstore.Writer, NativeRunEventAuthority) error {
	p.apply++
	return p.reject
}

func (*nativeRunEventParticipantFixture) ValidateNativeLeaseMutation(_ context.Context, _ tenantstore.Reader, authority *NativeLeaseAuthority) error {
	identity := authority.Scope.credential.Runner
	if identity.RunnerID == "" || identity.OrganizationID != authority.Lease.OrganizationID || string(identity.MachineID) != authority.Lease.MachineID {
		return tracker.ErrStaleFencingToken
	}
	return nil
}

func (p *nativeRunEventParticipantFixture) ApplyNativeLeaseMutation(context.Context, tenantstore.Writer, NativeLeaseAuthority) error {
	p.lease++
	return p.reject
}

func (*nativeRunEventParticipantFixture) NativeLeaseResponse(ctx context.Context, reader tenantstore.Reader, scope nativeScope, lease leasestore.Lease) (NativeLeaseResponseDetails, error) {
	item, err := reader.WorkItemByID(ctx, scope.project, lease.IssueID)
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	return NativeLeaseResponseDetails{WorkItemID: item.NativeID}, nil
}

func TestNativeRunEventTenantMutationStoresAttemptEventAndReceiptAtomically(t *testing.T) {
	repository := projectRecordPostgres(t)
	participant := &nativeRunEventParticipantFixture{}
	keys := secretTestKeys(t, "1", "1")
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{},
		NativeRunEvents: participant, SecretKeys: keys,
	})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.now().UTC()
	cursorKey := bytes.Repeat([]byte{1}, 32)
	cursorEnvelope, err := keys.Seal(cursorKey, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: cursorEnvelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursorEnvelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{
		"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates(), "grant_access": true,
	})
	requireNativeStatus(t, projectResponse, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, projectResponse, &project)
	itemPath := projectPath + "/" + string(project.ID) + "/work-items"
	itemResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "item"}, Title: "Example issue", Body: workItemRecordBody,
	})
	requireNativeStatus(t, itemResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, itemResponse, &issue)
	binding := runnerauth.NewBinding()
	credentialID, enrollmentID := "credential_"+binding.RunnerID, "enrollment_"+binding.RunnerID
	expires := now.Add(4 * time.Hour)
	operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}
	var lease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: apikey.HashToken("example_" + binding.RunnerID), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken("enrollment_" + binding.RunnerID), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, binding.RunnerID, binding.MachineID, now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: string(binding.MachineID), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "session", LeaseID: "lease", TTL: time.Hour})
		if err != nil {
			return err
		}
		return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization.ID, LeaseID: lease.LeaseID, RunnerID: binding.RunnerID, AssignedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
	relatedEvents := []tracker.CollaborationEvent{{Data: tracker.CollaborationData{RelatedWorkItemID: issue.WorkItemID}}}
	if err := service.hydrateTenantHistoryRelatedIssues(t.Context(), scope, relatedEvents); err != nil {
		t.Fatal(err)
	}
	if relatedEvents[0].Data.RelatedIssue == nil || relatedEvents[0].Data.RelatedIssue.ID != issue.WorkItemID || relatedEvents[0].Data.RelatedIssue.ProjectID != project.ID {
		t.Fatalf("typed related issue hydration=%+v", relatedEvents[0].Data)
	}
	start := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: tracker.LeaseID(lease.LeaseID),
			FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: "policy_" + strings.Repeat("a", 64), Sequence: 1,
			Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "example-model"}},
	}
	for _, field := range []string{"machine", "session", "runner"} {
		invalid := start
		invalid.IdempotencyKey = "wrong-" + field
		switch field {
		case "machine":
			invalid.Data.MachineID = "other-machine"
		case "session":
			invalid.Data.SessionID = "other-session"
		case "runner":
			invalid.Data.RunnerID = "other-runner"
		}
		if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), invalid); err == nil || !strings.Contains(err.Error(), "Execution identity must match") {
			t.Fatalf("mismatched %s identity error=%v", field, err)
		}
	}
	result, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), start)
	if err != nil || string(result) != `{"accepted":true}` {
		t.Fatalf("start result=%s error=%v", result, err)
	}
	replay, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), start)
	if err != nil || string(replay) != string(result) {
		t.Fatalf("start replay=%s error=%v", replay, err)
	}
	duplicate := start
	duplicate.IdempotencyKey = "start-again"
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), duplicate); err != nil {
		t.Fatal(err)
	}
	if participant.apply != 1 {
		t.Fatalf("duplicate event reapplied participant %d times", participant.apply)
	}
	finish := start
	finish.IdempotencyKey = "finish"
	finish.Type = "run.finished"
	finish.Data.Sequence = 2
	finish.Data.Outcome = "succeeded"
	participant.reject = errors.New("injected participant refusal")
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), finish); !errors.Is(err, participant.reject) {
		t.Fatalf("participant refusal=%v", err)
	}
	participant.reject = nil
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), string(start.Data.AttemptID))
		if err != nil {
			return err
		}
		if attempt.Sequence != 1 || attempt.Status != "running" || attempt.DispatchGeneration != 7 {
			t.Errorf("attempt after rejected completion=%+v", attempt)
		}
		events, err := r.Events(t.Context(), project.ID, issue.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		runEvents := 0
		for _, event := range events {
			if strings.HasPrefix(event.Type, "run.") {
				runEvents++
				if event.AttemptID != string(start.Data.AttemptID) || event.RunID != string(start.Data.RunID) {
					t.Errorf("unexpected run history event=%+v", event)
				}
			}
		}
		if runEvents != 1 {
			t.Errorf("history after rejected completion=%+v", events)
		}
		if _, err := r.Receipt(t.Context(), "test_hub_admin", "POST /api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/work-items/"+string(issue.WorkItemID)+"/events", "finish"); !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("rejected completion receipt=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := withClosedSQLite(t, service, organization.ID, func() error {
		_, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), finish)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stale := finish
	stale.IdempotencyKey = "stale"
	stale.Data.Sequence = 4
	var conflict *nativeError
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), stale); !errors.As(err, &conflict) || conflict.Code != "run_sequence_conflict" {
		t.Fatalf("stale event sequence error=%v", err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), string(start.Data.AttemptID))
		if err != nil {
			return err
		}
		if attempt.Sequence != 2 || attempt.Status != "succeeded" {
			t.Errorf("attempt after accepted completion=%+v", attempt)
		}
		events, err := r.Events(t.Context(), project.ID, issue.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		runEvents := 0
		for _, event := range events {
			if strings.HasPrefix(event.Type, "run.") {
				runEvents++
			}
		}
		if runEvents != 2 {
			t.Errorf("history events=%+v", events)
		}
		if _, err := r.Receipt(t.Context(), "test_hub_admin", "POST /api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/work-items/"+string(issue.WorkItemID)+"/events", "stale"); !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("stale event receipt=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	secondItemResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "second-item"}, Title: "Second example issue", Body: workItemRecordBody,
	})
	requireNativeStatus(t, secondItemResponse, http.StatusOK)
	var secondIssue tracker.NativeIssue
	decodeHubResponse(t, secondItemResponse, &secondIssue)
	var secondLease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), project.ID, secondIssue.WorkItemID)
		if err != nil {
			return err
		}
		secondLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "second-session", LeaseID: "second-lease", TTL: time.Hour})
		if err != nil {
			return err
		}
		return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization.ID, LeaseID: secondLease.LeaseID, RunnerID: binding.RunnerID, AssignedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	secondStart := start
	secondStart.IdempotencyKey = "second-start"
	secondStart.Data.AttemptID = newNativeID("attempt")
	secondStart.Data.RunID = newNativeID("run")
	secondStart.Data.LeaseID = tracker.LeaseID(secondLease.LeaseID)
	secondStart.Data.FencingToken = tracker.FencingToken(secondLease.FencingToken)
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(secondIssue.WorkItemID), secondStart); err != nil {
		t.Fatal(err)
	}
	observation := secondStart
	observation.IdempotencyKey = "second-observation"
	observation.Type = "run.observed"
	observation.Data.Sequence = 2
	observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: service.config.now().UTC()}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(secondIssue.WorkItemID), observation); err != nil {
		t.Fatal(err)
	}
	laterObservation := observation
	laterObservation.IdempotencyKey = "later-observation"
	laterObservation.Data.Sequence = 3
	laterObservation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "validation", HeartbeatAt: service.config.now().UTC().Add(time.Second)}
	laterObservation.Type = "run.checkpointed"
	pendingCheckpoint := *nativeTestCheckpoint()
	pendingCheckpoint.ExternalEffect, pendingCheckpoint.EffectState, pendingCheckpoint.EffectID = "git_push", "pending", newNativeID("effect")
	pendingCheckpoint.WorktreeState, pendingCheckpoint.HeadSHA = "unpushed", strings.Repeat("a", 40)
	laterObservation.Data.Handoff = &pendingCheckpoint
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(secondIssue.WorkItemID), laterObservation); err != nil {
		t.Fatal(err)
	}
	var retainedCheckpointKey string
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), secondStart.Data.AttemptID)
		if err == nil && attempt.CheckpointObjectKey == nil {
			t.Fatal("pending publication checkpoint was not retained")
		}
		if err == nil {
			retainedCheckpointKey = *attempt.CheckpointObjectKey
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	batchFinish := secondStart
	batchFinish.IdempotencyKey = "batch-finish"
	batchFinish.Type = "run.finished"
	batchFinish.Data.Sequence = 4
	batchFinish.Data.Outcome = "succeeded"
	batchFinish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
	batchFinish.PriorEvents = []tracker.NativeRunEvent{observation}
	rejectedCheckpointPrefix := batchFinish
	rejectedCheckpointPrefix.IdempotencyKey = "checkpoint-prefix-refused"
	rejectedCheckpointPrefix.PriorEvents = []tracker.NativeRunEvent{laterObservation}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(secondIssue.WorkItemID), rejectedCheckpointPrefix); err == nil {
		t.Fatal("checkpoint replay prefix was accepted")
	}
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), secondStart.Data.AttemptID)
		if err == nil && (attempt.Sequence != 3 || attempt.ClaimDisposition == nil || *attempt.ClaimDisposition != "progress" || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey != retainedCheckpointKey) {
			t.Fatalf("rejected prefix changed current publication claim facts: %+v", attempt)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(secondIssue.WorkItemID), batchFinish); err != nil {
		t.Fatalf("historical observation prefix and finish batch: %v", err)
	}
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), secondStart.Data.AttemptID)
		if err == nil && (attempt.ClaimDisposition == nil || *attempt.ClaimDisposition != "progress" || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey != retainedCheckpointKey) {
			t.Fatalf("replayed prefix replaced current publication claim facts: %+v", attempt)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	previousAccessRepository := service.config.AccessRepository
	service.config.AccessRepository = repository
	history, err := service.readHistory(t.Context(), scope, string(secondIssue.WorkItemID), url.Values{"limit": {"2"}})
	if err != nil || len(history.Items) != 2 || history.NextCursor == "" {
		t.Fatalf("typed oldest history page=%+v error=%v", history, err)
	}
	newestParams := url.Values{"direction": {"newest"}, "limit": {"2"}, "view": {"timeline"}}
	newest, err := service.readHistory(t.Context(), scope, string(secondIssue.WorkItemID), newestParams)
	if err != nil || len(newest.Items) != 2 || newest.Items[0].Type != "run.finished" || newest.Items[1].Type != "run.checkpointed" || newest.NextCursor == "" {
		t.Fatalf("typed newest history page=%+v error=%v", newest, err)
	}
	if newest.Items[0].Data.Run != nil || newest.Items[1].Data.Run != nil {
		t.Fatalf("timeline view retained run details: %+v", newest.Items)
	}
	for _, direction := range []string{"oldest", "newest"} {
		params := url.Values{"limit": {"200"}, "view": {"timeline"}}
		if direction == "newest" {
			params.Set("direction", direction)
		}
		page, err := service.readHistoryTenant(t.Context(), scope, string(secondIssue.WorkItemID), params)
		if err != nil || len(page.Items) == 0 {
			t.Fatalf("typed %s history at maximum limit: items=%d error=%v", direction, len(page.Items), err)
		}
	}
	newestParams.Set("cursor", newest.NextCursor)
	nextNewest, err := service.readHistory(t.Context(), scope, string(secondIssue.WorkItemID), newestParams)
	if err != nil || len(nextNewest.Items) != 2 || nextNewest.Items[0].Type != "run.observed" || nextNewest.Items[1].Type != "run.started" || nextNewest.NextCursor == "" {
		t.Fatalf("typed newest history continuation=%+v error=%v", nextNewest, err)
	}
	newestParams.Set("cursor", nextNewest.NextCursor)
	lastNewest, err := service.readHistory(t.Context(), scope, string(secondIssue.WorkItemID), newestParams)
	if err != nil || len(lastNewest.Items) != 1 || lastNewest.Items[0].Type != "issue.created" || lastNewest.NextCursor != "" {
		t.Fatalf("typed newest history final page=%+v error=%v", lastNewest, err)
	}
	reporterScope := scope
	reporterScope.credential.HostedRole = "reporter"
	reporterScope.sourceActor = &secondIssue.Actor
	if _, err := service.readHistoryTenant(t.Context(), reporterScope, string(secondIssue.WorkItemID), url.Values{"limit": {"2"}}); err != nil {
		t.Fatalf("reporter-owned history read: %v", err)
	}
	if _, err := service.readNativeAttemptTenant(t.Context(), reporterScope, string(secondIssue.WorkItemID), string(secondStart.Data.AttemptID)); err != nil {
		t.Fatalf("reporter-owned attempt read: %v", err)
	}
	if _, err := service.readNativeAttemptPageTenant(t.Context(), reporterScope, string(secondIssue.WorkItemID), url.Values{"limit": {"200"}}, json.Marshal); err != nil {
		t.Fatalf("reporter-owned attempt page: %v", err)
	}
	originalDB, originalReader := service.database.db, service.database.reader
	service.database.db, service.database.reader = nil, nil
	func() {
		defer func() {
			service.database.db, service.database.reader = originalDB, originalReader
		}()
		attemptsPath := itemPath + "/" + string(secondIssue.WorkItemID) + "/attempts?view=detail"
		response := performHubAPIRequest(t, service, http.MethodGet, attemptsPath, testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var attemptPage tracker.Page[tracker.NativeAttempt]
		decodeHubResponse(t, response, &attemptPage)
		if len(attemptPage.Items) == 0 {
			t.Fatal("typed attempts route returned no stored attempts")
		}
		attemptPath := itemPath + "/" + string(secondIssue.WorkItemID) + "/attempts/" + string(secondStart.Data.AttemptID)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, attemptPath, testHubAdminToken, nil), http.StatusOK)
		wrongItemAttemptPath := itemPath + "/" + string(issue.WorkItemID) + "/attempts/" + string(secondStart.Data.AttemptID)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, wrongItemAttemptPath, testHubAdminToken, nil), http.StatusNotFound)
		foreignProject := strings.Replace(itemPath, string(project.ID), "prj_foreign", 1) + "/" + string(secondIssue.WorkItemID) + "/attempts"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, foreignProject, testHubAdminToken, nil), http.StatusNotFound)
		foreignOrganization := strings.ReplaceAll(attemptsPath, string(organization.ID), "org_foreign")
		foreignOrganizationResponse := performHubAPIRequest(t, service, http.MethodGet, foreignOrganization, testHubAdminToken, nil)
		if foreignOrganizationResponse.Code != http.StatusNotFound && foreignOrganizationResponse.Code != http.StatusUnauthorized {
			t.Fatalf("foreign organization status=%d", foreignOrganizationResponse.Code)
		}
	}()
	foreignReporterScope := reporterScope
	foreignReporterScope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "another-reporter"}
	for _, read := range []struct {
		name string
		fn   func() error
	}{
		{name: "history", fn: func() error {
			_, err := service.readHistoryTenant(t.Context(), foreignReporterScope, string(secondIssue.WorkItemID), url.Values{"limit": {"2"}})
			return err
		}},
		{name: "attempt", fn: func() error {
			_, err := service.readNativeAttemptTenant(t.Context(), foreignReporterScope, string(secondIssue.WorkItemID), string(secondStart.Data.AttemptID))
			return err
		}},
		{name: "attempt page", fn: func() error {
			_, err := service.readNativeAttemptPageTenant(t.Context(), foreignReporterScope, string(secondIssue.WorkItemID), url.Values{"limit": {"2"}}, json.Marshal)
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			err := read.fn()
			var failure *nativeError
			if !errors.As(err, &failure) || failure.Code != "not_found" {
				t.Fatalf("foreign reporter read error=%v, want opaque not-found", err)
			}
		})
	}
	ownedRelation := []tracker.CollaborationEvent{{Data: tracker.CollaborationData{RelatedWorkItemID: secondIssue.WorkItemID}}}
	if err := service.hydrateTenantHistoryRelatedIssues(t.Context(), reporterScope, ownedRelation); err != nil || ownedRelation[0].Data.RelatedIssue == nil {
		t.Fatalf("reporter-owned related issue=%+v error=%v", ownedRelation, err)
	}
	foreignRelation := []tracker.CollaborationEvent{{Data: tracker.CollaborationData{RelatedWorkItemID: secondIssue.WorkItemID}}}
	if err := service.hydrateTenantHistoryRelatedIssues(t.Context(), foreignReporterScope, foreignRelation); err != nil || foreignRelation[0].Data.RelatedIssue != nil || foreignRelation[0].Data.RelatedWorkItemID != "" {
		t.Fatalf("foreign reporter related issue=%+v error=%v", foreignRelation, err)
	}
	nestedObservation := observation
	nestedObservation.PriorEvents = []tracker.NativeRunEvent{laterObservation}
	nestedBatch := batchFinish
	nestedBatch.IdempotencyKey = "nested-batch"
	nestedBatch.Data.Sequence = 5
	nestedBatch.PriorEvents = []tracker.NativeRunEvent{nestedObservation}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(secondIssue.WorkItemID), nestedBatch); err == nil {
		t.Fatal("nested prior-event batch was accepted")
	}
	runnerScope := scope
	runnerScope.credential = apiCredential{ID: credentialID, Hash: apikey.HashToken("example_" + binding.RunnerID), Scope: apiScopeWorker, NativeOnly: true, Runner: runnerauth.Identity{Binding: binding, Scope: "projects", OrganizationID: organization.ID, ProjectIDs: []tracker.ProjectID{project.ID}, Operations: operations, ExpiresAt: expires}}
	renewRequest := tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(secondLease.FencingToken), TTLSeconds: 7200, LandingCapacity: 3}
	renewBody, err := json.Marshal(renewRequest)
	if err != nil {
		t.Fatal(err)
	}
	renewResponse := httptest.NewRecorder()
	renewContext := service.echo.NewContext(httptest.NewRequest(http.MethodPost, "/leases/"+secondLease.LeaseID+"/renew", strings.NewReader(string(renewBody))).WithContext(t.Context()), renewResponse)
	renewContext.Set("native_scope", runnerScope)
	renewContext.SetParamNames("lease")
	renewContext.SetParamValues(secondLease.LeaseID)
	if err := service.renewNativeLeaseTenant(renewContext); err != nil || renewResponse.Code != http.StatusOK {
		t.Fatalf("renewNativeLeaseTenant status=%d error=%v body=%s", renewResponse.Code, err, renewResponse.Body.String())
	}
	var renewed tracker.NativeLease
	if err := json.Unmarshal(renewResponse.Body.Bytes(), &renewed); err != nil {
		t.Fatal(err)
	}
	if renewed.WorkItemID != secondIssue.WorkItemID || renewed.FencingToken != tracker.FencingToken(secondLease.FencingToken) {
		t.Fatalf("renewed lease response=%+v", renewed)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), secondLease.LeaseID)
		if err != nil {
			return err
		}
		if lease.LandingCapacity != 3 || !lease.ExpiresAt.After(secondLease.ExpiresAt) {
			t.Errorf("renewed PG lease=%+v", lease)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	releaseRequest := tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(secondLease.FencingToken), Reason: "completed"}
	releaseBody, err := json.Marshal(releaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	releaseResponse := httptest.NewRecorder()
	releaseContext := service.echo.NewContext(httptest.NewRequest(http.MethodPost, "/leases/"+secondLease.LeaseID+"/release", strings.NewReader(string(releaseBody))).WithContext(t.Context()), releaseResponse)
	releaseContext.Set("native_scope", runnerScope)
	releaseContext.SetParamNames("lease")
	releaseContext.SetParamValues(secondLease.LeaseID)
	if err := service.releaseNativeLeaseTenant(releaseContext); err != nil || releaseResponse.Code != http.StatusNoContent {
		t.Fatalf("releaseNativeLeaseTenant status=%d error=%v body=%s", releaseResponse.Code, err, releaseResponse.Body.String())
	}
	service.config.AccessRepository = previousAccessRepository
	thirdItemResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "third-item"}, Title: "Third example issue", Body: workItemRecordBody,
	})
	requireNativeStatus(t, thirdItemResponse, http.StatusOK)
	var thirdIssue tracker.NativeIssue
	decodeHubResponse(t, thirdItemResponse, &thirdIssue)
	var thirdLease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), project.ID, thirdIssue.WorkItemID)
		if err != nil {
			return err
		}
		thirdLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "third-session", LeaseID: "third-lease", TTL: time.Hour})
		if err != nil {
			return err
		}
		return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization.ID, LeaseID: thirdLease.LeaseID, RunnerID: binding.RunnerID, AssignedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	cancelStart := start
	cancelStart.IdempotencyKey = "cancel-start"
	cancelStart.Data.AttemptID = newNativeID("attempt")
	cancelStart.Data.RunID = newNativeID("run")
	cancelStart.Data.LeaseID = tracker.LeaseID(thirdLease.LeaseID)
	cancelStart.Data.FencingToken = tracker.FencingToken(thirdLease.FencingToken)
	if _, err := service.appendNativeRunEventTenant(t.Context(), runnerScope, string(thirdIssue.WorkItemID), cancelStart); err != nil {
		t.Fatal(err)
	}
	service.conversations = newConversationService(service, ConversationConfig{}.normalized())
	conversationID := conversation.NewConversationID()
	conversationNow := service.config.now().UTC()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: organization.ID, ID: conversationID, ProjectID: project.ID,
			OwnerPrincipalID: "test_hub_admin", Origin: conversationOriginWorker, Title: "Example worker issue",
			Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: string(thirdIssue.WorkItemID),
			LinkedAt: &conversationNow, Revision: 1,
			Execution: conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{
				AttemptID: cancelStart.Data.AttemptID, RunID: cancelStart.Data.RunID, LeaseID: string(thirdLease.LeaseID),
				FencingToken: int64(thirdLease.FencingToken), RunnerID: binding.RunnerID, MachineID: string(binding.MachineID),
			}, UpdatedAt: conversationNow},
			Preferences: conversation.DefaultPreferences(), CreatedAt: conversationNow, UpdatedAt: conversationNow,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cancelRequest := tracker.NativeLeaseMutation{CancelAttempt: true, FencingToken: tracker.FencingToken(thirdLease.FencingToken), Reason: "cancelled"}
	callCancelRelease := func() (*httptest.ResponseRecorder, error) {
		t.Helper()
		body, err := json.Marshal(cancelRequest)
		if err != nil {
			return nil, err
		}
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/leases/"+thirdLease.LeaseID+"/release", strings.NewReader(string(body))).WithContext(t.Context())
		cancelContext := service.echo.NewContext(request, response)
		cancelContext.Set("native_scope", runnerScope)
		cancelContext.SetParamNames("lease")
		cancelContext.SetParamValues(thirdLease.LeaseID)
		return response, service.releaseNativeLeaseTenant(cancelContext)
	}
	participant.reject = errors.New("injected release participant refusal")
	failedRelease, err := callCancelRelease()
	if err != nil || failedRelease.Code == http.StatusNoContent {
		t.Fatalf("participant-rejected cancellation release response=%v error=%v", failedRelease, err)
	}
	participant.reject = nil
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), thirdLease.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), string(cancelStart.Data.AttemptID))
		if err != nil {
			return err
		}
		if lease.ReleasedAt != nil || attempt.Status != "running" || attempt.Sequence != 1 {
			t.Errorf("rejected cancellation persisted lease=%+v attempt=%+v", lease, attempt)
		}
		events, err := r.Events(t.Context(), project.ID, thirdIssue.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		runEvents := 0
		for _, event := range events {
			if strings.HasPrefix(event.Type, "run.") {
				runEvents++
			}
		}
		if runEvents != 1 {
			t.Errorf("history after rejected cancellation=%+v", events)
		}
		linked, err := r.ConversationByExecutionLease(t.Context(), thirdLease.LeaseID)
		if err != nil {
			return err
		}
		if linked.ID != conversationID || linked.Execution.Settled {
			t.Errorf("rejected cancellation settled linked execution=%+v", linked.Execution)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var cancelRelease *httptest.ResponseRecorder
	originalNow := service.config.now
	service.config.now = func() time.Time { return thirdLease.RenewedAt.Add(-time.Second) }
	err = withClosedSQLite(t, service, organization.ID, func() error {
		var err error
		cancelRelease, err = callCancelRelease()
		return err
	})
	service.config.now = originalNow
	if err != nil || cancelRelease.Code != http.StatusNoContent {
		t.Fatalf("cancellation release status=%d error=%v body=%s", cancelRelease.Code, err, cancelRelease.Body.String())
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), thirdLease.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), string(cancelStart.Data.AttemptID))
		if err != nil {
			return err
		}
		events, err := r.Events(t.Context(), project.ID, thirdIssue.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		runEvents := 0
		var finish eventstore.Event
		for _, event := range events {
			if strings.HasPrefix(event.Type, "run.") {
				runEvents++
				finish = event
			}
		}
		if lease.ReleasedAt == nil || attempt.Status != "cancelled" || attempt.Sequence != 2 || runEvents != 2 || finish.Type != "run.finished" {
			t.Errorf("cancellation result lease=%+v attempt=%+v events=%+v", lease, attempt, events)
		}
		linked, err := r.ConversationByExecutionLease(t.Context(), thirdLease.LeaseID)
		if err != nil {
			return err
		}
		if linked.ID != conversationID || linked.Execution.Status != conversation.ExecutionInterrupted || !linked.Execution.Settled {
			t.Errorf("released conversation execution=%+v", linked.Execution)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.config.AccessRepository = repository
	cancelHistory, err := service.readHistory(t.Context(), scope, string(thirdIssue.WorkItemID), url.Values{"limit": {"20"}})
	service.config.AccessRepository = previousAccessRepository
	if err != nil {
		t.Fatalf("cancelled attempt PG history: %v", err)
	}
	var cancelledEvent *tracker.CollaborationEvent
	for i := range cancelHistory.Items {
		if cancelHistory.Items[i].Type == "run.finished" && cancelHistory.Items[i].Data.Run != nil {
			cancelledEvent = &cancelHistory.Items[i]
		}
	}
	cancelledAttempt, err := service.readNativeAttemptTenant(t.Context(), scope, string(thirdIssue.WorkItemID), string(cancelStart.Data.AttemptID))
	if err != nil {
		t.Fatalf("cancelled attempt detail: %v", err)
	}
	wantCancelledData := cancelledAttempt.NativeRunData
	wantCancelledData.ArtifactIDs = nil
	if cancelledEvent == nil || !reflect.DeepEqual(*cancelledEvent.Data.Run, wantCancelledData) {
		t.Fatalf("PG history lost cancellation payload: event=%+v, want=%+v", cancelledEvent, wantCancelledData)
	}
	readAttempt, err := service.readNativeAttemptTenant(t.Context(), scope, string(secondIssue.WorkItemID), string(secondStart.Data.AttemptID))
	if err != nil || readAttempt.Sequence != 4 || readAttempt.Outcome != "succeeded" || readAttempt.Status != "succeeded" || len(readAttempt.Unavailable) != 0 {
		t.Fatalf("tenant attempt detail=%+v error=%v", readAttempt, err)
	}
	service.config.AccessRepository = repository
	readPage, err := service.readNativeAttemptPageTenant(t.Context(), scope, string(secondIssue.WorkItemID), url.Values{"limit": {"1"}}, json.Marshal)
	if err != nil || len(readPage.Items) != 1 || readPage.Items[0].AttemptID != string(secondStart.Data.AttemptID) || readPage.Items[0].Sequence != 4 {
		t.Fatalf("tenant attempt page=%+v error=%v", readPage.Page, err)
	}
	attemptPath := itemPath + "/" + string(secondIssue.WorkItemID) + "/attempts"
	listResponse := httptest.NewRecorder()
	listContext := service.echo.NewContext(httptest.NewRequest(http.MethodGet, attemptPath, nil).WithContext(t.Context()), listResponse)
	listContext.Set("native_scope", scope)
	listContext.SetParamNames("item")
	listContext.SetParamValues(string(secondIssue.WorkItemID))
	if err := service.listNativeAttempts(listContext); err != nil || listResponse.Code != http.StatusOK {
		t.Fatalf("listNativeAttempts status=%d error=%v body=%s", listResponse.Code, err, listResponse.Body.String())
	}
	var listed tracker.Page[tracker.NativeAttempt]
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].AttemptID != string(secondStart.Data.AttemptID) || listed.Items[0].Sequence != 4 {
		t.Fatalf("HTTP attempt list=%+v", listed)
	}
	detailResponse := httptest.NewRecorder()
	detailContext := service.echo.NewContext(httptest.NewRequest(http.MethodGet, attemptPath+"/"+string(secondStart.Data.AttemptID), nil).WithContext(t.Context()), detailResponse)
	detailContext.Set("native_scope", scope)
	detailContext.SetParamNames("item", "attempt")
	detailContext.SetParamValues(string(secondIssue.WorkItemID), string(secondStart.Data.AttemptID))
	if err := service.getNativeAttempt(detailContext); err != nil || detailResponse.Code != http.StatusOK {
		t.Fatalf("getNativeAttempt status=%d error=%v body=%s", detailResponse.Code, err, detailResponse.Body.String())
	}
	var detailed tracker.NativeAttempt
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detailed); err != nil {
		t.Fatal(err)
	}
	if detailed.AttemptID != string(secondStart.Data.AttemptID) || detailed.Sequence != 4 || detailed.Outcome != "succeeded" {
		t.Fatalf("HTTP attempt detail=%+v", detailed)
	}
	service.config.AccessRepository = previousAccessRepository
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), secondStart.Data.AttemptID)
		if err != nil {
			return err
		}
		if attempt.Sequence != 4 || attempt.Status != "succeeded" {
			t.Errorf("attempt after nested-batch rejection=%+v", attempt)
		}
		events, err := r.Events(t.Context(), project.ID, secondIssue.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		runEvents := 0
		for _, event := range events {
			if strings.HasPrefix(event.Type, "run.") {
				runEvents++
			}
		}
		if runEvents != 4 {
			t.Errorf("second issue history events=%+v", events)
		}
		if _, err := r.Receipt(t.Context(), "test_hub_admin", "POST /api/v2/organizations/"+string(organization.ID)+"/projects/"+string(project.ID)+"/work-items/"+string(secondIssue.WorkItemID)+"/events", "nested-batch"); !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("nested batch receipt=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFirstRunEventAfterClaimRenewAndWorkflow(t *testing.T) {
	repository := projectRecordPostgres(t)
	var logs bytes.Buffer
	service := openPostgresHubTestService(t, repository, Config{RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	for _, name := range []string{"First example organization", "Second example organization"} {
		organization, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(string(organization.ID), func(t *testing.T) {
			seedPostgresHubAdmin(t, repository, service, organization)
			service.localOrganization = string(organization.ID)
			projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
			response := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "fence-project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
			requireNativeStatus(t, response, http.StatusCreated)
			var project tracker.NativeProject
			decodeHubResponse(t, response, &project)
			base := projectPath + "/" + string(project.ID)
			now := service.config.now().UTC().Truncate(time.Microsecond)
			expires := now.Add(time.Hour)
			policy, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization.ID, hubTestPolicy())
			if err != nil {
				t.Fatal(err)
			}
			policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = project.ID, "test_hub_admin", now
			binding := runnerauth.NewBinding()
			token := "example_runner_" + string(organization.ID)
			credential := "credential_" + binding.RunnerID
			enrollment := "enrollment_" + binding.RunnerID
			hash := apikey.HashToken(token)
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			err = repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.ApproveProjectPolicy(t.Context(), policy, ""); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credential, Name: "Example runner", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credential, ProjectID: project.ID}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollment, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken("example_enrollment_" + binding.RunnerID), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), enrollment, binding.RunnerID, binding.MachineID, now); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: string(binding.MachineID), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: credential, EnrollmentID: enrollment, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
				return w.UpdateRunnerCapacityConfig(t.Context(), binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now})
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, scenario := range []string{"renew only", "dispatch start", "intervening edit"} {
				t.Run(scenario, func(t *testing.T) {
					response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: scenario}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
					requireNativeStatus(t, response, http.StatusOK)
					var issue tracker.NativeIssue
					decodeHubResponse(t, response, &issue)
					path := base + "/work-items/" + string(issue.WorkItemID)
					claim := tracker.NativeClaim{PolicyID: policy.PolicyID, WorkItemID: issue.WorkItemID, MachineID: binding.MachineID, SessionID: newNativeID("session"), TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
					response = performHubAPIRequest(t, service, http.MethodPost, base+"/claims", token, claim)
					requireNativeStatus(t, response, http.StatusOK)
					var lease tracker.NativeLease
					decodeHubResponse(t, response, &lease)
					for range 2 {
						response = performHubAPIRequest(t, service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/renew", token, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 300})
						requireNativeStatus(t, response, http.StatusOK)
						var renewed tracker.NativeLease
						decodeHubResponse(t, response, &renewed)
						if renewed.FencingToken != lease.FencingToken {
							t.Fatal("renew changed the acquisition fence")
						}
					}
					if scenario == "intervening edit" {
						issue.Revision++
						issue.Title = "Updated example issue"
						raw, err := json.Marshal(issue)
						if err != nil {
							t.Fatal(err)
						}
						version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization.ID, ProjectID: project.ID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
						if err != nil {
							t.Fatal(err)
						}
						document, err := searchstore.NewContent(service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: organization.ID, ProjectID: project.ID, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: now}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
							item, err := w.WorkItem(t.Context(), project.ID, issue.WorkItemID)
							if err != nil {
								return err
							}
							item.Title = issue.Title
							if _, err = w.UpdateWorkItem(t.Context(), item); err != nil {
								return err
							}
							if _, err = w.CreateCollaborationVersion(t.Context(), version); err != nil {
								return err
							}
							return w.PutSearch(t.Context(), document, int64(issue.Revision)-1)
						}); err != nil {
							t.Fatal(err)
						}
					}
					if scenario != "renew only" {
						transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "dispatch-start", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: issue.Revision, State: "In Progress", Reason: "worker_progress", ReasonDetail: "dispatch start"}
						response = performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", token, transition)
						requireNativeStatus(t, response, http.StatusOK)
						replay := performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", token, transition)
						requireNativeStatus(t, replay, http.StatusOK)
						if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
							t.Fatal("workflow replay changed response")
						}
					}
					start := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "first-event"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: policy.PolicyID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}}
					stale := start
					stale.IdempotencyKey = "stale-event"
					stale.Data.FencingToken++
					response = performHubAPIRequest(t, service, http.MethodPost, path+"/events", token, stale)
					requireNativeStatus(t, response, http.StatusConflict)
					if !strings.Contains(response.Body.String(), "stale_fencing_token") {
						t.Fatalf("stale refusal=%s", response.Body.String())
					}
					logs.Reset()
					response = performHubAPIRequest(t, service, http.MethodPost, path+"/events", token, start)
					want := http.StatusOK
					if scenario == "intervening edit" {
						want = http.StatusConflict
						if !strings.Contains(response.Body.String(), "stale_fencing_token") || !strings.Contains(logs.String(), `"fence_check":"dispatch_revision"`) {
							t.Errorf("unrelated edit lost revision fence: body=%s logs=%s", response.Body.String(), logs.String())
						}
					}
					if response.Code != want {
						t.Errorf("first event after %s status=%d want=%d body=%s logs=%s", scenario, response.Code, want, response.Body.String(), logs.String())
					}
					if scenario == "dispatch start" && response.Code == http.StatusOK {
						next := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "after-start", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: issue.Revision + 1, State: "Todo", Reason: "worker_progress"}
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", token, next), http.StatusOK)
						checkpoint := start
						checkpoint.IdempotencyKey, checkpoint.Type, checkpoint.Data.Sequence = "checkpoint", "run.checkpointed", 2
						checkpoint.Data.Handoff = nativeTestCheckpoint()
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", token, checkpoint), http.StatusOK)
						observation := start
						observation.IdempotencyKey, observation.Type, observation.Data.Sequence = "after-checkpoint", "run.observed", 3
						observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: service.config.now().UTC()}
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", token, observation), http.StatusOK)
					}
					response = performHubAPIRequest(t, service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/release", token, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"})
					requireNativeStatus(t, response, http.StatusNoContent)
					start.IdempotencyKey = "released-event"
					response = performHubAPIRequest(t, service, http.MethodPost, path+"/events", token, start)
					requireNativeStatus(t, response, http.StatusConflict)
					if !strings.Contains(response.Body.String(), "stale_fencing_token") {
						t.Fatalf("released lease refusal=%s", response.Body.String())
					}
				})
			}
		})
	}
}

func TestRuntimeRefreshPreservesAuditWithoutGrowingHistory(t *testing.T) {
	f := newRunnerPGFixture(t)
	f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
	envelope, err := f.service.config.SecretKeys.Seal(bytes.Repeat([]byte{0x35}, 32), accessstore.SecretAAD("org_startup", "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: time.Now().UTC()}, accessstore.SecretAudit{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	for index, lease := range f.leases {
		itemPath := "/api/v2/organizations/org_startup/projects/prj_runner/work-items/" + string(lease.WorkItemID)
		credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), "org_startup", apikey.HashToken(f.tokens[index]), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org_startup", project: "prj_runner", credential: credential}
		data := f.data[index]
		var pending []tracker.NativeRunEvent
		var last tracker.NativeRunEvent
		for sequence := int64(2); sequence <= 5; sequence++ {
			data.Sequence = sequence
			phase := "implementation"
			if sequence == 5 {
				phase = "validation"
			}
			data.Runtime = &tracker.NativeRuntimeObservation{Phase: phase, HeartbeatAt: time.Now().UTC()}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + "-history-" + strconv.FormatInt(sequence, 10)}, Type: "run.observed", SchemaVersion: 1, Data: data}
			if index == 1 && sequence > 2 && sequence < 5 {
				pending = append(pending, event)
				continue
			}
			event.PriorEvents = pending
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", f.tokens[index], event), http.StatusOK)
			last = event
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", f.tokens[index], last), http.StatusOK)
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.Events(t.Context(), scope.project, lease.WorkItemID, 0, 10)
			if err == nil && len(rows) != 5 {
				t.Fatalf("immutable audit rows = %d, want 5", len(rows))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, direction := range []string{"oldest", "newest"} {
			query := url.Values{"view": {"timeline"}, "limit": {"2"}, "direction": {direction}}
			first, err := f.service.readHistoryTenant(t.Context(), scope, string(lease.WorkItemID), query)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Items) != 2 || first.NextCursor == "" {
				t.Fatalf("first history page = %+v", first)
			}
			query.Set("cursor", first.NextCursor)
			next, err := f.service.readHistoryTenant(t.Context(), scope, string(lease.WorkItemID), query)
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Items) != 1 || next.NextCursor != "" {
				t.Fatalf("runtime refresh grew %s history: %+v", direction, next)
			}
			if direction == "oldest" && (first.Items[0].Type != "run.started" || next.Items[0].ID != nativeAttemptHistoryID("run.observed", last.Data)) {
				t.Fatal("meaningful oldest history lost order or identity")
			}
			if direction == "newest" && (first.Items[0].ID != nativeAttemptHistoryID("run.observed", last.Data) || next.Items[0].Type != "run.started") {
				t.Fatal("meaningful newest history lost order or identity")
			}
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/attempts/"+data.AttemptID, f.tokens[index], nil)
		requireNativeStatus(t, response, http.StatusOK)
		var attempt tracker.NativeAttempt
		decodeHubResponse(t, response, &attempt)
		if attempt.Sequence != 5 || attempt.Runtime == nil || attempt.Runtime.Phase != "validation" {
			t.Fatalf("latest immutable attempt changed: %+v", attempt)
		}
	}
}
