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
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
	f := newNativeFixture(t, openTestService(t, config), "", "claims")
	issue := f.create(t, "work")
	blocker := f.create(t, "blocker")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "block"}, ExpectedRevision: 1, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
	worker := f.worker(t, "worker")
	otherWorker := f.worker(t, "other-worker")
	machine := map[string]any{"id": "native-machine", "hostname": "runner", "display_name": "Runner", "version": "test", "capacity": 1}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, machine), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", otherWorker, machine), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, machine), http.StatusNotFound)
	request := tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: "native-machine", SessionID: "native-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
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
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "run-start"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: newNativeID("policy")}}
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
	f.service = openTestService(t, config)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, worker, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	now = now.Add(91 * time.Second)
	request.SessionID = "replacement-session"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	var replacement tracker.NativeLease
	decodeHubResponse(t, response, &replacement)
	if replacement.FencingToken <= lease.FencingToken {
		t.Fatal("reclaim did not advance fencing")
	}
	event.IdempotencyKey = "stale-new-event"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusConflict)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &history)
	if len(history.Items) != 3 {
		t.Fatalf("history after restart = %#v", history.Items)
	}
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
