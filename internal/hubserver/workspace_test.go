package hubserver

import (
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Workspace sessions end to end (decisions section 18.1): the request and its
// limits, the dispatch item the hub creates for it, the worker tuple that
// fences it, and the deadlines that make a workspace nobody is using stop
// costing a runner slot.

// workspaceTestCapabilities is what a runner in these tests reports it can
// serve. It deliberately excludes the terminal: section 18.3 leaves that off
// until an owner turns it on, so the default fixture is the default product.
var workspaceTestCapabilities = workspacesession.Capabilities{Files: true, Diff: true}

// workspaceFixture is a token-authenticated hub with workspace sessions
// enabled, one ordinary issue to open them on, and a second operator in the
// same project. The second actor exists because the per-person and the
// per-organization cap differ only in who asked, and a test that cannot ask
// as somebody else cannot tell them apart.
type workspaceFixture struct {
	nativeFixture
	ownerID string
	other   string
	otherID string
	policy  string
	issue   tracker.NativeIssue
}

func newWorkspaceFixture(t *testing.T, cfg *WorkspaceConfig) workspaceFixture {
	t.Helper()
	if cfg == nil {
		cfg = &WorkspaceConfig{Enabled: true}
	}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Workspace: cfg})
	f := newNativeFixture(t, service, "", "workspace")
	var ownerID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = ?", "operator-workspace").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", testHubAdminToken,
		map[string]any{"name": "operator-workspace-other", "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken,
		map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID}), http.StatusNoContent)
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, service, f.base+"/policy", descriptor)
	return workspaceFixture{nativeFixture: f, ownerID: ownerID, other: token.Token, otherID: token.ID,
		policy: descriptor.ID, issue: f.create(t, "workspace-subject")}
}

// post sends one POST /workspaces. Every call gets its own idempotency key
// unless the test names one: the key is scoped to the actor and the route, so
// a shared one would replay the first workspace instead of opening a second.
func (f workspaceFixture) post(t *testing.T, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	filled := map[string]any{"idempotency_key": newNativeID("wsk")}
	maps.Copy(filled, body)
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces", token, filled)
}

func (f workspaceFixture) opened(t *testing.T, token string, body map[string]any) workspacesession.Session {
	t.Helper()
	response := f.post(t, token, body)
	requireNativeStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	return session
}

func (f workspaceFixture) get(t *testing.T, token, id string) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+id, token, nil)
}

func (f workspaceFixture) list(t *testing.T, query string) workspaceListResponse {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces"+query, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var listing workspaceListResponse
	decodeHubResponse(t, response, &listing)
	return listing
}

// projectEvents reads the typed project log directly rather than through the
// stream: the point of the assertion is that the transition and its event
// committed together, and a subscriber would only prove that they arrived.
func (f workspaceFixture) projectEvents(t *testing.T, subject string) []projectEvent {
	t.Helper()
	rows, err := f.service.database.db.QueryContext(t.Context(),
		`SELECT seq, type, subject_id, data_json FROM project_events
WHERE organization_id = ? AND project_id = ? AND subject_id = ? ORDER BY seq`,
		f.project.OrganizationID, f.project.ID, subject)
	if err != nil {
		t.Fatalf("read project events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	events := []projectEvent{}
	for rows.Next() {
		var event projectEvent
		var data string
		if err := rows.Scan(&event.Seq, &event.Type, &event.SubjectID, &data); err != nil {
			t.Fatalf("scan project event: %v", err)
		}
		event.Data = json.RawMessage(data)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate project events: %v", err)
	}
	return events
}

func (f workspaceFixture) eventTypes(t *testing.T, subject string) []string {
	t.Helper()
	types := []string{}
	for _, event := range f.projectEvents(t, subject) {
		types = append(types, event.Type)
	}
	return types
}

// item resolves the dispatch issue a workspace was given, which is the handle
// the runner is actually offered.
func (f workspaceFixture) item(t *testing.T, workspaceID string) string {
	t.Helper()
	var item string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT work_item_id FROM workspace_items WHERE workspace_id = ?", workspaceID).Scan(&item); err != nil {
		t.Fatalf("read workspace item for %s: %v", workspaceID, err)
	}
	return item
}

func (f workspaceFixture) itemClosed(t *testing.T, workspaceID string) bool {
	t.Helper()
	var closed sql.NullString
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT closed_at FROM workspace_items WHERE workspace_id = ?", workspaceID).Scan(&closed); err != nil {
		t.Fatalf("read workspace item closure for %s: %v", workspaceID, err)
	}
	return closed.Valid
}

// occupancy reports the workspace's occupancy rows as (started, ended) pairs.
// The usage report adds runner seconds from this table and never from relay
// traffic, so a missing or never-ended row is a billing bug, not a detail.
func (f workspaceFixture) occupancy(t *testing.T, workspaceID string) (rows int, open int) {
	t.Helper()
	result, err := f.service.database.db.QueryContext(t.Context(),
		"SELECT ended_at FROM workspace_occupancy WHERE workspace_id = ?", workspaceID)
	if err != nil {
		t.Fatalf("read workspace occupancy: %v", err)
	}
	defer func() { _ = result.Close() }()
	for result.Next() {
		var ended sql.NullString
		if err := result.Scan(&ended); err != nil {
			t.Fatalf("scan workspace occupancy: %v", err)
		}
		rows++
		if !ended.Valid {
			open++
		}
	}
	if err := result.Err(); err != nil {
		t.Fatalf("iterate workspace occupancy: %v", err)
	}
	return rows, open
}

// attempt claims an issue of its own with an ordinary worker token and starts
// a run, because an attempt exists only as the record of one. Each call gets
// its own subject so the leases it leaves behind cannot collide. running
// leaves the run in flight, which is what makes a workspace on it read-only.
func (f workspaceFixture) attempt(t *testing.T, name string, running bool) string {
	t.Helper()
	subject := f.create(t, "attempt-subject-"+name)
	worker := f.worker(t, "workspace-"+name+"-worker")
	machine := "machine-" + name
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker,
		map[string]any{"id": machine, "hostname": "fixture", "display_name": "Fixture", "version": "test", "capacity": 4}), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: subject.WorkItemID, MachineID: tracker.MachineID(machine),
			SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2,
			Capabilities: []string{"native_issues", "scoped_collaboration"}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	attempt, run := newNativeID("attempt"), newNativeID("run")
	identity := &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}
	path := f.base + "/work-items/" + string(subject.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: identity, LeaseID: lease.ID, FencingToken: lease.FencingToken,
			RunID: run, AttemptID: attempt, PolicyID: f.policy},
	}), http.StatusOK)
	if running {
		return attempt
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 2, Identity: identity, LeaseID: lease.ID, FencingToken: lease.FencingToken,
			RunID: run, AttemptID: attempt, PolicyID: f.policy, Outcome: "succeeded"},
	}), http.StatusOK)
	return attempt
}

