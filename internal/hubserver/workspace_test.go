package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Workspace sessions end to end: the request and its
// limits, the dispatch item the hub creates for it, the worker tuple that
// fences it, and the deadlines that make a workspace nobody is using stop
// costing a runner slot.

// workspaceTestCapabilities is what a runner in these tests reports it can
// serve. It deliberately excludes the terminal: the terminal contract leaves that off
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

func newWorkspaceFixture(t *testing.T, cfg *WorkspaceConfig, clocks ...func() time.Time) workspaceFixture {
	t.Helper()
	if cfg == nil {
		cfg = &WorkspaceConfig{Enabled: true}
	}
	config := Config{Workspace: cfg}
	if len(clocks) != 0 {
		config.Clock = clocks[0]
	}
	return workspaceFixtureOn(t, newDefaultNativeFixture(t, config))
}

func workspaceFixtureOn(t *testing.T, f nativeFixture) workspaceFixture {
	t.Helper()
	service := f.service
	credential, _, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ownerID := credential.ID
	token := hubapi.TokenResponse{ID: hubapi.NewID("credential"), Token: hubapi.NewID("token")}
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: token.ID, Name: "operator-workspace-other", Hash: apikey.HashToken(token.Token), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: token.ID, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
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
	filled := map[string]any{"idempotency_key": hubapi.NewID("wsk")}
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

// item resolves the dispatch issue a workspace was given, which is the handle
// the runner is actually offered.
func (f workspaceFixture) item(t *testing.T, workspaceID string) string {
	t.Helper()
	var item string
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.(postgres.WorkspaceReader).WorkspaceDispatchItem(t.Context(), f.project.ID, workspaceID)
		item = string(row.WorkItemID)
		return err
	}); err != nil {
		t.Fatalf("read workspace item for %s: %v", workspaceID, err)
	}
	return item
}

func (f workspaceFixture) itemClosed(t *testing.T, workspaceID string) bool {
	t.Helper()
	var closed bool
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.(postgres.WorkspaceReader).WorkspaceDispatchItem(t.Context(), f.project.ID, workspaceID)
		closed = row.ClosedAt != nil
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return closed
}

// occupancy reports the workspace's occupancy rows as (started, ended) pairs.
// The usage report adds runner seconds from this table and never from relay
// traffic, so a missing or never-ended row is a billing bug, not a detail.
func (f workspaceFixture) occupancy(t *testing.T, workspaceID string) (rows int, open int) {
	t.Helper()
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		entries, err := r.(postgres.WorkspaceReader).WorkspaceOccupancy(t.Context(), f.project.ID, workspaceID, 100)
		if err != nil {
			return err
		}
		rows = len(entries)
		for _, entry := range entries {
			if entry.EndedAt == nil {
				open++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read workspace occupancy: %v", err)
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
	runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	runner.reportCapacity(t)
	worker := runner.redemption.Credential
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: subject.WorkItemID, MachineID: runner.binding.MachineID,
			SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
			Capabilities: []string{"native_issues", "scoped_collaboration"}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	attempt, run := hubapi.NewID("attempt"), hubapi.NewID("run")
	identity := &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}
	path := f.base + "/work-items/" + string(subject.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("start")}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: identity, LeaseID: lease.ID, FencingToken: lease.FencingToken,
			RunID: run, AttemptID: attempt, PolicyID: f.policy},
	}), http.StatusOK)
	if running {
		return attempt
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 2, Identity: identity, LeaseID: lease.ID, FencingToken: lease.FencingToken,
			RunID: run, AttemptID: attempt, PolicyID: f.policy, Outcome: "succeeded"},
	}), http.StatusOK)
	return attempt
}

