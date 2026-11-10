package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f nativeFixture) worker(t *testing.T, name string) string {
	t.Helper()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	binding := runnerauth.NewBinding()
	credentialID, enrollmentID := "credential_"+binding.RunnerID, "enrollment_"+binding.RunnerID
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	expires := now.Add(runnerauth.CredentialTTL)
	operations := []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}
	projects := []tracker.ProjectID{f.project.ID}
	hash := apikey.HashToken(token)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: credentialID, Name: name, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: credentialID, ProjectID: f.project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: f.project.OrganizationID, ID: enrollmentID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken(newNativeID("enrollment")), Scope: "projects", ProjectIDs: projects, Operations: operations, CreatedAt: now, ExpiresAt: now.Add(runnerauth.MaxEnrollmentTTL), CreatedBy: "test_hub_admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, binding.RunnerID, binding.MachineID, now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: f.project.OrganizationID, ID: string(binding.MachineID), Hostname: "example", Capacity: 1, Version: f.service.config.Version}); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: f.project.OrganizationID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", DisplayName: name, MachineUse: "dedicated", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: projects, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, hash, "", "")
	if err != nil || status != http.StatusOK || credential.Scope != apiScopeWorker || credential.HostedKeyKind != "service" || credential.Runner.Binding != binding || credential.Runner.Scope != "projects" || !slices.Equal(credential.Runner.ProjectIDs, projects) {
		t.Fatalf("fixture worker authority status=%d: %v", status, err)
	}
	return token
}