// A workspace request carries the default surfaces, records the resource on
// the project stream in the same commit, and creates the dispatch item that a
// runner -- and nothing else -- is meant to see.
func TestWorkspaceRequestCreatesSessionAndDispatchItem(t *testing.T) {
	t.Parallel()
	f := newWorkspaceFixture(t, nil)

	t.Run("a request without requires opens the two read-only surfaces and emits one workspace.requested", func(t *testing.T) {
		session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
		if session.State != workspacesession.StateRequested || session.ReadOnly || session.Revision != 1 {
			t.Fatalf("session = %#v", session)
		}
		if len(session.Requires) != 2 || session.Requires[0] != workspacesession.CapabilityFiles || session.Requires[1] != workspacesession.CapabilityDiff {
			t.Fatalf("requires = %v, want the section 18.1 default", session.Requires)
		}
		if session.CreatedBy != f.ownerID || session.IdleTimeoutSeconds != int(defaultWorkspaceIdleTimeout/time.Second) {
			t.Fatalf("session provenance = %#v", session)
		}
		events := f.projectEvents(t, session.ID)
		if len(events) != 1 || events[0].Type != "workspace.requested" {
			t.Fatalf("events = %#v", events)
		}
		var carried workspacesession.Session
		if err := json.Unmarshal(events[0].Data, &carried); err != nil {
			t.Fatalf("decode event data: %v", err)
		}
		// The stream carries the resource, not a ping: section 18.1 says a
		// client observes readiness by subscription and never by polling.
		if carried.ID != session.ID || carried.State != session.State || carried.WorkItemID != session.WorkItemID {
			t.Fatalf("event data = %#v", carried)
		}
	})

	t.Run("the dispatch item is a detent:workspace issue the board does not show", func(t *testing.T) {
		session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
		item := f.item(t, session.ID)
		if item == string(f.issue.WorkItemID) {
			t.Fatal("the workspace dispatched its own subject instead of a workspace item")
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=100", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.Page[nativeIssueListItem]
		decodeHubResponse(t, response, &page)
		if listedWorkItem(page, item) {
			t.Fatal("a workspace item appeared as project work")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=100&include=workspace", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		page = tracker.Page[nativeIssueListItem]{}
		decodeHubResponse(t, response, &page)
		if !listedWorkItem(page, item) {
			t.Fatal("include=workspace did not surface the workspace item")
		}
	})

	t.Run("the reserved detent:workspace label cannot be set by an API caller", func(t *testing.T) {
		// The label selects the runner's hold-open mode, so a tracker writer
		// who could set it would park a runner on any issue indefinitely.
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token,
			tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("label")}, Title: "Smuggled",
				State: "Todo", Labels: []string{workspaceItemLabel}})
		requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
}

// listedWorkItem reports whether a work item page names an item.
func listedWorkItem(page tracker.Page[nativeIssueListItem], item string) bool {
	for _, listed := range page.Items {
		if string(listed.WorkItemID) == item {
			return true
		}
	}
	return false
}

// A request the hub cannot honour is refused before anything is created, and
// the refusal names what to do next rather than only that it failed.
func TestWorkspaceRequestRefusals(t *testing.T) {
	t.Parallel()
	f := newWorkspaceFixture(t, nil)

	t.Run("an unknown capability in requires is refused rather than silently dropped", func(t *testing.T) {
		response := f.post(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID), "requires": []string{"telnet"}})
		requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("a body naming neither a work item nor an attempt is refused", func(t *testing.T) {
		requireNativeCode(t, f.post(t, f.token, map[string]any{}), http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("a second workspace on one attempt names the one already open and is reopenable after it closes", func(t *testing.T) {
		attempt := f.attempt(t, "exists", false)
		first := f.opened(t, f.token, map[string]any{"attempt_id": attempt})
		response := f.post(t, f.token, map[string]any{"attempt_id": attempt})
		requireNativeCode(t, response, http.StatusConflict, "workspace_exists")
		var failure nativeError
		decodeHubResponse(t, response, &failure)
		// The client's next move is to use the workspace it already has, so
		// the refusal has to name it.
		if failure.Details["workspace_id"] != first.ID {
			t.Fatalf("details = %#v, want workspace_id %s", failure.Details, first.ID)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/workspaces/"+first.ID, f.token, nil), http.StatusNoContent)
		second := f.opened(t, f.token, map[string]any{"attempt_id": attempt})
		if second.ID == first.ID || second.AttemptID != attempt {
			t.Fatalf("reopened workspace = %#v", second)
		}
	})

	t.Run("a terminal is refused while the subject attempt is still running", func(t *testing.T) {
		attempt := f.attempt(t, "running", true)
		session := f.opened(t, f.token, map[string]any{"attempt_id": attempt})
		// A person may look at the worktree the model is editing; typing into
		// it is what the read-only flag and the refusal below prevent.
		if !session.ReadOnly {
			t.Fatalf("workspace on a running attempt = %#v, want read_only", session)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/workspaces/"+session.ID, f.token, nil), http.StatusNoContent)
		response := f.post(t, f.token, map[string]any{"attempt_id": attempt, "requires": []string{workspacesession.CapabilityTerminal}})
		requireNativeCode(t, response, http.StatusForbidden, "forbidden")
	})

	t.Run("a terminal is refused when the organization has terminals turned off", func(t *testing.T) {
		response := f.post(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID),
			"requires": []string{workspacesession.CapabilityTerminal}})
		requireNativeCode(t, response, http.StatusForbidden, "forbidden")
	})
}

// The two caps of section 18.1 are checked together and say which one was
// reached, because "you have too many" is only actionable if it names whose.
func TestWorkspaceOpenLimitsNameTheScopeReached(t *testing.T) {
	t.Parallel()
	f := newWorkspaceFixture(t, &WorkspaceConfig{Enabled: true, PersonMaxOpen: 2, PlanMaxOpen: 3})
	subject := map[string]any{"work_item_id": string(f.issue.WorkItemID)}
	for range 2 {
		f.opened(t, f.token, subject)
	}

	t.Run("a person past their own cap is refused with scope person", func(t *testing.T) {
		requireWorkspaceLimit(t, f.post(t, f.token, subject), "person", 2)
	})

	t.Run("an organization past its plan cap is refused with scope organization", func(t *testing.T) {
		// The third open workspace belongs to someone else, so the actor's own
		// cap is not what stops the fourth request.
		f.opened(t, f.other, subject)
		requireWorkspaceLimit(t, f.post(t, f.other, subject), "organization", 3)
	})
}

func requireWorkspaceLimit(t *testing.T, response *httptest.ResponseRecorder, scope string, limit int) {
	t.Helper()
	requireNativeCode(t, response, http.StatusUnprocessableEntity, "workspace_limit")
	var failure nativeError
	decodeHubResponse(t, response, &failure)
	if failure.Details["scope"] != scope {
		t.Fatalf("details = %#v, want scope %s", failure.Details, scope)
	}
	if value, ok := failure.Details["limit"].(float64); !ok || int(value) != limit {
		t.Fatalf("details = %#v, want limit %d", failure.Details, limit)
	}
}

// One idempotency key is one workspace: a retry of a request whose answer was
// lost must not open a second runner slot, and a key reused for something else
// must not hand back the first one.
func TestWorkspaceRequestIsIdempotentPerKey(t *testing.T) {
	t.Parallel()
	f := newWorkspaceFixture(t, nil)
	body := map[string]any{"idempotency_key": "open-once", "work_item_id": string(f.issue.WorkItemID), "ref": "main"}

	first := f.post(t, f.token, body)
	requireNativeStatus(t, first, http.StatusCreated)
	var opened workspacesession.Session
	decodeHubResponse(t, first, &opened)

	t.Run("the same key with the same body replays the committed workspace", func(t *testing.T) {
		retry := f.post(t, f.token, body)
		requireNativeStatus(t, retry, http.StatusCreated)
		var replayed workspacesession.Session
		decodeHubResponse(t, retry, &replayed)
		if replayed.ID != opened.ID || retry.Body.String() != first.Body.String() {
			t.Fatalf("replay = %#v, want %s byte for byte", replayed, opened.ID)
		}
		if listing := f.list(t, ""); len(listing.Workspaces) != 1 {
			t.Fatalf("a replay opened a second workspace: %#v", listing.Workspaces)
		}
	})

	t.Run("the same key with a different body is a conflict", func(t *testing.T) {
		changed := maps.Clone(body)
		changed["ref"] = "release"
		requireNativeCode(t, f.post(t, f.token, changed), http.StatusConflict, "idempotency_conflict")
	})
}

// Reading follows the issue's read rule, and an identifier that names nothing
// this reader may see is not found rather than described.
func TestWorkspaceReadsFilterAndScope(t *testing.T) {
	t.Parallel()
	f := newWorkspaceFixture(t, nil)
	other := f.create(t, "second-subject")
	first := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	second := f.opened(t, f.token, map[string]any{"work_item_id": string(other.WorkItemID)})

	t.Run("listing filters by work item and by state", func(t *testing.T) {
		if listing := f.list(t, "?work_item="+string(other.WorkItemID)); len(listing.Workspaces) != 1 || listing.Workspaces[0].ID != second.ID {
			t.Fatalf("work_item filter = %#v", listing.Workspaces)
		}
		if listing := f.list(t, "?state=requested"); len(listing.Workspaces) != 2 {
			t.Fatalf("state filter = %#v", listing.Workspaces)
		}
		if listing := f.list(t, "?state=closed"); len(listing.Workspaces) != 0 {
			t.Fatalf("closed listing = %#v", listing.Workspaces)
		}
		requireNativeStatus(t, f.get(t, f.token, first.ID), http.StatusOK)
	})

	t.Run("an unknown state is refused rather than matching nothing", func(t *testing.T) {
		// A typo that quietly returns an empty list reads as "no workspaces",
		// which is the one answer the client must not be given.
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces?state=paused", f.token, nil)
		requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("a malformed id and another project's workspace are both not found", func(t *testing.T) {
		elsewhere := newNativeFixture(t, f.service, f.project.OrganizationID, "workspace-elsewhere")
		foreign := workspaceFixture{nativeFixture: elsewhere}
		foreignIssue := elsewhere.create(t, "foreign-subject")
		session := foreign.opened(t, elsewhere.token, map[string]any{"work_item_id": string(foreignIssue.WorkItemID)})
		for _, test := range []struct{ name, id string }{
			{"not a workspace identifier", "workspace-1"},
			{"right shape, wrong length", "ws_abcdef"},
			{"another project's workspace", session.ID},
		} {
			t.Run(test.name, func(t *testing.T) {
				requireNativeStatus(t, f.get(t, f.token, test.id), http.StatusNotFound)
			})
		}
	})
}

// workspaceRunnerFixture adds an enrolled runner that reports the read-only
// surfaces, which is the only kind of runner a workspace item is ever offered
// to (section 18.1's claim gate).
type workspaceRunnerFixture struct {
	workspaceFixture
	runner runnerFixture
}

func newWorkspaceRunnerFixture(t *testing.T) *workspaceRunnerFixture {
	t.Helper()
	// The caps are lifted well clear of the subtests: these tests are about
	// the owner tuple, and a refusal from section 18.1's limits would only
	// hide it.
	f := newWorkspaceFixture(t, &WorkspaceConfig{Enabled: true, PersonMaxOpen: 50, PlanMaxOpen: 50})
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	// The capacity is generous so that one fixture can hold several
	// workspaces at once; the claim gate, not the host, is what these tests
	// are about.
	r.redemption.Capacity = 8
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", r.redemption.Credential,
		map[string]any{"id": r.binding.MachineID, "hostname": "customer-host", "display_name": "Runner",
			"capacity": 8, "version": "test", "workspace_capabilities": workspaceTestCapabilities,
			"workspace_isolation": workspacesession.IsolationContainer}), http.StatusOK)
	return &workspaceRunnerFixture{workspaceFixture: f, runner: r}
}

// claim takes the lease on one work item. A worker request presents that
// lease, so a test that wants to prove the fence has to hold a real one.
func (f *workspaceRunnerFixture) claim(t *testing.T, item string) tracker.NativeLease {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
			SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2,
			// The workspace lane declares the workspace capability; without it
			// the hub's candidate query excludes workspace items entirely
			// (decisions section 18.1).
			Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	return lease
}

func workspaceIdentity(lease tracker.NativeLease) workspaceWorkerIdentity {
	return workspaceWorkerIdentity{LeaseID: lease.ID, FencingToken: lease.FencingToken}
}

func (f *workspaceRunnerFixture) workerPost(t *testing.T, id, endpoint string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces/"+id+"/worker/"+endpoint,
		f.runner.redemption.Credential, body)
}

// requested opens a workspace and claims its dispatch item, leaving it exactly
// where a runner that has just been handed the job stands.
func (f *workspaceRunnerFixture) requested(t *testing.T) (workspacesession.Session, tracker.NativeLease) {
	t.Helper()
	session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	return session, f.claim(t, f.item(t, session.ID))
}

// bound takes that one step further and binds, which is the shortest path to
// every state after requested.
func (f *workspaceRunnerFixture) bound(t *testing.T) (workspaceBindResponse, tracker.NativeLease) {
	t.Helper()
	session, lease := f.requested(t)
	response := f.workerPost(t, session.ID, "bind", workspaceBindRequest{
		workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities,
		Isolation: workspacesession.IsolationContainer})
	requireNativeStatus(t, response, http.StatusOK)
	var bind workspaceBindResponse
	decodeHubResponse(t, response, &bind)
	return bind, lease
}

// Binding is the only way a runner takes a workspace, and it is fenced by this
// workspace's own generation: there is no state in which two runners hold it.
func TestWorkspaceWorkerBind(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)

	t.Run("a bind moves requested to starting and returns the tuple and the checkout", func(t *testing.T) {
		bind, lease := f.bound(t)
		if bind.Session.State != workspacesession.StateStarting {
			t.Fatalf("state = %q, want starting", bind.Session.State)
		}
		want := workspacesession.Owner{WorkspaceID: bind.Session.ID, RunnerID: f.runner.binding.RunnerID,
			MachineID: string(f.runner.binding.MachineID), LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}
		if bind.Owner != want {
			t.Fatalf("owner = %#v, want %#v", bind.Owner, want)
		}
		checkout := bind.Checkout
		// The workspace names no attempt, so there is no worktree to retain
		// and the runner is told to check out fresh.
		if checkout.WorkItemID != string(f.issue.WorkItemID) || checkout.Worktree != workspacesession.WorktreeFresh || checkout.ReadOnly {
			t.Fatalf("checkout = %#v", checkout)
		}
		if checkout.HeartbeatSeconds != int(workspacesession.HeartbeatInterval/time.Second) ||
			checkout.IdleTimeoutSeconds != int(defaultWorkspaceIdleTimeout/time.Second) ||
			!checkout.ExpiresAt.Equal(bind.Session.ExpiresAt) {
			t.Fatalf("checkout deadlines = %#v", checkout)
		}
		if rows, open := f.occupancy(t, bind.Session.ID); rows != 1 || open != 1 {
			t.Fatalf("occupancy rows = %d, open = %d, want one open row", rows, open)
		}
		if types := f.eventTypes(t, bind.Session.ID); len(types) != 2 || types[1] != "workspace.starting" {
			t.Fatalf("events = %v", types)
		}
	})

	t.Run("a lease held on a different work item cannot bind this workspace", func(t *testing.T) {
		session, _ := f.requested(t)
		// Without this check any runner holding any lease in the project
		// could bind any workspace.
		elsewhere := f.claim(t, string(f.issue.WorkItemID))
		response := f.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(elsewhere), Capabilities: workspaceTestCapabilities})
		requireNativeCode(t, response, http.StatusConflict, "stale_execution")
	})

	t.Run("a stale fencing token cannot bind", func(t *testing.T) {
		session, lease := f.requested(t)
		stale := workspaceIdentity(lease)
		stale.FencingToken++
		response := f.workerPost(t, session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: stale, Capabilities: workspaceTestCapabilities})
		requireNativeCode(t, response, http.StatusConflict, "stale_execution")
	})

	t.Run("a second bind on a workspace that is already starting is refused", func(t *testing.T) {
		bind, lease := f.bound(t)
		response := f.workerPost(t, bind.Session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities})
		requireNativeCode(t, response, http.StatusConflict, "stale_execution")
	})
}