// A workspace request carries the default surfaces and creates the dispatch
// item that a runner -- and nothing else -- is meant to see.
func TestWorkspaceRequestCreatesSessionAndDispatchItem(t *testing.T) {
	t.Run("two subjects keep distinct sessions and dispatch items", func(t *testing.T) {
		pair := newWorkspaceFixture(t, nil)
		second := pair.create(t, "second workspace subject")
		firstSession := pair.opened(t, pair.token, map[string]any{"work_item_id": string(pair.issue.WorkItemID)})
		secondSession := pair.opened(t, pair.token, map[string]any{"work_item_id": string(second.WorkItemID)})
		if firstSession.ID == secondSession.ID || firstSession.WorkItemID != string(pair.issue.WorkItemID) || secondSession.WorkItemID != string(second.WorkItemID) || firstSession.Ref != "" || secondSession.Ref != "" || firstSession.Revision != 1 || secondSession.Revision != 1 {
			t.Fatalf("workspace subjects collided: first=%+v second=%+v", firstSession, secondSession)
		}
		if pair.item(t, firstSession.ID) == pair.item(t, secondSession.ID) {
			t.Fatal("two workspaces shared one dispatch item")
		}
	})
	t.Run("association failure rolls back session issue history and receipt", func(t *testing.T) {
		failed := newWorkspaceFixture(t, nil)
		admin := hostedPostgresAdmin(t, failed.service)
		counts := func() [4]int64 {
			t.Helper()
			var result [4]int64
			for index, table := range []string{"workspace_sessions", "issues", "collaboration_versions", "collaboration_events"} {
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE organization_id=$1 AND project_id=$2", failed.project.OrganizationID, failed.project.ID).Scan(&result[index]); err != nil {
					t.Fatal(err)
				}
			}
			return result
		}
		before := counts()
		if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_workspace_creation_link() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'workspace association refused'; END $$`); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `CREATE TRIGGER reject_workspace_creation_link BEFORE INSERT ON workspace_items FOR EACH ROW EXECUTE FUNCTION reject_workspace_creation_link()`); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, failed.post(t, failed.token, map[string]any{"idempotency_key": "fail-link", "work_item_id": string(failed.issue.WorkItemID)}), http.StatusInternalServerError)
		if after := counts(); after != before {
			t.Fatalf("refused association leaked durable rows: before=%v after=%v", before, after)
		}
		if err := failed.service.config.TenantRepository.ReadTenant(t.Context(), failed.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.Receipt(t.Context(), failed.ownerID, nativeOperation(hubscope.NativeScope{Organization: failed.project.OrganizationID, Project: failed.project.ID}, "POST", "/workspaces"), "fail-link")
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("failed workspace retained a receipt: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Parallel()
	f := newWorkspaceFixture(t, nil)

	t.Run("a request without requires opens the two read-only surfaces and emits one workspace.requested", func(t *testing.T) {
		session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
		if session.State != workspacesession.StateRequested || session.ReadOnly || session.Revision != 1 {
			t.Fatalf("session = %#v", session)
		}
		if len(session.Requires) != 1 || session.Requires[0] != workspacesession.CapabilityFiles {
			t.Fatalf("requires = %v, want the workspace session contract default narrowed to what the relay serves", session.Requires)
		}
		if session.CreatedBy != f.ownerID || session.IdleTimeoutSeconds != int(defaultWorkspaceIdleTimeout/time.Second) {
			t.Fatalf("session provenance = %#v", session)
		}
	})

	t.Run("the dispatch item is a detent:workspace issue the board does not show", func(t *testing.T) {
		session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
		item := f.item(t, session.ID)
		if item == string(f.issue.WorkItemID) {
			t.Fatal("the workspace dispatched its own subject instead of a workspace item")
		}
		drainTestNativeBoard(t, f.service)
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=100", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.Page[tracker.NativeIssue]
		decodeHubResponse(t, response, &page)
		if listedWorkItem(page, item) {
			t.Fatal("a workspace item appeared as project work")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=100&include=workspace", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		page = tracker.Page[tracker.NativeIssue]{}
		decodeHubResponse(t, response, &page)
		if !listedWorkItem(page, item) {
			t.Fatal("include=workspace did not surface the workspace item")
		}
		for _, transport := range []string{"stdio", "http"} {
			current := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "workspace-list-"+transport)
			call := hostedContextProtocol(t, f.service, current, transport)
			for _, include := range [][]string{nil, {"workspace"}} {
				args := map[string]any{"project_id": string(f.project.ID), "limit": 100}
				if include != nil {
					args["include"] = include
				}
				raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
				var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, listed := range result.Data.Items {
					found = found || string(listed.WorkItemID) == item
				}
				if found != (include != nil) {
					t.Fatalf("%s workspace projection: found=%v include=%v", transport, found, include)
				}
			}
		}
	})

	t.Run("the reserved detent:workspace label cannot be set by an API caller", func(t *testing.T) {
		// The label selects the runner's hold-open mode, so a tracker writer
		// who could set it would park a runner on any issue indefinitely.
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token,
			tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("label")}, Title: "Smuggled",
				State: "Todo", Labels: []string{workspaceItemLabel}})
		requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
	})
}

// listedWorkItem reports whether a work item page names an item.
func listedWorkItem(page tracker.Page[tracker.NativeIssue], item string) bool {
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
		var failure hubapi.NativeError
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

// The two caps of the workspace session contract are checked together and say which one was
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
	var failure hubapi.NativeError
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
			t.Fatalf("a replay opened a second workspace: %d", len(listing.Workspaces))
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
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	f := newWorkspaceFixture(t, nil, clock.Now)
	other := f.create(t, "second-subject")
	first := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	clock.Advance(time.Second)
	second := f.opened(t, f.token, map[string]any{"work_item_id": string(other.WorkItemID)})

	t.Run("listing filters by work item and by state", func(t *testing.T) {
		if listing := f.list(t, ""); len(listing.Workspaces) != 2 || listing.Workspaces[0].ID != second.ID || listing.Workspaces[1].ID != first.ID {
			t.Fatalf("listing is not newest first: %+v", listing.Workspaces)
		}
		if listing := f.list(t, "?work_item="+string(other.WorkItemID)+"&state=requested"); len(listing.Workspaces) != 1 || listing.Workspaces[0].ID != second.ID {
			t.Fatalf("combined subject/state filter: %+v", listing.Workspaces)
		}
		if listing := f.list(t, "?work_item="+string(other.WorkItemID)+"&state=closed"); len(listing.Workspaces) != 0 {
			t.Fatalf("combined subject/state filter returned wrong state: %+v", listing.Workspaces)
		}
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
// to (the workspace session contract's claim gate).
type workspaceRunnerFixture struct {
	workspaceFixture
	runner runnerFixture
}

func newWorkspaceRunnerFixture(t *testing.T) *workspaceRunnerFixture {
	t.Helper()
	// The caps are lifted well clear of the subtests: these tests are about
	// the owner tuple, and a refusal from the workspace session contract's limits would only
	// hide it.
	return workspaceRunnerFixtureOn(t, newWorkspaceFixture(t, &WorkspaceConfig{Enabled: true, PersonMaxOpen: 50, PlanMaxOpen: 50}))
}

func workspaceRunnerFixtureOn(t *testing.T, f workspaceFixture) *workspaceRunnerFixture {
	t.Helper()
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	// The capacity is generous so that one fixture can hold several
	// workspaces at once; the claim gate, not the host, is what these tests
	// are about.
	r.redemption.Capacity = 8
	r.enroll(t)
	report := map[string]any{"id": r.binding.MachineID, "hostname": "customer-host", "display_name": "Runner",
		"backend_isolation": r.redemption.BackendIsolation, "capacity": 8, "version": "test", "workspace_capabilities": workspaceTestCapabilities,
		"workspace_isolation": workspacesession.IsolationContainer}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", r.redemption.Credential, report), http.StatusOK)
	report["capacity_configuration"] = runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.service.config.Clock()}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, report), http.StatusOK)
	return &workspaceRunnerFixture{workspaceFixture: f, runner: r}
}

// claim takes the lease on one work item. A worker request presents that
// lease, so a test that wants to prove the fence has to hold a real one.
func (f *workspaceRunnerFixture) claim(t *testing.T, item string) tracker.NativeLease {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
			SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
			// The workspace lane declares the workspace capability; without it
			// the hub's candidate query excludes workspace items entirely.
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
	})

	t.Run("a lease held on a different work item cannot bind this workspace", func(t *testing.T) {
		session, _ := f.requested(t)
		// Without this check any runner holding any lease in the project
		// could bind any workspace.
		claimed := f.ordinaryClaim(t, string(f.issue.WorkItemID), hubapi.NewID("session"))
		requireNativeStatus(t, claimed, http.StatusOK)
		var elsewhere tracker.NativeLease
		decodeHubResponse(t, claimed, &elsewhere)
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
func TestWorkspaceWorkerSharesClaimLockOrder(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{Workspace: &WorkspaceConfig{Enabled: true, PersonMaxOpen: 50, PlanMaxOpen: 50}})
	f := workspaceRunnerFixtureOn(t, workspaceFixtureOn(t, newNativeFixture(t, service, "", "Example project")))
	bound, lease := f.bound(t)
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.runner.redemption.Credential), "", "")
	if err != nil {
		t.Fatal(err)
	}
	commands, ok := f.service.config.TenantRepository.(interface {
		WriteRunnerCommand(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	})
	if !ok {
		t.Fatal("PostgreSQL runner command owner unavailable")
	}
	ctx, cancel := context.WithCancel(t.Context())
	runnerLocked, claimOrganization, releaseClaim := make(chan struct{}), make(chan struct{}), make(chan struct{})
	claimResult, claimDone := make(chan error, 1), make(chan struct{})
	workerResult, workerDone := make(chan *httptest.ResponseRecorder, 1), make(chan struct{})
	go func() {
		defer close(claimDone)
		claimResult <- commands.WriteRunnerCommand(ctx, f.project.OrganizationID, credential.ID, credential.Runner.RunnerID, func(ctx context.Context, w tenantstore.Writer) error {
			close(runnerLocked)
			select {
			case <-claimOrganization:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := w.LockRelationshipGraph(ctx); err != nil {
				return err
			}
			select {
			case <-releaseClaim:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-claimDone })
	select {
	case <-runnerLocked:
	case err := <-claimResult:
		t.Fatalf("claim runner lock: %v", err)
	}
	go func() {
		defer close(workerDone)
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/workspaces/"+bound.Session.ID+"/worker/heartbeat", f.runner.redemption.Credential, workspaceHeartbeatRequest{workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities, Isolation: workspacesession.IsolationContainer}).WithContext(ctx)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		workerResult <- response
	}()
	t.Cleanup(func() { cancel(); <-workerDone })
	admin := hostedPostgresAdmin(t, f.service)
	testenv.WaitUntil(t, ctx, func() error {
		select {
		case response := <-workerResult:
			t.Fatalf("workspace heartbeat returned before runner lock wait: status=%d body=%s", response.Code, response.Body.String())
		default:
		}
		var waiting bool
		if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%runner_identities%')`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("workspace heartbeat has not reached runner identity lock")
		}
		return nil
	})
	close(claimOrganization)
	close(releaseClaim)
	if err := <-claimResult; err != nil {
		t.Fatalf("claim organization lock: %v", err)
	}
	response := <-workerResult
	requireNativeStatus(t, response, http.StatusOK)
	if rows, open := f.occupancy(t, bound.Session.ID); rows != 1 || open != 1 {
		t.Fatalf("concurrent claim changed occupancy: rows=%d open=%d", rows, open)
	}
}