func TestNativeClaimsEventsAndRestartWithoutGitHub(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	config := Config{now: func() time.Time { return now }, Version: "v0.117.50"}
	f := newNativeFixture(t, newClockedPostgresNativeService(t, config), "", "claims")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Core.RequireDependencies = true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	descriptor := hubTestPolicy()
	issue := f.create(t, "work")
	blocker := f.create(t, "blocker")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "block"}, ExpectedRevision: 1, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
	worker := f.worker(t, "worker")
	otherWorker := f.worker(t, "other-worker")
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("worker authentication status=%d: %v", status, err)
	}
	machineID := credential.Runner.MachineID
	machine := map[string]any{"id": machineID, "hostname": "runner", "display_name": "Runner", "version": "v0.117.50", "capacity": 1, "backend_isolation": isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, machine), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", otherWorker, machine), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, machine), http.StatusNotFound)
	heartbeat := func(version string) {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(machineID)+"/heartbeat", worker, map[string]any{"display_name": "Runner", "version": version, "capacity": 1, "backend_isolation": isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now}})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		if snapshot.RunnerID != credential.Runner.RunnerID || snapshot.Revision < 1 {
			t.Fatalf("heartbeat routing=%+v", snapshot)
		}
	}
	heartbeat("v0.117.50")
	request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: machineID, SessionID: "native-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	drainTestNativeBoard(t, f.service)
	var beforeRefusal viewstore.BoardSequence
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		beforeRefusal, err = r.BoardSequence(t.Context(), f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	refusal := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
	requireNativeStatus(t, refusal, http.StatusConflict)
	drainTestNativeBoard(t, f.service)
	var afterRefusal viewstore.BoardSequence
	var deltas []viewstore.BoardDelta
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		afterRefusal, err = r.BoardSequence(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		deltas, err = r.BoardDeltas(t.Context(), f.project.ID, beforeRefusal.Sequence, 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if afterRefusal.ReplayFloor > beforeRefusal.Sequence || afterRefusal.Sequence != beforeRefusal.Sequence+1 || len(deltas) != 1 || deltas[0].Current == nil {
		t.Fatalf("claim refusal sequence=%#v deltas=%#v response=%s", afterRefusal, deltas, refusal.Body.String())
	}
	card, err := viewstore.NewContent(f.service.runDetailStore()).Board(t.Context(), f.project.OrganizationID, *deltas[0].Current)
	if err != nil {
		t.Fatal(err)
	}
	if card.Issue.WorkItemID != issue.WorkItemID || !card.Issue.LastActivityAt.Equal(now) {
		t.Fatalf("claim refusal did not refresh card activity: %#v", card)
	}

	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(blocker.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "unblock"}, ExpectedRevision: 1, State: "Done", Reason: "user_requested"}), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	retry := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
	requireNativeStatus(t, retry, http.StatusOK)
	if response.Body.String() != retry.Body.String() {
		t.Fatal("claim retry changed the lease")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", otherWorker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusNotFound)
	event := nativeStartedEvent(lease)
	f.service.config.Version = "v0.117.60"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	upgradeIssue := f.create(t, "upgrade-check")
	newClaim := request
	newClaim.SessionID = "new-session"
	newClaim.WorkItemID = upgradeIssue.WorkItemID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, newClaim), http.StatusUpgradeRequired)
	for range 3 {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	}
	for _, test := range []struct {
		name string
		body any
	}{
		{"raw prompt", map[string]any{"idempotency_key": "prompt", "type": "run.started", "schema_version": 1, "data": event.Data, "prompt": "private prompt"}},
		{"raw payload", map[string]any{"idempotency_key": "payload", "type": "run.started", "schema_version": 1, "payload": map[string]any{"secret": "example-secret"}}},
		{"artifact content", map[string]any{"idempotency_key": "artifact", "type": "run.checkpointed", "schema_version": 1, "data": map[string]any{"artifact_content": "private content"}}},
		{"unknown version", tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "version"}, Type: event.Type, SchemaVersion: 2, Data: event.Data}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, test.body), http.StatusUnprocessableEntity)
		})
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, worker, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	now = now.Add(91 * time.Second)
	heartbeat("v0.117.60")
	readAuthority := func(t *testing.T) (leasestore.Lease, leasestore.Attempt) {
		t.Helper()
		var current leasestore.Lease
		var attempt leasestore.Attempt
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			current, err = r.Lease(t.Context(), string(lease.ID))
			if err != nil {
				return err
			}
			attempt, err = r.Attempt(t.Context(), event.Data.AttemptID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return current, attempt
	}
	expiredLease, expiredAttempt := readAuthority(t)
	if expiredLease.ExpiresAt.After(now) || expiredLease.ReleasedAt != nil || expiredAttempt.Status != "running" {
		t.Fatalf("expected expired unreleased authority before reclaim: lease=%+v attempt=%+v", expiredLease, expiredAttempt)
	}
	for _, test := range []struct {
		name       string
		credential string
		fence      tracker.FencingToken
		status     int
	}{
		{name: "another owner cannot renew expired lease", credential: otherWorker, fence: lease.FencingToken, status: http.StatusNotFound},
		{name: "wrong token cannot renew expired lease", credential: worker, fence: lease.FencingToken + 1, status: http.StatusConflict},
		{name: "original owner cannot renew expired lease", credential: worker, fence: lease.FencingToken, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", test.credential, tracker.NativeLeaseMutation{FencingToken: test.fence, TTLSeconds: 90})
			requireNativeStatus(t, response, test.status)
			current, attempt := readAuthority(t)
			if !reflect.DeepEqual(current, expiredLease) || !reflect.DeepEqual(attempt, expiredAttempt) {
				t.Fatalf("refused expired renewal changed authority: lease=%+v attempt=%+v", current, attempt)
			}
		})
	}
	request.SessionID = "replacement-session"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	var replacement tracker.NativeLease
	decodeHubResponse(t, response, &replacement)
	if replacement.ID == lease.ID || replacement.FencingToken <= lease.FencingToken || replacement.SessionID != request.SessionID || replacement.MachineID != machineID || !replacement.ExpiresAt.After(now) {
		t.Fatalf("reclaim did not grant fresh authority: original=%+v replacement=%+v", lease, replacement)
	}
	retiredLease, retiredAttempt := readAuthority(t)
	if retiredLease.ReleasedAt == nil || retiredLease.ReleaseReason != "expired" || retiredLease.FencingToken != expiredLease.FencingToken || retiredAttempt.Status != "cancelled" || retiredAttempt.LeaseID != string(lease.ID) || retiredAttempt.FencingToken != int64(lease.FencingToken) {
		t.Fatalf("reclaim did not retire expired authority: lease=%+v attempt=%+v", retiredLease, retiredAttempt)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(replacement.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: replacement.FencingToken, TTLSeconds: 90}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
	event.IdempotencyKey = "stale-new-event"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusConflict)
	lateFinish := event
	lateFinish.IdempotencyKey, lateFinish.Type, lateFinish.Data.Outcome = "stale-finish", "run.finished", "succeeded"
	lateFinish.Data.Sequence = event.Data.Sequence + 1
	lateFinish.Data.MachineID, lateFinish.Data.RunnerID, lateFinish.Data.SessionID = machineID, credential.Runner.RunnerID, lease.SessionID
	refusedFinish := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, lateFinish)
	requireNativeStatus(t, refusedFinish, http.StatusConflict)
	var finishError apiErrorResponse
	decodeHubResponse(t, refusedFinish, &finishError)
	if finishError.Code != "run_sequence_conflict" {
		t.Fatalf("late completion refusal=%+v", finishError)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, worker, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(replacement.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: replacement.FencingToken, TTLSeconds: 90}), http.StatusOK)
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Lease(t.Context(), string(replacement.ID))
		if err == nil && (current.LeaseID != string(replacement.ID) || current.FencingToken != int64(replacement.FencingToken) || current.SessionID != replacement.SessionID || current.ReleasedAt != nil || !current.ExpiresAt.After(now)) {
			t.Fatalf("restart lost reclaimed authority: %+v", current)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	currentRetiredLease, currentRetiredAttempt := readAuthority(t)
	if !reflect.DeepEqual(currentRetiredLease, retiredLease) || !reflect.DeepEqual(currentRetiredAttempt, retiredAttempt) {
		t.Fatalf("late commands or restart changed retired authority: lease=%+v attempt=%+v", currentRetiredLease, currentRetiredAttempt)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &history)
	wantHistory := []string{"issue.created", "dependency.changed", "scheduler.decision", "scheduler.decision", "run.started", "scheduler.decision"}
	if len(history.Items) != len(wantHistory) {
		t.Fatalf("history after restart = %#v", history.Items)
	}
	for i, kind := range wantHistory {
		event := history.Items[i]
		if event.Type != kind || event.AggregateSequence != int64(i+1) {
			t.Fatalf("history event %d = %#v, want %s", i, event, kind)
		}
		if kind == "scheduler.decision" {
			source, outcome := "native_claim", "claimed"
			if i == 2 {
				source, outcome = "native_claim_eligibility", "skipped"
			}
			if event.Data.Decision == nil || event.Data.Decision.Source != source || event.Data.Decision.Outcome != outcome {
				t.Fatalf("claim decision %d = %#v", i, event.Data.Decision)
			}
		}
	}
	f.service.config.Version = "v0.117.61"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(replacement.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: replacement.FencingToken, Reason: "completed"}), http.StatusNoContent)
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		for _, item := range []tracker.NativeWorkItemID{issue.WorkItemID, blocker.WorkItemID, upgradeIssue.WorkItemID} {
			if _, err := r.LinkedSource(t.Context(), f.project.ID, item); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("native work acquired a linked GitHub source: item=%s error=%v", item, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	admin := nativeAttemptTestDatabase(t, f)
	var outbox int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if outbox != 0 {
		t.Fatalf("native work acquired GitHub outbox: %d", outbox)
	}
	for _, table := range []string{"collaboration_events", "collaboration_versions"} {
		_, err := admin.ExecContext(t.Context(), "DELETE FROM "+table+" WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
			t.Errorf("%s ordinary deletion error=%v, want immutable-record refusal", table, err)
		}
	}
}

func TestNativeRunnerMinimumVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, hub, runner, minimum string
		refused                    bool
	}{
		{name: "below", hub: "v0.117.60", runner: "v0.117.55", minimum: "0.117.56", refused: true},
		{name: "older supported release", hub: "v0.117.60", runner: "v0.117.57", minimum: "0.117.56"},
		{name: "supported minimum", hub: "v0.117.60", runner: "v0.117.56", minimum: "0.117.56"},
		{name: "above", hub: "v0.117.60", runner: "v0.118.0", minimum: "0.117.56"},
		{name: "prerelease", hub: "v0.117.60", runner: "v0.117.56-rc.1", minimum: "0.117.56", refused: true},
		{name: "build metadata", hub: "v0.117.60", runner: "v0.117.56+local", minimum: "0.117.56"},
		{name: "unprefixed", hub: " v0.117.60 ", runner: "0.117.55", minimum: "0.117.56", refused: true},
		{name: "development runner", hub: "v0.117.60", runner: "dev", minimum: "0.117.56"},
		{name: "hub older than supported minimum", hub: "v0.117.50", runner: "v0.117.49", minimum: "v0.117.50", refused: true},
		{name: "development hub", hub: "dev", runner: "v1.0.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Version: test.hub}), "", "minimum")
			descriptor := hubTestPolicy()
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("worker authentication status=%d: %v", status, err)
			}
			machineID := credential.Runner.MachineID
			capabilitiesRequest := newHubAPIRequest(t, http.MethodGet, "/api/v2/capabilities", worker, nil)
			capabilitiesRequest.Header.Set("X-Detent-Organization", string(f.project.OrganizationID))
			capabilities := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(capabilities, capabilitiesRequest)
			requireNativeStatus(t, capabilities, http.StatusOK)
			var published nativeCapabilitiesResponse
			decodeHubResponse(t, capabilities, &published)
			if published.MinimumRunnerVersion != test.minimum {
				t.Fatalf("published minimum = %q, want %q", published.MinimumRunnerVersion, test.minimum)
			}
			machine := map[string]any{"id": machineID, "hostname": "runner", "display_name": "Runner", "version": test.runner, "capacity": 1, "backend_isolation": isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, machine), http.StatusOK)
			heartbeat := func(version string) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(machineID)+"/heartbeat", worker, map[string]any{"display_name": "Runner", "version": version, "capacity": 1, "backend_isolation": isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: f.service.config.now()}})
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				if snapshot.RunnerID != credential.Runner.RunnerID || snapshot.Revision < 1 {
					t.Fatalf("heartbeat routing=%+v", snapshot)
				}
			}
			heartbeat(test.runner)
			request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: machineID, SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
			wantStatus := http.StatusOK
			if test.refused {
				wantStatus = http.StatusUpgradeRequired
			}
			requireNativeStatus(t, response, wantStatus)
			if test.refused {
				var failure apiErrorResponse
				decodeHubResponse(t, response, &failure)
				if failure.Code != "unavailable" || failure.Message != "Too old to take work, needs "+test.minimum || failure.Message != runnerClaimRefusal(test.minimum, test.runner) {
					t.Fatalf("claim refusal = %#v", failure)
				}
				var count int
				if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM leases WHERE organization_id=$1", f.project.OrganizationID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("refused runner acquired %d leases", count)
				}
				heartbeat(test.minimum)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request), http.StatusOK)
			}
		})
	}
}