// The heartbeat renews the lease and carries the runner's report. It is the
// one call that must not fail on disagreement: a heartbeat that errored would
// also stop renewing, and the workspace would die of the argument.
func TestWorkspaceWorkerHeartbeat(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)
	head := "0123456789abcdef0123456789abcdef01234567"

	t.Run("a ready report opens the workspace and records what the runner serves", func(t *testing.T) {
		bind, lease := f.bound(t)
		response := f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady, HeadSHA: head,
			Capabilities: workspaceTestCapabilities, Isolation: workspacesession.IsolationContainer,
			Worktree: workspacesession.WorktreeFresh})
		requireNativeStatus(t, response, http.StatusOK)
		var beat workspaceHeartbeatResponse
		decodeHubResponse(t, response, &beat)
		if beat.Session.State != workspacesession.StateReady || beat.Session.HeadSHA != head {
			t.Fatalf("session = %#v", beat.Session)
		}
		if beat.Session.Capabilities == nil || *beat.Session.Capabilities != workspaceTestCapabilities ||
			beat.Session.Isolation != workspacesession.IsolationContainer || beat.Session.Worktree != workspacesession.WorktreeFresh {
			t.Fatalf("report = %#v", beat.Session)
		}
		// opened_at is when the surfaces became usable, which is the moment
		// the idle clock starts from.
		if beat.Session.OpenedAt == nil {
			t.Fatal("a ready workspace has no opened_at")
		}
		if types := f.eventTypes(t, bind.Session.ID); len(types) != 3 || types[2] != "workspace.ready" {
			t.Fatalf("events = %v", types)
		}
	})

	t.Run("an illegal reported state answers the stored one instead of failing", func(t *testing.T) {
		bind, lease := f.bound(t)
		ready := workspaceHeartbeatRequest{workspaceWorkerIdentity: workspaceIdentity(lease),
			State: workspacesession.StateReady, Capabilities: workspaceTestCapabilities}
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "heartbeat", ready), http.StatusOK)
		confused := ready
		confused.State = workspacesession.StateStarting
		response := f.workerPost(t, bind.Session.ID, "heartbeat", confused)
		requireNativeStatus(t, response, http.StatusOK)
		var beat workspaceHeartbeatResponse
		decodeHubResponse(t, response, &beat)
		if beat.Session.State != workspacesession.StateReady {
			t.Fatalf("state = %q, want the stored ready so the runner converges", beat.Session.State)
		}
	})

	t.Run("a worktree_missing report fails the workspace whatever state it claims", func(t *testing.T) {
		bind, lease := f.bound(t)
		// The worktree is what the surfaces are, so there is nothing left to
		// be ready about.
		response := f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady,
			Reason: workspacesession.ReasonWorktreeMissing, Capabilities: workspaceTestCapabilities})
		requireNativeStatus(t, response, http.StatusOK)
		var beat workspaceHeartbeatResponse
		decodeHubResponse(t, response, &beat)
		if beat.Session.State != workspacesession.StateFailed || beat.Session.Reason != workspacesession.ReasonWorktreeMissing {
			t.Fatalf("session = %#v, want failed with worktree_missing", beat.Session)
		}
		if !f.itemClosed(t, bind.Session.ID) {
			t.Fatal("a failed workspace left its dispatch item open")
		}
	})
}