func TestWorkspaceWorkerRecoversWithOriginalLease(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)
	for range 2 {
		bound, lease := f.bound(t)
		if err := f.service.workspaces.recoverOpenWorkspaces(t.Context()); err != nil {
			t.Fatal(err)
		}
		recovered := f.read(t, bound.Session.ID)
		if recovered.State != workspacesession.StateRequested || recovered.Reason != workspacesession.ReasonHubRestarted || recovered.RunnerID != bound.Session.RunnerID || recovered.MachineID != bound.Session.MachineID || recovered.Capabilities != nil {
			t.Fatalf("recovery lost original owner: before=%+v after=%+v", bound.Session, recovered)
		}
		response := f.workerPost(t, bound.Session.ID, "bind", workspaceBindRequest{workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities, Isolation: workspacesession.IsolationContainer})
		requireNativeStatus(t, response, http.StatusOK)
		var rebound workspaceBindResponse
		decodeHubResponse(t, response, &rebound)
		if rebound.Session.State != workspacesession.StateReady || rebound.Owner != bound.Owner {
			t.Fatalf("restart bind replaced owner or failed to become ready: %+v", rebound)
		}
		if rows, open := f.occupancy(t, bound.Session.ID); rows != 1 || open != 1 {
			t.Fatalf("rebind duplicated occupancy: rows=%d open=%d", rows, open)
		}
	}
}

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
	var expires time.Time
	var released bool
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.Lease(t.Context(), string(id))
		expires, released = row.ExpiresAt, row.ReleasedAt != nil
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return expires, released
}

// setLeaseExpiry moves a lease's expiry to a known moment, so a renewal is
// observable whatever the clock's granularity and whatever TTL the claim asked
// for.
func (f *workspaceRunnerFixture) setLeaseExpiry(t *testing.T, id tracker.LeaseID, at time.Time) {
	t.Helper()
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(),
		"UPDATE leases SET acquired_at=LEAST(acquired_at,$1::timestamptz-interval '2 seconds'),renewed_at=LEAST(renewed_at,$1::timestamptz-interval '1 second'),expires_at=$1::timestamptz WHERE organization_id=$2 AND lease_id=$3", at, f.project.OrganizationID, id); err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceHeartbeatRenewsTheLease is the September 12 fifth dogfood