func TestNativeRequestedClaimRefusalsAreTenantScopedAndDeduplicated(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := newClockedPostgresNativeService(t, Config{now: func() time.Time { return now }, Version: "v0.117.60"})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(organization), func(t *testing.T) {
			now = time.Now().UTC().Truncate(time.Microsecond)
			service := service
			if organization != "org_first" {
				service = reopenPostgresNativeService(t, service)
				service.database.hostedOrganization = organization
				service.localOrganization = string(organization)
				service.requestMetrics.organization = organization
			}
			f := newNativeFixture(t, service, organization, "refusals")
			perform := func(method, path, token string, payload any) *httptest.ResponseRecorder {
				request := newHubAPIRequest(t, method, path, token, payload)
				request.Header.Set("X-Detent-Organization", string(organization))
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				return response
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				project.Core.RequireDependencies = true
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			issue, blocker := f.create(t, "work"), f.create(t, "blocker")
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			requireNativeStatus(t, perform(http.MethodPost, path+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "block"}, ExpectedRevision: 1, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
			workers := []string{f.worker(t, "first"), f.worker(t, "second")}
			requests := make([]tracker.NativeClaim, len(workers))
			for i, worker := range workers {
				credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(worker), "", "")
				if err != nil || status != http.StatusOK {
					t.Fatalf("worker authentication status=%d: %v", status, err)
				}
				machine := credential.Runner.MachineID
				report := map[string]any{"id": machine, "hostname": "example", "display_name": "Example runner", "version": "v0.117.60", "capacity": 1, "backend_isolation": isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
				requireNativeStatus(t, perform(http.MethodPost, f.base+"/machines/register", worker, report), http.StatusOK)
				report["capacity_configuration"] = runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now}
				requireNativeStatus(t, perform(http.MethodPost, f.base+"/machines/"+string(machine)+"/heartbeat", worker, report), http.StatusOK)
				requests[i] = tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: machine, SessionID: "session_" + credential.Runner.RunnerID, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			}
			type state struct {
				revision, sequence, board int64
				leases, attempts          int
				activity                  time.Time
			}
			read := func() state {
				t.Helper()
				drainTestNativeBoard(t, service)
				var value state
				if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					item, err := r.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
					if err != nil {
						return err
					}
					board, err := r.BoardSequence(t.Context(), f.project.ID)
					value.revision, value.sequence, value.board, value.activity = item.Revision, item.EventSequence, board.Sequence, item.LastActivityAt
					return err
				}); err != nil {
					t.Fatal(err)
				}
				admin := nativeAttemptTestDatabase(t, f)
				for _, query := range []struct {
					sql   string
					count *int
				}{
					{"SELECT count(*) FROM leases WHERE organization_id=$1", &value.leases},
					{"SELECT count(*) FROM native_attempts WHERE organization_id=$1", &value.attempts},
				} {
					if err := admin.QueryRowContext(t.Context(), query.sql, organization).Scan(query.count); err != nil {
						t.Fatal(err)
					}
				}
				return value
			}
			refuse := func(i int) {
				t.Helper()
				response := perform(http.MethodPost, f.base+"/claims", workers[i], requests[i])
				requireNativeStatus(t, response, http.StatusConflict)
				if !strings.Contains(response.Body.String(), `"code":"no_claimable_work"`) {
					t.Fatalf("refusal=%s", response.Body.String())
				}
			}
			before := read()
			now = now.Add(time.Second)
			refuse(0)
			after := read()
			if after.revision != before.revision || after.sequence != before.sequence+1 || after.board != before.board+1 || !after.activity.Equal(now) || after.leases != 0 || after.attempts != 0 {
				t.Fatalf("first refusal before=%+v after=%+v", before, after)
			}
			response := perform(http.MethodGet, path+"/history", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var history tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &history)
			decision := history.Items[len(history.Items)-1].Data.Decision
			if decision == nil || decision.Outcome != "skipped" || decision.Source != "native_claim_eligibility" || decision.Reason != "no_claimable_work" || int64(decision.WorkItemRevision) != after.revision {
				t.Fatalf("refusal decision=%+v", decision)
			}
			assertDuplicate := func() {
				t.Helper()
				before := read()
				original := service.config.TenantRepository
				counted := &nativeClaimWriteCounter{Repository: original.(*postgres.Repository)}
				func() {
					service.config.TenantRepository = counted
					defer func() { service.config.TenantRepository = original }()
					for range 3 {
						refuse(0)
					}
				}()
				if counted.writes != 0 {
					t.Fatalf("identical polls opened %d write transactions", counted.writes)
				}
				if after := read(); after != before {
					t.Fatalf("identical polls mutated durable state: before=%+v after=%+v", before, after)
				}
				t.Logf("tenant=%s identical_polls=3 write_transactions=%d durable_state_unchanged=true", organization, counted.writes)
			}
			assertDuplicate()
			now = now.Add(time.Second)
			refuse(1)
			interleaved := read()
			if interleaved.sequence != after.sequence+1 || interleaved.board != after.board+1 || interleaved.revision != after.revision || interleaved.leases != 0 || interleaved.attempts != 0 {
				t.Fatalf("second runner refusal=%+v", interleaved)
			}
			assertDuplicate()
			requireNativeStatus(t, perform(http.MethodPost, f.base+"/work-items/"+string(blocker.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "unblock"}, ExpectedRevision: 1, State: "Done", Reason: "user_requested"}), http.StatusOK)
			claimed := perform(http.MethodPost, f.base+"/claims", workers[0], requests[0])
			requireNativeStatus(t, claimed, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, claimed, &lease)
			requireNativeStatus(t, perform(http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", workers[0], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
			requireNativeStatus(t, perform(http.MethodPost, f.base+"/work-items/"+string(blocker.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "block-again"}, ExpectedRevision: 2, State: "Todo", Reason: "user_requested"}), http.StatusOK)
			requests[0].SessionID += "_after_claim"
			beforeReset := read()
			now = now.Add(time.Second)
			refuse(0)
			afterReset := read()
			if afterReset.revision != before.revision || afterReset.sequence != beforeReset.sequence+1 || afterReset.board != beforeReset.board+1 || afterReset.leases != beforeReset.leases || afterReset.attempts != beforeReset.attempts || !afterReset.activity.Equal(now) {
				t.Fatalf("committed claim did not reset refusal dedup: before=%+v after=%+v", beforeReset, afterReset)
			}
			assertDuplicate()
			title := "Updated required work"
			requireNativeStatus(t, perform(http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "new-source-revision"}, ExpectedRevision: tracker.Revision(afterReset.revision), Title: &title}), http.StatusOK)
			beforeRevision := read()
			now = now.Add(time.Second)
			refuse(0)
			afterRevision := read()
			if afterRevision.revision != afterReset.revision+1 || afterRevision.sequence != beforeRevision.sequence+1 || afterRevision.board != beforeRevision.board+1 || afterRevision.leases != beforeRevision.leases || afterRevision.attempts != beforeRevision.attempts || !afterRevision.activity.Equal(now) {
				t.Fatalf("new item revision did not fence refusal dedup: before=%+v after=%+v", beforeRevision, afterRevision)
			}
			assertDuplicate()
		})
	}
}

type nativeClaimWriteCounter struct {
	*postgres.Repository
	writes int
}

func (r *nativeClaimWriteCounter) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.writes++
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

func (r *nativeClaimWriteCounter) WriteRunnerCommand(ctx context.Context, org tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	r.writes++
	return r.Repository.WriteRunnerCommand(ctx, org, credential, runner, write)
}