// leaseWindow reads what the hub currently believes about one lease: when it
// expires and whether it has been released. Both are what the relay's per-frame
// authority check reads, so a test that wants to prove a workspace outlives its
// first lease TTL has to look here and not at workspace_sessions.
func (f *workspaceRunnerFixture) leaseWindow(t *testing.T, id tracker.LeaseID) (time.Time, bool) {
	t.Helper()
	var expires string
	var released sql.NullString
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT expires_at, released_at FROM leases WHERE lease_id = ?", id).Scan(&expires, &released); err != nil {
		t.Fatal(err)
	}
	expiresAt, err := parseTimeValue(expires)
	if err != nil {
		t.Fatal(err)
	}
	return expiresAt, released.Valid
}

// setLeaseExpiry moves a lease's expiry to a known moment, so a renewal is
// observable whatever the clock's granularity and whatever TTL the claim asked
// for.
func (f *workspaceRunnerFixture) setLeaseExpiry(t *testing.T, id tracker.LeaseID, at time.Time) {
	t.Helper()
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE leases SET expires_at = ? WHERE lease_id = ?", formatHubTime(at), id); err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceHeartbeatRenewsTheLease is the September 12 fifth dogfood
// defect: nothing on the workspace path renewed the workspace lease, so a Files
// session lived exactly one lease_ttl_seconds from the claim and then answered
// the person revoked. Section 18.1 puts the renewal on the heartbeat itself --
// "POST .../worker/heartbeat (renews the lease ...)" -- and it happens in the
// same transaction as the session row, so the runner needs no second call.
func TestWorkspaceHeartbeatRenewsTheLease(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)

	t.Run("a heartbeat moves the lease expiry a full lease TTL ahead", func(t *testing.T) {
		bind, lease := f.bound(t)
		// A lease about to run out is the case the defect was about: before
		// the fix the heartbeat wrote workspace_sessions and left this alone.
		about := f.service.config.now().UTC().Add(2 * time.Second)
		f.setLeaseExpiry(t, lease.ID, about)
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady,
			Capabilities: workspaceTestCapabilities}), http.StatusOK)
		renewed, released := f.leaseWindow(t, lease.ID)
		if released {
			t.Fatal("an open workspace's lease was released by its own heartbeat")
		}
		if !renewed.After(about) {
			t.Fatalf("lease expiry = %s, want it moved past %s", renewed, about)
		}
		// The window is the workspace's own TTL, because that is what the
		// workspace machine measures lease_lost from.
		if ahead := renewed.Sub(f.service.config.now().UTC()); ahead > workspacesession.LeaseTTL || ahead < workspacesession.LeaseTTL-10*time.Second {
			t.Fatalf("lease renewed %s ahead, want about %s", ahead, workspacesession.LeaseTTL)
		}
	})

	t.Run("a second heartbeat keeps a workspace alive past its first lease TTL", func(t *testing.T) {
		bind, lease := f.bound(t)
		first, _ := f.leaseWindow(t, lease.ID)
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady,
			Capabilities: workspaceTestCapabilities}), http.StatusOK)
		// The claim's own TTL is far longer than the workspace TTL in this
		// fixture, so the renewal is proved by the window being re-cut from
		// now rather than by it growing.
		renewed, _ := f.leaseWindow(t, lease.ID)
		if !renewed.Before(first) {
			t.Fatalf("lease expiry = %s, want it re-cut from now inside the claim's %s window", renewed, first)
		}
		f.setLeaseExpiry(t, lease.ID, f.service.config.now().UTC().Add(time.Second))
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities}), http.StatusOK)
		again, _ := f.leaseWindow(t, lease.ID)
		if !again.After(f.service.config.now().UTC().Add(workspacesession.LeaseTTL - 10*time.Second)) {
			t.Fatalf("lease expiry = %s, want a second renewal a lease TTL ahead", again)
		}
	})

	t.Run("a stale fencing token is refused and renews nothing", func(t *testing.T) {
		bind, lease := f.bound(t)
		before, _ := f.leaseWindow(t, lease.ID)
		stale := workspaceIdentity(lease)
		stale.FencingToken++
		requireNativeCode(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: stale, State: workspacesession.StateReady,
			Capabilities: workspaceTestCapabilities}), http.StatusConflict, "stale_execution")
		after, released := f.leaseWindow(t, lease.ID)
		if released || !after.Equal(before) {
			t.Fatalf("lease expiry = %s (released %v), want %s untouched", after, released, before)
		}
	})

	t.Run("an expired lease is refused rather than renewed back to life", func(t *testing.T) {
		bind, lease := f.bound(t)
		// A renewal that resurrected a lease the hub had already given up on
		// would put a runner back on a worktree the workspace machine has
		// already failed with lease_lost.
		f.setLeaseExpiry(t, lease.ID, f.service.config.now().UTC().Add(-time.Second))
		requireNativeCode(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady,
			Capabilities: workspaceTestCapabilities}), http.StatusConflict, "stale_execution")
		after, _ := f.leaseWindow(t, lease.ID)
		if after.After(f.service.config.now().UTC()) {
			t.Fatalf("lease expiry = %s, want the expired window left where it was", after)
		}
	})

	t.Run("a heartbeat that fails the workspace releases the lease instead of renewing it", func(t *testing.T) {
		bind, lease := f.bound(t)
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady,
			Reason: workspacesession.ReasonWorktreeMissing, Capabilities: workspaceTestCapabilities}), http.StatusOK)
		if session := f.read(t, bind.Session.ID); session.State != workspacesession.StateFailed {
			t.Fatalf("session = %#v, want failed", session)
		}
		if _, released := f.leaseWindow(t, lease.ID); !released {
			t.Fatal("a failed workspace kept a live claim on its runner's capacity")
		}
	})
}