// defect: nothing on the workspace path renewed the workspace lease, so a Files
// session lived exactly one lease_ttl_seconds from the claim and then answered
// the person revoked. The workspace session contract puts the renewal on the heartbeat itself --
// "POST .../worker/heartbeat (renews the lease ...)" -- and it happens in the
// same transaction as the session row, so the runner needs no second call.
func TestWorkspaceHeartbeatRenewsTheLease(t *testing.T) {
	t.Parallel()
	f := newWorkspaceRunnerFixture(t)

	t.Run("a heartbeat moves the lease expiry a full lease TTL ahead", func(t *testing.T) {
		bind, lease := f.bound(t)
		// A lease about to run out is the case the defect was about: before
		// the fix the heartbeat wrote workspace_sessions and left this alone.
		about := f.service.config.Clock().UTC().Add(2 * time.Second)
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
		if ahead := renewed.Sub(f.service.config.Clock().UTC()); ahead > workspacesession.LeaseTTL || ahead < workspacesession.LeaseTTL-10*time.Second {
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
		f.setLeaseExpiry(t, lease.ID, f.service.config.Clock().UTC().Add(time.Second))
		requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: workspaceTestCapabilities}), http.StatusOK)
		again, _ := f.leaseWindow(t, lease.ID)
		if !again.After(f.service.config.Clock().UTC().Add(workspacesession.LeaseTTL - 10*time.Second)) {
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
		f.setLeaseExpiry(t, lease.ID, f.service.config.Clock().UTC().Add(-time.Second))
		requireNativeCode(t, f.workerPost(t, bind.Session.ID, "heartbeat", workspaceHeartbeatRequest{
			workspaceWorkerIdentity: workspaceIdentity(lease), State: workspacesession.StateReady,
			Capabilities: workspaceTestCapabilities}), http.StatusConflict, "stale_execution")
		after, _ := f.leaseWindow(t, lease.ID)
		if after.After(f.service.config.Clock().UTC()) {
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
		if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(),
			"UPDATE workspace_sessions SET state = 'closing', reason = $1 WHERE organization_id=$2 AND project_id=$3 AND id = $4",
			workspacesession.ReasonClosedByActor, f.project.OrganizationID, f.project.ID, bind.Session.ID); err != nil {
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
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	f := newWorkspaceFixture(t, &WorkspaceConfig{Enabled: true, RequestTimeout: time.Minute}, clock.Now)
	opened := f.opened(t, f.token, map[string]any{
		"work_item_id": string(f.issue.WorkItemID),
		"requires":     []string{workspacesession.CapabilityFiles},
	})
	if opened.State != workspacesession.StateRequested {
		t.Fatalf("a new workspace = %#v, want requested", opened)
	}

	clock.Advance(time.Minute)
	f.service.workspaces.sweep(t.Context())

	failed := f.read(t, opened.ID)
	if failed.State != workspacesession.StateFailed || failed.Reason != workspacesession.ReasonNoRunner {
		t.Fatalf("workspace after the request timeout = state %q reason %q, want failed no_runner", failed.State, failed.Reason)
	}
	if failed.RunnerID != "" {
		t.Fatalf("a workspace no runner claimed reports runner %q", failed.RunnerID)
	}
}

// TestWorkspaceRequestAfterThePreviousOneClosedIsAccepted covers the second
// half of the same story: the pre-warmed workspace expires on its idle
// timeout, and the next Files click has to be able to open another one.
//
// The workspace session contract refuses a second workspace only while one is open on the same
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
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(),
					"UPDATE workspace_sessions SET state = 'closed', reason = $1 WHERE organization_id=$2 AND project_id=$3 AND id = $4",
					workspacesession.ReasonExpired, f.project.OrganizationID, f.project.ID, id); err != nil {
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

// Every deadline of the workspace session contract that no request would ever reach on its own.
// decideDeadline is pure, so the whole timing contract is a table rather than
// a set of tests that sleep.
func TestWorkspaceLifecycleUsesConfiguredTenant(t *testing.T) {
	t.Parallel()
	base := newDefaultNativeFixture(t, Config{})
	clock := &leaseTestClock{value: base.service.config.Clock().UTC().Truncate(time.Microsecond)}
	fixtures := []nativeFixture{base, newNativeFixture(t, base.service, base.project.OrganizationID+"_second", "Second project")}
	services := make([]*workspaceService, 0, 2)
	for _, fixture := range fixtures {
		subject := fixture.create(t, "lifecycle-subject")
		server := &Service{config: Config{TenantRepository: base.service.config.TenantRepository, Hosted: &hostedplans.HostedConfig{OrganizationID: string(fixture.project.OrganizationID)}, Logger: discardLogger(), Clock: clock.Now}}
		service := newWorkspaceService(server, WorkspaceConfig{Enabled: true})
		if server.accessOrganization() != fixture.project.OrganizationID {
			t.Fatalf("configured tenant resolved to %q", server.accessOrganization())
		}
		services = append(services, service)
		for index := 1; index <= 2; index++ {
			now := clock.Now()
			if err := base.service.config.TenantRepository.WriteTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.(postgres.WorkspaceWriter).CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: fixture.project.OrganizationID, ProjectID: fixture.project.ID, ID: fmt.Sprintf("ws_%032x", index), SubjectWorkItemID: string(subject.WorkItemID), Requires: []string{"files"}, IdleTimeoutSeconds: 60, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: "example", CreatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	clock.Advance(time.Minute)
	for selected, service := range services {
		records, err := service.dueWorkspaces(t.Context(), clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 2 {
			t.Fatalf("tenant %d sweep rows=%d", selected, len(records))
		}
		for _, record := range records {
			if record.OrganizationID != fixtures[selected].project.OrganizationID {
				t.Fatalf("sweep crossed tenant boundary: %+v", record)
			}
			moved, changed, err := service.applyDeadline(t.Context(), record, clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !changed || moved.State != workspacesession.StateFailed || moved.Reason != workspacesession.ReasonNoRunner {
				t.Fatalf("tenant deadline did not execute: changed=%t row=%+v", changed, moved)
			}
		}
		for tenant, fixture := range fixtures {
			if err := base.service.config.TenantRepository.ReadTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.(postgres.WorkspaceReader).WorkspaceSessions(t.Context(), fixture.project.ID, "", 10)
				if err != nil {
					return err
				}
				if len(rows) != 2 {
					t.Fatalf("tenant %d rows=%d", tenant, len(rows))
				}
				want := workspacesession.StateRequested
				if tenant <= selected {
					want = workspacesession.StateFailed
				}
				for _, row := range rows {
					if row.State != want {
						t.Fatalf("sweep %d mutated tenant %d: state=%q want=%q", selected, tenant, row.State, want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestWorkspaceItemsAreNotOrdinaryWork is the September 12 dogfood defect: the
// preview fixture pre-warmed a workspace session, and the real runner claimed
// its dispatch issue through its ordinary issue lane and spent a Codex turn on
// it. A workspace is its own work item kind and never project work, whether
// its workspace is open or long closed.
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
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(),
		"UPDATE workspace_items SET closed_at = $1 WHERE organization_id=$2 AND project_id=$3 AND work_item_id = $4", f.service.config.Clock(), f.project.OrganizationID, f.project.ID, item); err != nil {
		t.Fatal(err)
	}
	if reclaimed := f.ordinaryClaim(t, item, "ordinary-closed"); reclaimed.Code == http.StatusOK {
		t.Fatalf("the issue lane claimed a closed workspace item: %s", reclaimed.Body.String())
	}
	if reclaimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
		tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
			SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
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
type workspaceClaimChangeRepository struct {
	tenantstore.Repository
	before func()
}

func (r *workspaceClaimChangeRepository) WriteRunnerCommand(ctx context.Context, organization tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	if r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.Repository.(interface {
		WriteRunnerCommand(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	}).WriteRunnerCommand(ctx, organization, credential, runner, write)
}

func (r *workspaceClaimChangeRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, organization, workload, read)
}

type workspaceClaimSourceFailureRepository struct {
	*workspaceClaimChangeRepository
	wrap func(tenantstore.Reader) tenantstore.Reader
}

func (r workspaceClaimSourceFailureRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error { return read(r.wrap(reader)) })
}

type workspaceClaimSourceFailureReader struct {
	tenantstore.Reader
	postgres.WorkspaceReader
	source  string
	attempt string
	subject tracker.NativeWorkItemID
	lease   string
	failure error
}

func (r workspaceClaimSourceFailureReader) IsWorkspaceWorkItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (bool, error) {
	return r.Reader.IsWorkspaceWorkItem(ctx, project, item)
}

func (r workspaceClaimSourceFailureReader) Attempt(ctx context.Context, id string) (leasestore.Attempt, error) {
	if r.source == "attempt" && id == r.attempt {
		return leasestore.Attempt{}, r.failure
	}
	return r.Reader.Attempt(ctx, id)
}

func (r workspaceClaimSourceFailureReader) WorkItem(ctx context.Context, project tracker.ProjectID, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	if r.source == "subject" && id == r.subject {
		return corestore.WorkItem{}, r.failure
	}
	return r.Reader.WorkItem(ctx, project, id)
}

func (r workspaceClaimSourceFailureReader) Lease(ctx context.Context, id string) (leasestore.Lease, error) {
	if r.source == "lease" && id == r.lease {
		return leasestore.Lease{}, r.failure
	}
	return r.Reader.Lease(ctx, id)
}

func (r workspaceClaimSourceFailureReader) RunnerForLease(ctx context.Context, id string) (runnerstore.Identity, error) {
	if r.source == "attempt_runner" && id == r.lease {
		return runnerstore.Identity{}, r.failure
	}
	return r.Reader.RunnerForLease(ctx, id)
}

func TestWorkspaceClaimable(t *testing.T) {
	t.Parallel()
	const runner = "runner_one"
	surfaces := []string{workspacesession.CapabilityFiles, workspacesession.CapabilityDiff}
	terminal := []string{workspacesession.CapabilityFiles, workspacesession.CapabilityDiff, workspacesession.CapabilityTerminal}
	withTerminal := workspacesession.Capabilities{Files: true, Diff: true, Terminal: true}
	for _, test := range []struct {
		name           string
		requires       []string
		capabilities   workspacesession.Capabilities
		fresh          bool
		retained       string
		isolation      string
		configured     string
		publicRetained string
		finished       bool
		expired        bool
		race           bool
		want           bool
		reason         string
	}{
		{
			name: "a runner with no fresh report serves nothing", requires: surfaces, capabilities: workspaceTestCapabilities,
			want: false, reason: "the runner has no fresh workspace capability report",
		},
		{
			name: "a runner missing a required surface is not offered the item", requires: surfaces,
			// The request asked for a diff; handing it to a runner that only
			// serves files would open a workspace that cannot do the job.
			capabilities: workspacesession.Capabilities{Files: true}, fresh: true,
			want: false, reason: "the runner does not serve every required surface",
		},
		{
			name: "a retained worktree is only offered to the runner that produced it", requires: surfaces,
			// Nobody else has that tree, so a second runner would silently
			// show the reader a fresh checkout of a different one.
			capabilities: workspaceTestCapabilities, fresh: true, retained: "runner_two",
			want: false, reason: "the attempt's retained worktree belongs to another runner",
		},
		{
			name: "an eligible runner holding the retained worktree may claim", requires: surfaces,
			capabilities: workspaceTestCapabilities, fresh: true, retained: runner, want: true,
		},
		{
			name: "a user-isolation runner is refused a terminal in a container organization", requires: terminal,
			capabilities: withTerminal, fresh: true, isolation: workspacesession.IsolationUser,
			configured: workspacesession.IsolationContainer,
			want:       false, reason: "the runner's terminal isolation is not allowed by the organization",
		},
		{
			name: "a container runner serves a terminal in a container organization", requires: terminal,
			capabilities: withTerminal, fresh: true, isolation: workspacesession.IsolationContainer,
			configured: workspacesession.IsolationContainer, want: true,
		},
		{
			name: "a user-isolation runner serves a terminal where the organization chose user", requires: terminal,
			capabilities: withTerminal, fresh: true, isolation: workspacesession.IsolationUser,
			configured: workspacesession.IsolationUser, want: true,
		},
		{
			name: "a finished retained attempt stays with its runner", requires: surfaces,
			capabilities: workspaceTestCapabilities, fresh: true, retained: runner, finished: true, want: true,
		},
		{
			name: "a finished retained attempt excludes another runner", requires: surfaces,
			capabilities: workspaceTestCapabilities, fresh: true, retained: "runner_two", finished: true,
			reason: "the attempt's retained worktree belongs to another runner",
		},
		{
			name: "expired retention permits a fresh checkout on another runner", requires: surfaces,
			capabilities: workspaceTestCapabilities, fresh: true, publicRetained: "runner_two", finished: true, expired: true, want: true,
		},
		{
			name: "a changed capability report is rechecked at commit", requires: surfaces,
			capabilities: workspacesession.Capabilities{Files: true}, fresh: true, race: true,
			reason: "the runner does not serve every required surface",
		},
		{
			name: "a changed retained attempt is rechecked at commit", requires: surfaces,
			capabilities: workspaceTestCapabilities, fresh: true, retained: "runner_two", race: true,
			reason: "the attempt's retained worktree belongs to another runner",
		},
		{
			name: "isolation does not matter without a terminal", requires: surfaces,
			capabilities: workspaceTestCapabilities, fresh: true, isolation: workspacesession.IsolationUser,
			configured: workspacesession.IsolationContainer, want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			record := workspaceRecord{Requires: test.requires}
			claimable, reason := workspaceClaimable(record, test.capabilities, test.fresh, runner, test.retained, test.isolation, test.configured)
			if claimable != test.want || reason != test.reason {
				t.Fatalf("workspaceClaimable() = %v, %q, want %v, %q", claimable, reason, test.want, test.reason)
			}
			f := workspaceRunnerFixtureOn(t, newWorkspaceFixture(t, &WorkspaceConfig{Enabled: true, PersonMaxOpen: 50, PlanMaxOpen: 50, RetainAfterRun: defaultWorkspaceRetainAfterRun}))
			admin := hostedPostgresAdmin(t, f.service)
			f.service.workspaces.config.Terminal.Isolation = test.configured
			heartbeat := func(capabilities workspacesession.Capabilities, isolation string) {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential, map[string]any{
					"display_name": "Runner", "capacity": 8, "version": minimumSupportedRunnerVersion, "backend_isolation": f.runner.redemption.BackendIsolation,
					"workspace_capabilities": capabilities, "workspace_isolation": isolation,
					"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.service.config.Clock()},
				}), http.StatusOK)
			}
			heartbeat(workspacesession.Capabilities{Files: true, Diff: true, Exec: true}, workspacesession.IsolationContainer)
			session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
			item := f.item(t, session.ID)
			f.setRequires(t, session.ID, test.requires)
			isolation := test.isolation
			if isolation == "" {
				isolation = workspacesession.IsolationContainer
			}
			if test.race {
				heartbeat(workspaceTestCapabilities, isolation)
			} else {
				heartbeat(test.capabilities, isolation)
			}
			if !test.fresh {
				if _, err := admin.ExecContext(t.Context(), "DELETE FROM runner_workspace_reports WHERE organization_id=$1 AND runner_id=$2", f.project.OrganizationID, f.runner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			retainedOwner := test.retained
			if test.publicRetained != "" {
				retainedOwner = test.publicRetained
			}
			attempt := ""
			if retainedOwner != "" {
				owner := f.runner
				if retainedOwner != runner {
					owner = prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
					owner.enroll(t)
				}
				lease := nativeAttemptFixtureLease(t, owner, f.issue, hubapi.NewID("retained-session"))
				start := nativeStartedEvent(lease)
				start.IdempotencyKey = hubapi.NewID("start")
				attempt = start.Data.AttemptID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", owner.redemption.Credential, start), http.StatusOK)
				if test.finished {
					finish := start
					finish.IdempotencyKey, finish.Type, finish.Data.Sequence, finish.Data.Outcome = hubapi.NewID("finish"), "run.finished", 2, "succeeded"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", owner.redemption.Credential, finish), http.StatusOK)
				}
				if test.expired {
					now := f.service.config.Clock().UTC()
					f.service.config.Clock = func() time.Time { return now }
					if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET updated_at=$3 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, attempt, now.Add(-f.service.workspaces.config.RetainAfterRun)); err != nil {
						t.Fatal(err)
					}
				}
				if !test.race {
					if _, err := admin.ExecContext(t.Context(), "UPDATE workspace_sessions SET attempt_id=$4 WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, session.ID, attempt); err != nil {
						t.Fatal(err)
					}
				}
			}
			request := tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID, SessionID: hubapi.NewID("workspace-claim"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
			preview := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.runner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: request})
			requireNativeStatus(t, preview, http.StatusOK)
			var page tracker.NativeCapacityPage
			decodeHubResponse(t, preview, &page)
			listed := slices.ContainsFunc(page.Items, func(value tracker.NativeIssue) bool { return value.WorkItemID == tracker.NativeWorkItemID(item) })
			wantPreview := test.want || test.race
			if listed != wantPreview {
				t.Fatalf("workspace preview listed=%t want=%t", listed, wantPreview)
			}
			changed := false
			if test.race {
				original := f.service.config.TenantRepository
				f.service.config.TenantRepository = &workspaceClaimChangeRepository{Repository: original, before: func() {
					if attempt != "" {
						if _, err := admin.ExecContext(t.Context(), "UPDATE workspace_sessions SET attempt_id=$4 WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, session.ID, attempt); err != nil {
							t.Fatal(err)
						}
					} else if err := original.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						return w.ReplaceRunnerWorkspaceReport(t.Context(), f.runner.binding.RunnerID, test.capabilities, isolation)
					}); err != nil {
						t.Fatal(err)
					}
					changed = true
				}}
				defer func() { f.service.config.TenantRepository = original }()
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential, request)
			wantStatus := http.StatusConflict
			if test.want {
				wantStatus = http.StatusOK
			}
			requireNativeStatus(t, response, wantStatus)
			if test.race && !changed {
				t.Fatal("workspace claim did not reach the intervening write")
			}
			if !test.want {
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.WorkItem(t.Context(), f.project.ID, tracker.NativeWorkItemID(item))
					if err != nil {
						return err
					}
					_, err = r.ActiveLeaseForIssue(t.Context(), row.ID)
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("refused workspace acquired a lease: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

		})
	}
	for _, test := range []struct {
		name, source string
		failure      error
		wantHealthy  bool
	}{
		{name: "missing attempt does not block the lane", source: "attempt", failure: corestore.ErrNotFound, wantHealthy: true},
		{name: "missing subject does not block the lane", source: "subject", failure: corestore.ErrNotFound, wantHealthy: true},
		{name: "missing lease does not block the lane", source: "lease", failure: corestore.ErrNotFound, wantHealthy: true},
		{name: "missing retained runner does not block the lane", source: "attempt_runner", failure: corestore.ErrNotFound, wantHealthy: true},
		{name: "backend failures remain errors", source: "attempt", failure: errors.New("example backend failure")},
		{name: "authorization failures remain errors", source: "subject", failure: accessstore.ErrDenied},
		{name: "tenant corruption remains an error", source: "attempt", failure: corestore.ErrTenantMismatch},
		{name: "fence corruption remains an error", source: "lease", failure: corestore.ErrRevisionConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWorkspaceRunnerFixture(t)
			admin := hostedPostgresAdmin(t, f.service)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 8, "version": minimumSupportedRunnerVersion, "backend_isolation": f.runner.redemption.BackendIsolation, "workspace_capabilities": workspacesession.Capabilities{Files: true, Diff: true, Exec: true}, "workspace_isolation": workspacesession.IsolationContainer, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.service.config.Clock()}}), http.StatusOK)
			bad := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
			other := f.create(t, "Healthy workspace subject")
			good := f.opened(t, f.token, map[string]any{"work_item_id": string(other.WorkItemID)})
			badItem, goodItem := f.item(t, bad.ID), f.item(t, good.ID)
			lease := nativeAttemptFixtureLease(t, f.runner, f.issue, hubapi.NewID("source-session"))
			start := nativeStartedEvent(lease)
			start.IdempotencyKey = hubapi.NewID("source-start")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.runner.redemption.Credential, start), http.StatusOK)
			attachedAttempt := start.Data.AttemptID
			if test.wantHealthy && test.source == "attempt" {
				attachedAttempt = hubapi.NewID("missing-attempt")
			}
			if test.wantHealthy && test.source == "attempt_runner" {
				if _, err := admin.ExecContext(t.Context(), "DELETE FROM runner_lease_assignments WHERE organization_id=$1 AND lease_id=$2", f.project.OrganizationID, string(lease.ID)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE workspace_sessions SET attempt_id=$4 WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, bad.ID, attachedAttempt); err != nil {
				t.Fatal(err)
			}
			original := f.service.config.TenantRepository
			if err := original.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				for index, id := range []string{badItem, goodItem} {
					entry, err := w.QueueEntry(t.Context(), f.project.ID, tracker.NativeWorkItemID(id), string(f.project.ID))
					if err != nil {
						return err
					}
					entry.Priority, entry.Rank = new(int64(0)), fmt.Sprintf("%04d", index)
					if err := w.PutQueueEntry(t.Context(), entry); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			failureSource := test.source
			if test.wantHealthy && (test.source == "attempt" || test.source == "attempt_runner") {
				failureSource = ""
			}
			f.service.config.TenantRepository = workspaceClaimSourceFailureRepository{workspaceClaimChangeRepository: &workspaceClaimChangeRepository{Repository: original}, wrap: func(r tenantstore.Reader) tenantstore.Reader {
				return workspaceClaimSourceFailureReader{Reader: r, WorkspaceReader: r.(postgres.WorkspaceReader), source: failureSource, attempt: start.Data.AttemptID, subject: f.issue.WorkItemID, lease: string(lease.ID), failure: test.failure}
			}}
			defer func() { f.service.config.TenantRepository = original }()
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.runner.redemption.Credential), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("authenticate claim runner status=%d error=%v", status, err)
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
			if !test.wantHealthy {
				err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := f.service.nativeWorkspaceClaimAllowed(t.Context(), r, scope, tracker.NativeWorkItemID(badItem), f.service.config.Clock())
					return err
				})
				if !errors.Is(err, test.failure) {
					t.Fatalf("workspace failure became a candidate refusal: %v want %v", err, test.failure)
				}
				return
			}
			request := tracker.NativeClaim{PolicyID: f.policy, MachineID: f.runner.binding.MachineID, SessionID: hubapi.NewID("lane"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}, LabelInclude: []string{workspaceItemLabel}}
			preview := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.runner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: request, Limit: 100})
			requireNativeStatus(t, preview, http.StatusOK)
			var page tracker.NativeCapacityPage
			decodeHubResponse(t, preview, &page)
			if len(page.Items) != 1 || page.Items[0].WorkItemID != tracker.NativeWorkItemID(goodItem) {
				t.Fatalf("one missing workspace source blocked preview: %+v", page)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential, request)
			requireNativeStatus(t, response, http.StatusOK)
			var claimed tracker.NativeLease
			decodeHubResponse(t, response, &claimed)
			if claimed.WorkItemID != tracker.NativeWorkItemID(goodItem) {
				t.Fatalf("workspace lane claimed=%s want=%s", claimed.WorkItemID, goodItem)
			}
			if err := original.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				cursor, err := r.SelectionCursor(t.Context(), f.runner.binding.RunnerID, scope.Project)
				if err != nil {
					return err
				}
				if cursor.Position.WorkItemID != tracker.NativeWorkItemID(badItem) {
					t.Fatalf("skipped workspace did not advance selection: %+v", cursor)
				}
				row, err := r.WorkItem(t.Context(), scope.Project, tracker.NativeWorkItemID(badItem))
				if err != nil {
					return err
				}
				_, err = r.ActiveLeaseForIssue(t.Context(), row.ID)
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("unavailable workspace acquired lease: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("ordinary candidates cannot crowd the workspace page", func(t *testing.T) {
		f := newWorkspaceRunnerFixture(t)
		heartbeat := func() {
			t.Helper()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 8, "version": minimumSupportedRunnerVersion, "backend_isolation": f.runner.redemption.BackendIsolation, "workspace_capabilities": workspacesession.Capabilities{Files: true, Diff: true, Exec: true}, "workspace_isolation": workspacesession.IsolationContainer, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.service.config.Clock()}}), http.StatusOK)
		}
		ordinary := []tracker.NativeWorkItemID{f.issue.WorkItemID}
		for index := range nativeClaimCandidatePageSize {
			ordinary = append(ordinary, f.create(t, fmt.Sprintf("Ordinary item %03d", index)).WorkItemID)
		}
		heartbeat()
		session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
		item := tracker.NativeWorkItemID(f.item(t, session.ID))
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			for index, id := range append(ordinary, item) {
				entry, err := w.QueueEntry(t.Context(), f.project.ID, id, string(f.project.ID))
				if err != nil {
					return err
				}
				entry.Priority, entry.Rank = new(int64(0)), fmt.Sprintf("%05d", index)
				if err := w.PutQueueEntry(t.Context(), entry); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		request := tracker.NativeClaim{PolicyID: f.policy, MachineID: f.runner.binding.MachineID, SessionID: hubapi.NewID("uncrowded-workspace"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			query := nativeTenantCandidateQuery(hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, request, "", f.service.config.Clock())
			query.WorkspaceLane, query.Limit = false, nativeClaimCandidatePageSize
			page, err := r.CandidatePage(t.Context(), query)
			if err != nil {
				return err
			}
			if len(page.Items) != nativeClaimCandidatePageSize || slices.ContainsFunc(page.Items, func(value claimstore.Candidate) bool { return value.WorkItemID == item }) {
				t.Fatalf("ordinary claim population=%+v", page)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		heartbeat()
		preview := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.runner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: request, Limit: nativeClaimCandidatePageSize})
		requireNativeStatus(t, preview, http.StatusOK)
		var page tracker.NativeCapacityPage
		decodeHubResponse(t, preview, &page)
		if len(page.Items) != 1 || page.Items[0].WorkItemID != item {
			t.Fatalf("ordinary population displaced workspace preview: %+v", page)
		}
		requireNativeStatus(t, f.ordinaryClaim(t, "", hubapi.NewID("ordinary-poll")), http.StatusOK)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential, request)
		requireNativeStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, response, &lease)
		if lease.WorkItemID != item {
			t.Fatalf("ordinary poll displaced workspace claim: %+v", lease)
		}
	})

}

