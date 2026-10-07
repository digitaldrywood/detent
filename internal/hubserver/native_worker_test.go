package hubserver

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f nativeFixture) worker(t *testing.T, name string) string {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": name, "scope": "worker"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	response = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID})
	requireNativeStatus(t, response, http.StatusNoContent)
	return token.Token
}

func TestNativeClaimsEventsAndRestartWithoutGitHub(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }, Version: "v1.2.3"}
	f := newNativeFixture(t, openTestService(t, config), "", "claims")
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	issue := f.create(t, "work")
	blocker := f.create(t, "blocker")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "block"}, ExpectedRevision: 1, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
	worker := f.worker(t, "worker")
	otherWorker := f.worker(t, "other-worker")
	machine := map[string]any{"id": "native-machine", "hostname": "runner", "display_name": "Runner", "version": "v1.2.3", "capacity": 1}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, machine), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", otherWorker, machine), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, machine), http.StatusNotFound)
	request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: "native-machine", SessionID: "native-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request), http.StatusConflict)
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
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "run-start"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: descriptor.ID}}
	f.service.config.Version = "v1.2.4"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	newClaim := request
	newClaim.SessionID = "new-session"
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
	config.Version = "v1.2.4"
	f.service = openTestService(t, config)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, worker, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	now = now.Add(91 * time.Second)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/native-machine/heartbeat", worker, map[string]any{"version": "v1.2.4", "capacity": 1}), http.StatusNoContent)
	for _, test := range []struct {
		name       string
		credential string
		fence      tracker.FencingToken
		status     int
	}{
		{name: "another owner cannot renew expired lease", credential: otherWorker, fence: lease.FencingToken, status: http.StatusNotFound},
		{name: "wrong token cannot renew expired lease", credential: worker, fence: lease.FencingToken + 1, status: http.StatusConflict},
		{name: "original owner resumes expired lease", credential: worker, fence: lease.FencingToken, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", test.credential, tracker.NativeLeaseMutation{FencingToken: test.fence, TTLSeconds: 90})
			requireNativeStatus(t, response, test.status)
			if test.status == http.StatusOK {
				var renewed tracker.NativeLease
				decodeHubResponse(t, response, &renewed)
				if renewed.ID != lease.ID || renewed.FencingToken != lease.FencingToken || renewed.SessionID != lease.SessionID || !renewed.ExpiresAt.After(now) {
					t.Fatalf("reconnect replaced original authority: %+v", renewed)
				}
			}
		})
	}
	now = now.Add(91 * time.Second)
	request.SessionID = "replacement-session"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	var replacement tracker.NativeLease
	decodeHubResponse(t, response, &replacement)
	if replacement.FencingToken <= lease.FencingToken {
		t.Fatal("reclaim did not advance fencing")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
	event.IdempotencyKey = "stale-new-event"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusConflict)
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
	f.service.config.Version = "v1.2.5"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(replacement.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: replacement.FencingToken, Reason: "completed"}), http.StatusNoContent)
	var aliases, outbox int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE github_node_id IS NOT NULL").Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox").Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if aliases != 0 || outbox != 0 {
		t.Fatalf("native work acquired GitHub identities or outbox: %d %d", aliases, outbox)
	}
	for _, table := range []string{"collaboration_events", "collaboration_versions"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM "+table); err == nil {
			t.Errorf("%s allowed ordinary deletion", table)
		}
	}
}

func TestNativeRunnerMinimumVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, hub, runner, minimum string
		refused                    bool
	}{
		{name: "below", hub: "v1.2.4", runner: "v1.2.3", minimum: "v1.2.4", refused: true},
		{name: "equal", hub: "v1.2.4", runner: "v1.2.4", minimum: "v1.2.4"},
		{name: "above", hub: "v1.2.4", runner: "v1.3.0", minimum: "v1.2.4"},
		{name: "prerelease", hub: "v1.2.4", runner: "v1.2.4-rc.1", minimum: "v1.2.4", refused: true},
		{name: "build metadata", hub: "v1.2.4", runner: "v1.2.4+local", minimum: "v1.2.4"},
		{name: "unprefixed", hub: " v1.2.4 ", runner: "1.2.3", minimum: "v1.2.4", refused: true},
		{name: "development runner", hub: "v1.2.4", runner: "dev", minimum: "v1.2.4"},
		{name: "development hub", hub: "dev", runner: "v1.0.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Version: test.hub}), "", "minimum")
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			capabilities := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/capabilities", worker, nil)
			requireNativeStatus(t, capabilities, http.StatusOK)
			var published nativeCapabilitiesResponse
			decodeHubResponse(t, capabilities, &published)
			if published.MinimumRunnerVersion != test.minimum {
				t.Fatalf("published minimum = %q, want %q", published.MinimumRunnerVersion, test.minimum)
			}
			machine := map[string]any{"id": "native-machine", "hostname": "runner", "version": test.runner, "capacity": 1}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, machine), http.StatusOK)
			request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: "native-machine", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
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
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("refused runner acquired %d leases", count)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/native-machine/heartbeat", worker, map[string]any{"version": test.minimum, "capacity": 1}), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request), http.StatusOK)
			}
		})
	}
}