// Unbinding releases the workspace, and the reason decides where it lands: a
// restarting runner fails it, a close that was already asked for ends it.
func TestWorkspaceWorkerUnbind(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)

	t.Run("a runner_restarted unbind fails the workspace", func(t *testing.T) {
		bind, lease := f.bound(t)
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "unbind", workspaceUnbindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Reason: workspacesession.ReasonRunnerRestarted}), http.StatusNoContent)
		// A failed workspace is never resumed; the person opens a new one.
		session := f.read(t, bind.Session.ID)
		if session.State != workspacesession.StateFailed || session.Reason != workspacesession.ReasonRunnerRestarted {
			t.Fatalf("session = %#v", session)
		}
		if _, open := f.occupancy(t, bind.Session.ID); open != 0 {
			t.Fatal("a failed workspace kept accruing runner time")
		}
	})

	t.Run("an unbind from closing ends the workspace closed", func(t *testing.T) {
		bind, lease := f.bound(t)
		// closing is the hub asking the runner to let go; it is reached by a
		// sweep or a delete, neither of which leaves it there long enough for
		// a test to observe, so the row is put there directly.
		if _, err := f.service.database.db.ExecContext(t.Context(),
			"UPDATE workspace_sessions SET state = 'closing', reason = ? WHERE id = ?",
			workspacesession.ReasonClosedByActor, bind.Session.ID); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "unbind", workspaceUnbindRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Reason: workspacesession.ReasonClosedByActor}), http.StatusNoContent)
		if session := f.read(t, bind.Session.ID); session.State != workspacesession.StateClosed {
			t.Fatalf("session = %#v, want closed", session)
		}
	})

	t.Run("a terminal workspace refuses every worker call that would revive it", func(t *testing.T) {
		bind, lease := f.bound(t)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete,
			f.base+"/workspaces/"+bind.Session.ID, f.token, nil), http.StatusNoContent)
		identity := workspaceIdentity(lease)
		requireNativeCode(t, f.workerPost(t, bind.Session.ID, "bind", workspaceBindRequest{
			workspaceWorkerIdentity: identity, Capabilities: workspaceTestCapabilities}), http.StatusConflict, "stale_execution")
		requireNativeCode(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: identity, Capabilities: workspaceTestCapabilities}), http.StatusConflict, "stale_execution")
		// unbind is the exception on purpose: releasing something already
		// released is the runner agreeing with the hub, so it answers 204
		// rather than sending the runner into a retry loop it cannot win.
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "unbind", workspaceUnbindRequest{
			workspaceWorkerIdentity: identity, Reason: workspacesession.ReasonRunnerRestarted}), http.StatusNoContent)
	})
}

func (f workspaceFixture) read(t *testing.T, id string) workspacesession.Session {
	t.Helper()
	response := f.get(t, f.token, id)
	requireNativeStatus(t, response, http.StatusOK)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	return session
}