// requireTerminal makes a requested workspace require a terminal. A terminal
// request needs a hosted session to create, so the row is changed directly:
// what is under test is the runner side, not who may ask for one.
func (f *workspaceRunnerFixture) requireTerminal(t *testing.T, id string) {
	t.Helper()
	f.setRequires(t, id, []string{workspacesession.CapabilityFiles, workspacesession.CapabilityDiff, workspacesession.CapabilityTerminal})
}

func (f *workspaceRunnerFixture) setRequires(t *testing.T, id string, requires []string) {
	t.Helper()
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(),
		`UPDATE workspace_sessions SET requires = $4 WHERE organization_id=$1 AND project_id=$2 AND id = $3`, f.project.OrganizationID, f.project.ID, id, requires); err != nil {
		t.Fatal(err)
	}
}

// reportRunner re-registers the fixture's machine with a capacity, surfaces
// and terminal isolation.
func (f *workspaceRunnerFixture) reportRunner(t *testing.T, capacity int, capabilities workspacesession.Capabilities, isolation string) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", f.runner.redemption.Credential,
		map[string]any{"id": f.runner.binding.MachineID, "hostname": "customer-host", "display_name": "Runner",
			"backend_isolation": f.runner.redemption.BackendIsolation, "capacity": capacity, "version": "test", "workspace_capabilities": capabilities,
			"workspace_isolation": isolation}), http.StatusOK)
}

// A workspace that requires a terminal is served only by a runner whose
// terminal is confined at least as tightly as the organization configured, and
// that is checked at the claim, the bind and every heartbeat.
func TestWorkspaceTerminalIsolationIsEnforced(t *testing.T) {
	t.Parallel()
	withTerminal := workspacesession.Capabilities{Files: true, Diff: true, Terminal: true}

	t.Run("the claim gate skips a user-isolation runner", func(t *testing.T) {
		t.Parallel()
		f := newWorkspaceRunnerFixture(t)
		f.reportRunner(t, 8, withTerminal, workspacesession.IsolationUser)
		session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
		f.requireTerminal(t, session.ID)
		item := f.item(t, session.ID)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
			tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
				SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
				Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
		if response.Code == http.StatusOK {
			t.Fatalf("a user-isolation runner claimed a terminal workspace in a container organization: %s", response.Body.String())
		}
		f.reportRunner(t, 8, withTerminal, workspacesession.IsolationContainer)
		f.claim(t, item)
	})

	for _, test := range []struct {
		name          string
		bindIsolation string
		beatIsolation string
		wantBind      int
		wantBeat      int
	}{
		{name: "a container bind and beat are accepted", bindIsolation: workspacesession.IsolationContainer,
			beatIsolation: workspacesession.IsolationContainer, wantBind: http.StatusOK, wantBeat: http.StatusOK},
		{name: "a beat that omits isolation keeps the bound level", bindIsolation: workspacesession.IsolationContainer,
			wantBind: http.StatusOK, wantBeat: http.StatusOK},
		{name: "a user bind is refused", bindIsolation: workspacesession.IsolationUser, wantBind: http.StatusForbidden},
		{name: "a bind reporting no isolation is refused", wantBind: http.StatusForbidden},
		{name: "a beat that switches to user is refused", bindIsolation: workspacesession.IsolationContainer,
			beatIsolation: workspacesession.IsolationUser, wantBind: http.StatusOK, wantBeat: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newWorkspaceRunnerFixture(t)
			f.reportRunner(t, 8, withTerminal, workspacesession.IsolationContainer)
			session := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
			f.requireTerminal(t, session.ID)
			lease := f.claim(t, f.item(t, session.ID))
			identity := workspaceIdentity(lease)
			bind := f.workerPost(t, session.ID, "bind", workspaceBindRequest{
				workspaceWorkerIdentity: identity, Capabilities: withTerminal, Isolation: test.bindIsolation})
			requireNativeStatus(t, bind, test.wantBind)
			if test.wantBind != http.StatusOK {
				if state := f.read(t, session.ID).State; state != workspacesession.StateRequested {
					t.Fatalf("a refused bind moved the workspace to %q", state)
				}
				return
			}
			beat := f.workerPost(t, session.ID, "heartbeat", workspaceHeartbeatRequest{
				workspaceWorkerIdentity: identity, State: workspacesession.StateReady,
				Capabilities: withTerminal, Isolation: test.beatIsolation})
			requireNativeStatus(t, beat, test.wantBeat)
			if isolation := f.read(t, session.ID).Isolation; isolation != workspacesession.IsolationContainer {
				t.Fatalf("stored isolation = %q, want container", isolation)
			}
		})
	}
}