// Deleting a workspace closes it and stops the accounting; it never touches
// the attempt's artifacts, and asking twice is not an error.
func TestWorkspaceDeleteClosesSessionAndAccounting(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)
	bind, _ := f.bound(t)
	path := f.base + "/workspaces/" + bind.Session.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, path, f.token, nil), http.StatusNoContent)

	t.Run("the workspace closes with closed_by_actor through closing", func(t *testing.T) {
		closed := f.read(t, bind.Session.ID)
		if closed.State != workspacesession.StateClosed || closed.Reason != workspacesession.ReasonClosedByActor {
			t.Fatalf("session = %#v", closed)
		}
		// closing then closed, not a jump: a runner learns it must let go
		// from the first and that the hub is done from the second.
		types := f.eventTypes(t, bind.Session.ID)
		if len(types) != 4 || types[2] != "workspace.closing" || types[3] != "workspace.closed" {
			t.Fatalf("events = %v", types)
		}
	})

	t.Run("the dispatch item closes so the claim gate stops offering it", func(t *testing.T) {
		if !f.itemClosed(t, bind.Session.ID) {
			t.Fatal("the workspace item is still open, so a runner would still be offered it")
		}
	})

	t.Run("the occupancy the bind opened is ended", func(t *testing.T) {
		// Nothing else closes the row, and the usage report bills runner
		// seconds from it.
		if rows, open := f.occupancy(t, bind.Session.ID); rows != 1 || open != 0 {
			t.Fatalf("occupancy rows = %d, open = %d, want one ended row", rows, open)
		}
	})

	t.Run("a second delete is still no content", func(t *testing.T) {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, path, f.token, nil), http.StatusNoContent)
	})
}

// TestWorkspaceRequestWithNoRunnerFailsWithTheReasonOnTheResource walks the
// no_runner deadline through the sweep and out to the reader.
//
// decideDeadline already has the timing as a table, but a person watching a
// Files panel does not read decideDeadline: they read the resource, and the
// surface turns its reason into the sentence that says why the wait ended. The
// value of this test is the trip from the deadline to that field -- a
// transition the sweep never applied, or applied without carrying the reason,
// would leave the panel waiting on a state that never changes, which is the
// defect this whole change is about.
func TestWorkspaceRequestWithNoRunnerFailsWithTheReasonOnTheResource(t *testing.T) {
	t.Parallel()
	// A request timeout short enough to expire between two lines, because no
	// runner will ever claim this one: the fixture enrolls none.
	f := newWorkspaceFixture(t, &WorkspaceConfig{Enabled: true, RequestTimeout: time.Nanosecond})
	opened := f.opened(t, f.token, map[string]any{
		"work_item_id": string(f.issue.WorkItemID),
		"requires":     []string{workspacesession.CapabilityFiles},
	})
	if opened.State != workspacesession.StateRequested {
		t.Fatalf("a new workspace = %#v, want requested", opened)
	}

	f.service.workspaces.sweep(t.Context())

	failed := f.read(t, opened.ID)
	if failed.State != workspacesession.StateFailed || failed.Reason != workspacesession.ReasonNoRunner {
		t.Fatalf("workspace after the request timeout = state %q reason %q, want failed no_runner", failed.State, failed.Reason)
	}
	if failed.RunnerID != "" {
		t.Fatalf("a workspace no runner claimed reports runner %q", failed.RunnerID)
	}
	// The surface subscribes rather than polls (section 18.1), so the
	// transition is only visible if it was published too.
	types := f.eventTypes(t, opened.ID)
	if len(types) == 0 || types[len(types)-1] != "workspace.failed" {
		t.Fatalf("events = %v, want workspace.failed last", types)
	}
}

// TestWorkspaceRequestAfterThePreviousOneClosedIsAccepted covers the second
// half of the same story: the pre-warmed workspace expires on its idle
// timeout, and the next Files click has to be able to open another one.
//
// Section 18.1 refuses a second workspace only while one is open on the same
// worktree -- 409 workspace_exists -- so a closed or closing predecessor must
// not be treated as that conflict. Getting this wrong would turn a panel that
// merely waits into one that cannot be opened again at all for the life of the
// hub, so it is worth pinning at both ends of the close.
func TestWorkspaceRequestAfterThePreviousOneClosedIsAccepted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// close drives the predecessor to the state under test and reports
		// what it reached.
		closeTo func(t *testing.T, f *workspaceRunnerFixture, id string) string
	}{
		{
			name: "while the predecessor is still closing",
			closeTo: func(t *testing.T, f *workspaceRunnerFixture, id string) string {
				t.Helper()
				// DELETE on a bound workspace asks the runner to let go, so it
				// sits in closing until the runner answers.
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/workspaces/"+id, f.token, nil), http.StatusNoContent)
				return f.read(t, id).State
			},
		},
		{
			// The preview's own case: the pre-warmed workspace reached its
			// idle timeout and the hub closed it as expired hours before the
			// next Files click.
			name: "once the predecessor has expired and closed",
			closeTo: func(t *testing.T, f *workspaceRunnerFixture, id string) string {
				t.Helper()
				if _, err := f.service.database.db.ExecContext(t.Context(),
					"UPDATE workspace_sessions SET state = 'closed', reason = ? WHERE id = ?",
					workspacesession.ReasonExpired, id); err != nil {
					t.Fatal(err)
				}
				return f.read(t, id).State
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newWorkspaceRunnerFixture(t)
			bind, _ := f.bound(t)
			reached := test.closeTo(t, f, bind.Session.ID)
			if reached != workspacesession.StateClosing && reached != workspacesession.StateClosed {
				t.Fatalf("the predecessor reached %q, want closing or closed", reached)
			}

			opened := f.opened(t, f.token, map[string]any{
				"work_item_id": string(f.issue.WorkItemID),
				"requires":     []string{workspacesession.CapabilityFiles},
			})
			if opened.ID == bind.Session.ID {
				t.Fatalf("the request replayed the %s workspace instead of opening a new one", reached)
			}
			if opened.State != workspacesession.StateRequested {
				t.Fatalf("the new workspace = %#v, want requested", opened)
			}
		})
	}
}

// Every deadline of section 18.1 that no request would ever reach on its own.
// decideDeadline is pure, so the whole timing contract is a table rather than
// a set of tests that sleep.
func TestWorkspaceDecideDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time { moment := now.Add(offset); return &moment }
	// base is a workspace with every deadline comfortably ahead of it, so each
	// case moves exactly the one field it is about.
	base := func(state string) workspaceRecord {
		return workspaceRecord{
			State: state, IdleTimeoutSeconds: 1800, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
			ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Hour),
		}
	}
	for _, test := range []struct {
		name   string
		record func() workspaceRecord
		want   workspaceDeadline
	}{
		{
			name: "a request no runner claimed in time fails with no_runner",
			record: func() workspaceRecord {
				record := base(workspacesession.StateRequested)
				record.RequestedExpiresAt = now.Add(-time.Second)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonNoRunner, endedAt: now, apply: true},
		},
		{
			name: "a request inside the re-bind grace waits for the runner that held it",
			record: func() workspaceRecord {
				record := base(workspacesession.StateRequested)
				record.RebindDeadline = at(10 * time.Second)
				return record
			},
			want: workspaceDeadline{},
		},
		{
			name: "a request past the re-bind grace fails with hub_restarted",
			record: func() workspaceRecord {
				record := base(workspacesession.StateRequested)
				record.RebindDeadline = at(-time.Second)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonHubRestarted, endedAt: now, apply: true},
		},
		{
			name: "two missed heartbeats mark a ready workspace unreachable",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-(workspacesession.MissedHeartbeats*workspacesession.HeartbeatInterval + time.Second))
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateUnreachable, reason: workspacesession.ReasonRunnerRestarted, endedAt: now, apply: true},
		},
		{
			// The renewal the heartbeat now does must not move this line: a
			// workspace still inside the two-heartbeat window is answering,
			// and nothing is due.
			name: "a workspace one second inside the two-heartbeat window is left alone",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-(workspacesession.MissedHeartbeats*workspacesession.HeartbeatInterval - time.Second))
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: workspaceDeadline{},
		},
		{
			name: "the lease TTL itself is the lease_lost boundary",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				// Exactly at 90 seconds the lease is gone: the sweep reads
				// the same window the hub renews the lease row to, so the two
				// halves of section 18.1's timing cannot drift apart.
				record.LastHeartbeatAt = at(-workspacesession.LeaseTTL)
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonLeaseLost, endedAt: now, apply: true},
		},
		{
			name: "an expired lease fails the workspace and ends occupancy at expiry, not at discovery",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-(workspacesession.LeaseTTL + time.Minute))
				record.OpenedAt = at(-time.Minute)
				return record
			},
			// The crashed runner stopped serving when its lease ran out, so
			// that is the second it stops being charged for.
			want: workspaceDeadline{state: workspacesession.StateFailed, reason: workspacesession.ReasonLeaseLost,
				endedAt: now.Add(-(workspacesession.LeaseTTL + time.Minute)).Add(workspacesession.LeaseTTL), apply: true},
		},
		{
			name: "the hard lifetime cap closes a ready workspace as expired",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(0)
				record.OpenedAt = at(-time.Minute)
				record.ExpiresAt = now.Add(-time.Second)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true},
		},
		{
			name: "a ready workspace past its idle timeout closes as expired",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				record.IdleTimeoutSeconds = 60
				record.LastHeartbeatAt = at(0)
				record.OpenedAt = at(-time.Hour)
				record.LastActivityAt = at(-2 * time.Minute)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true},
		},
		{
			name: "a ready workspace past the idle mark only reports idle",
			record: func() workspaceRecord {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(0)
				record.OpenedAt = at(-time.Hour)
				// Runner output does not reset this: a busy shell left alone
				// is still idle.
				record.LastActivityAt = at(-(workspaceIdleMark + time.Second))
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateIdle, endedAt: now, apply: true},
		},
		{
			name: "an unreachable workspace closes after its idle timeout",
			record: func() workspaceRecord {
				record := base(workspacesession.StateUnreachable)
				record.IdleTimeoutSeconds = 60
				record.UpdatedAt = now.Add(-2 * time.Minute)
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonExpired, endedAt: now, apply: true},
		},
		{
			name: "a runner that never answers a close stops holding the slot at the lease TTL",
			record: func() workspaceRecord {
				record := base(workspacesession.StateClosing)
				record.Reason = workspacesession.ReasonClosedByActor
				record.UpdatedAt = now.Add(-(workspacesession.LeaseTTL + time.Second))
				return record
			},
			want: workspaceDeadline{state: workspacesession.StateClosed, reason: workspacesession.ReasonClosedByActor, endedAt: now, apply: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := decideDeadline(test.record(), now)
			if got.state != test.want.state || got.reason != test.want.reason || got.apply != test.want.apply || !got.endedAt.Equal(test.want.endedAt) {
				t.Fatalf("decideDeadline() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestWorkspaceItemsAreNotOrdinaryWork is the September 12 dogfood defect: the
// preview fixture pre-warmed a workspace session, and the real runner claimed
// its dispatch issue through its ordinary issue lane and spent a Codex turn on
// it. A workspace is its own work item kind and never project work, whether
// its workspace is open or long closed (decisions section 18.1).
func TestWorkspaceItemsAreNotOrdinaryWork(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)
	session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	item := f.item(t, session.ID)

	// The issue lane asks for whatever is next. It reaches the subject issue,
	// never the workspace item, even though the workspace item is a
	// dispatchable issue in the same project.
	claimed := f.ordinaryClaim(t, "", "ordinary-next")
	requireNativeStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimed, &lease)
	if string(lease.WorkItemID) != string(f.issue.WorkItemID) {
		t.Fatalf("the issue lane claimed %q, want the subject issue %q", lease.WorkItemID, f.issue.WorkItemID)
	}

	// Naming it outright is refused too: the exclusion is the candidate
	// query, not a preference the caller can talk its way past.
	if pinned := f.ordinaryClaim(t, item, "ordinary-pinned"); pinned.Code == http.StatusOK {
		t.Fatalf("the issue lane claimed the workspace item: %s", pinned.Body.String())
	}

	// The workspace lane, which declares the workspace capability, still gets
	// it.
	workspaceLease := f.claim(t, item)
	if string(workspaceLease.WorkItemID) != item {
		t.Fatalf("the workspace lane claimed %q, want the workspace item %q", workspaceLease.WorkItemID, item)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(workspaceLease.ID)+"/release",
		f.runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: workspaceLease.FencingToken, Reason: "completed"}), http.StatusNoContent)

	// Once the workspace has ended the association closes. Its issue is a
	// spent dispatch record, so neither lane picks it up again: before the
	// fix this was the state the dogfood runner found it in.
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE workspace_items SET closed_at = ? WHERE work_item_id = ?", formatHubTime(time.Now().UTC()), item); err != nil {
		t.Fatal(err)
	}
	if reclaimed := f.ordinaryClaim(t, item, "ordinary-closed"); reclaimed.Code == http.StatusOK {
		t.Fatalf("the issue lane claimed a closed workspace item: %s", reclaimed.Body.String())
	}
	if reclaimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
			SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2,
			Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}); reclaimed.Code == http.StatusOK {
		t.Fatalf("the workspace lane claimed a closed workspace item: %s", reclaimed.Body.String())
	}
}

// ordinaryClaim is the issue lane's claim: no workspace capability declared.
// An empty item asks for whatever is next.
func (f *workspaceRunnerFixture) ordinaryClaim(t *testing.T, item, session string) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
			SessionID: session, TTLSeconds: 600, ProtocolMajor: 2,
			Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
}

// The claim gate decides which runner may be handed a workspace item, and it
// is stricter than the coordinator's because the runner has to be able to
// serve the surfaces and, when a worktree is retained, to be the one holding
// it.
func TestWorkspaceClaimable(t *testing.T) {
	t.Parallel()
	const runner = "runner_one"
	record := workspaceRecord{Requires: []string{workspacesession.CapabilityFiles, workspacesession.CapabilityDiff}}
	for _, test := range []struct {
		name         string
		capabilities workspacesession.Capabilities
		fresh        bool
		retained     string
		want         bool
		reason       string
	}{
		{
			name: "a runner with no fresh report serves nothing", capabilities: workspaceTestCapabilities,
			want: false, reason: "the runner has no fresh workspace capability report",
		},
		{
			name: "a runner missing a required surface is not offered the item",
			// The request asked for a diff; handing it to a runner that only
			// serves files would open a workspace that cannot do the job.
			capabilities: workspacesession.Capabilities{Files: true}, fresh: true,
			want: false, reason: "the runner does not serve every required surface",
		},
		{
			name: "a retained worktree is only offered to the runner that produced it",
			// Nobody else has that tree, so a second runner would silently
			// show the reader a fresh checkout of a different one.
			capabilities: workspaceTestCapabilities, fresh: true, retained: "runner_two",
			want: false, reason: "the attempt's retained worktree belongs to another runner",
		},
		{
			name:         "an eligible runner holding the retained worktree may claim",
			capabilities: workspaceTestCapabilities, fresh: true, retained: runner, want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			claimable, reason := workspaceClaimable(record, test.capabilities, test.fresh, runner, test.retained)
			if claimable != test.want || reason != test.reason {
				t.Fatalf("workspaceClaimable() = %v, %q, want %v, %q", claimable, reason, test.want, test.reason)
			}
		})
	}
}