// An unbind gives the runner's slot back in the same transaction, so a
// capacity-one runner can take its next job straight away rather than waiting
// out the lease TTL.
func TestWorkspaceUnbindReleasesTheLease(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{workspacesession.ReasonRunnerRestarted, workspacesession.ReasonClosedByActor, ""} {
		t.Run("reason "+reason, func(t *testing.T) {
			t.Parallel()
			f := newWorkspaceRunnerFixture(t)
			f.reportRunner(t, 1, workspaceTestCapabilities, workspacesession.IsolationContainer)
			bind, lease := f.bound(t)
			second := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
			secondItem := f.item(t, second.ID)
			held := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
				tracker.NativeClaim{PolicyID: f.policy, WorkItemID: tracker.NativeWorkItemID(secondItem), MachineID: f.runner.binding.MachineID,
					SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
					Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
			if held.Code == http.StatusOK {
				t.Fatal("a capacity-one runner holding a workspace claimed a second one")
			}
			requireNativeStatus(t, f.workerPost(t, bind.Session.ID, "unbind", workspaceUnbindRequest{
				workspaceWorkerIdentity: workspaceIdentity(lease), Reason: reason}), http.StatusNoContent)
			if _, released := f.leaseWindow(t, lease.ID); !released {
				t.Fatal("the unbound workspace's lease was not released")
			}
			f.claim(t, secondItem)
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
		Models: []string{"gpt-6-astra"}, MaxConcurrent: 4, Availability: "available", ObservedAt: f.service.config.Clock().UTC()}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential,
		map[string]any{"display_name": "Runner", "capacity": capacity, "version": "test", "backend_isolation": f.runner.redemption.BackendIsolation,
			"provider_reports": []providercapacity.Report{report}}), http.StatusOK)
}

// reservations counts the provider reservations written against a lease.
func (f workspaceFixture) reservations(t *testing.T, lease tracker.LeaseID) int {
	t.Helper()
	rows := 0
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ProviderReservation(t.Context(), lease)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err == nil {
			rows = 1
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestWorkspaceClaimTakesCapacityNotAProviderReservation is the September 12
// fourth dogfood run's first observation. The runner's workspace lane was
// running and was being offered workspace items, and every claim it made was
// refused 409 provider_incompatible, "Work item needs local model selection
// before a provider reservation can be claimed".
//
// A workspace session serves files over the relay and runs no model. The workspace session contract
// gives its claim one slot of the runner's capacity and a workspace lease
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
	// be admitted against one remaining slot.
	second := f.opened(t, f.token, map[string]any{"work_item_id": string(f.issue.WorkItemID)})
	failure := requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims",
		f.runner.redemption.Credential, tracker.NativeClaim{PolicyID: f.policy,
			WorkItemID: tracker.NativeWorkItemID(f.item(t, second.ID)), MachineID: f.runner.binding.MachineID,
			SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
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
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
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
			issue, err := f.service.readWorkItem(t.Context(), scope, item)
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
			closed, err := f.service.readWorkItem(t.Context(), scope, item)
			if err != nil {
				t.Fatal(err)
			}
			if !closed.Terminal {
				t.Fatalf("the workspace item stayed in %q, want a terminal state", closed.State)
			}
			// Terminal is what a person reading the board sees; the candidate
			// query reads dispatchable. Neither lane reaches it again.
			if claimed := f.ordinaryClaim(t, item, hubapi.NewID("ordinary")); claimed.Code == http.StatusOK {
				t.Fatalf("the issue lane claimed a closed workspace item: %s", claimed.Body.String())
			}
			if claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims",
				f.runner.redemption.Credential, tracker.NativeClaim{PolicyID: f.policy,
					WorkItemID: tracker.NativeWorkItemID(item), MachineID: f.runner.binding.MachineID,
					SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2,
					Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability},
				}); claimed.Code == http.StatusOK {
				t.Fatalf("the workspace lane claimed a closed workspace item: %s", claimed.Body.String())
			}
		})
	}
}