// reportProviderCapacity gives the runner the provider report a real one sends
// on every heartbeat, and the capacity the claim path measures it against.
//
// The fixture's runner reports no provider account by default, which is what
// hid the defect below: a claim from a runner that reports nothing leaves the
// provider path before it can refuse anything.
func (f *workspaceRunnerFixture) reportProviderCapacity(t *testing.T, capacity int) {
	t.Helper()
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local",
		Models: []string{"gpt-6-astra"}, MaxConcurrent: 4, Availability: "available", ObservedAt: f.service.config.now().UTC()}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential,
		map[string]any{"display_name": "Runner", "capacity": capacity, "version": "test",
			"provider_reports": []providercapacity.Report{report}}), http.StatusNoContent)
}

// reservations counts the provider reservations written against a lease.
func (f workspaceFixture) reservations(t *testing.T, lease tracker.LeaseID) int {
	t.Helper()
	var rows int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM provider_reservations WHERE lease_id = ?", lease).Scan(&rows); err != nil {
		t.Fatalf("count provider reservations for %s: %v", lease, err)
	}
	return rows
}

// TestWorkspaceClaimTakesCapacityNotAProviderReservation is the September 12
// fourth dogfood run's first observation. The runner's workspace lane was
// running and was being offered workspace items, and every claim it made was
// refused 409 provider_incompatible, "Work item needs local model selection
// before a provider reservation can be claimed".
//
// A workspace session serves files over the relay and runs no model. Section
// 18.1 gives its claim one slot of the runner's capacity and a workspace lease
// with its own fencing token, and says nothing about a provider reservation,
// so the provider path is not this claim's to walk.
func TestWorkspaceClaimTakesCapacityNotAProviderReservation(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)
	// One slot, and a Codex account reported the way a real runner reports it.
	// The report is the whole precondition: it is what made the hub look for a
	// model selection the lane has no reason to carry.
	f.reportProviderCapacity(t, 1)

	session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	item := f.item(t, session.ID)

	// f.claim sends what hubclient.WorkspaceClaimer sends: the workspace
	// capability, and no provider candidates.
	lease := f.claim(t, item)
	if string(lease.WorkItemID) != item {
		t.Fatalf("the workspace lane claimed %q, want the workspace item %q", lease.WorkItemID, item)
	}
	if lease.FencingToken <= 0 {
		t.Fatalf("the workspace claim returned fencing token %d, want a real one", lease.FencingToken)
	}
	if lease.ProviderReservation != nil {
		t.Fatalf("the workspace claim carried a provider reservation %+v, want none", lease.ProviderReservation)
	}
	if rows := f.reservations(t, lease.ID); rows != 0 {
		t.Fatalf("the workspace claim wrote %d provider reservations, want 0", rows)
	}

	// The slot is genuinely taken. A second workspace, eligible in every other
	// way, is refused by capacity and by nothing else: two claims cannot both
	// be admitted against one remaining slot (decisions section 18.1).
	second := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	failure := requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims",
		f.runner.redemption.Credential, tracker.NativeClaim{PolicyID: f.policy,
			WorkItemID: tracker.NativeWorkItemID(f.item(t, second.ID)), MachineID: f.runner.binding.MachineID,
			SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2,
			Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}),
		http.StatusConflict, "runner_capacity")
	if failure.Message == "" {
		t.Fatal("the capacity refusal carried no message")
	}

	// The issue lane is unchanged by any of this: a workspace item is not
	// project work, with or without a provider report in play.
	if claimed := f.ordinaryClaim(t, item, "ordinary-workspace-item"); claimed.Code == http.StatusOK {
		t.Fatalf("the issue lane claimed the workspace item: %s", claimed.Body.String())
	}
}

// TestWorkspaceCloseEndsItsDispatchItem is the fourth run's second
// observation: closing a workspace left its work item in Todo and
// dispatchable, so the board carried a row that looked like outstanding work
// and was not. Closing now moves the item to a terminal state, the way
// closeCoordinatorItem does for a coordinator item.
func TestWorkspaceCloseEndsItsDispatchItem(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID,
		credential: apiCredential{Scope: apiScopeAdmin}}
	for _, test := range []struct {
		name  string
		close func(t *testing.T, session workspacesession.Session)
	}{
		{
			name: "an owner deleting a workspace ends its item",
			close: func(t *testing.T, session workspacesession.Session) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete,
					f.base+"/workspaces/"+session.ID, f.token, nil), http.StatusNoContent)
			},
		},
		{
			name: "a workspace that fails ends its item too",
			close: func(t *testing.T, session workspacesession.Session) {
				lease := f.claim(t, f.item(t, session.ID))
				requireNativeStatus(t, f.workerPost(t, session.ID, "bind", workspaceBindRequest{
					workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities,
					Isolation: workspacesession.IsolationContainer}), http.StatusOK)
				requireNativeStatus(t, f.workerPost(t, session.ID, "unbind", workspaceUnbindRequest{
					workspaceWorkerIdentity: workspaceIdentity(lease),
					Reason:                  workspacesession.ReasonRunnerRestarted}), http.StatusNoContent)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
			item := f.item(t, session.ID)
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, item)
			if err != nil {
				t.Fatal(err)
			}
			if issue.Terminal {
				t.Fatalf("the workspace item opened terminal in %q", issue.State)
			}
			test.close(t, session)
			if !f.itemClosed(t, session.ID) {
				t.Fatal("closing the workspace left the association open")
			}
			closed, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, item)
			if err != nil {
				t.Fatal(err)
			}
			if !closed.Terminal {
				t.Fatalf("the workspace item stayed in %q, want a terminal state", closed.State)
			}
			// Terminal is what a person reading the board sees; the candidate
			// query reads dispatchable. Neither lane reaches it again.
			if claimed := f.ordinaryClaim(t, item, newNativeID("ordinary")); claimed.Code == http.StatusOK {
				t.Fatalf("the issue lane claimed a closed workspace item: %s", claimed.Body.String())
			}
			if claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims",
				f.runner.redemption.Credential, tracker.NativeClaim{PolicyID: f.policy,
					WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
					SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2,
					Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability},
				}); claimed.Code == http.StatusOK {
				t.Fatalf("the workspace lane claimed a closed workspace item: %s", claimed.Body.String())
			}
		})
	}
}
