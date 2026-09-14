package workspacerunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The September 12 fifth dogfood defect, from the runner's side: nothing on the
// workspace path renewed the hub lease, so a Files session lived exactly one
// lease_ttl_seconds from the claim and then reported revoked to the person.
// Section 18.1 puts the renewal on the heartbeat, which means the session's own
// heartbeat loop is what keeps the lease alive and no second call is involved.
//
// This is tested against a real HTTP hub rather than the scripted one: what has
// to hold is that the loop keeps reaching POST .../worker/heartbeat often
// enough, over the real client, that a hub enforcing a lease window never sees
// the window lapse. A scripted hub that simply answers every call cannot show
// that.

// leaseHub answers the workspace worker endpoints and keeps a lease the way the
// hub keeps one: a heartbeat inside the window renews it for another lease TTL,
// and a heartbeat after the window is refused with stale_execution.
//
// Its clock is its own, advanced one nominal heartbeat interval per beat, so a
// test crosses several 90-second lease windows in a few real seconds without
// waiting for any of them.
type leaseHub struct {
	server *httptest.Server

	mu        sync.Mutex
	clock     time.Time
	expiresAt time.Time
	renewals  int
	lapsed    bool
	state     string
	unbound   string
	beats     chan struct{}
	sockets   chan *websocket.Conn
}

func newLeaseHub(t *testing.T) *leaseHub {
	t.Helper()
	hub := &leaseHub{
		clock: time.Date(2026, 9, 12, 9, 27, 14, 0, time.UTC),
		state: workspacesession.StateReady,
		beats: make(chan struct{}, 64), sockets: make(chan *websocket.Conn, 4),
	}
	hub.server = httptest.NewServer(http.HandlerFunc(hub.serve))
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *leaseHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/worker/bind"):
		h.bind(w)
	case strings.HasSuffix(r.URL.Path, "/worker/heartbeat"):
		h.heartbeat(w, r)
	case strings.HasSuffix(r.URL.Path, "/worker/unbind"):
		h.unbind(w, r)
	case strings.HasSuffix(r.URL.Path, "/worker/relay"):
		h.relay(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *leaseHub) bind(w http.ResponseWriter) {
	h.mu.Lock()
	h.expiresAt = h.clock.Add(workspacesession.LeaseTTL)
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, hubclient.WorkspaceBindResponse{
		Checkout: hubclient.WorkspaceCheckout{
			WorkItemID: "wi_1", Worktree: workspacesession.WorktreeFresh, Requires: []string{"files"},
			// One second is the shortest interval a checkout can express, and
			// it is what makes the loop observable in real time; the hub's own
			// clock still moves a full nominal interval per beat.
			HeartbeatSeconds: 1, IdleTimeoutSeconds: 1800,
		},
		Session: workspacesession.Session{ID: testWorkspaceID, State: workspacesession.StateStarting},
	})
}

func (h *leaseHub) heartbeat(w http.ResponseWriter, r *http.Request) {
	var request hubclient.WorkspaceHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.clock = h.clock.Add(workspacesession.HeartbeatInterval)
	stale := request.FencingToken != testFencingToken || !h.clock.Before(h.expiresAt)
	if stale {
		h.lapsed = true
	} else {
		h.expiresAt = h.clock.Add(workspacesession.LeaseTTL)
		h.renewals++
	}
	state := h.state
	h.mu.Unlock()
	select {
	case h.beats <- struct{}{}:
	default:
	}
	if stale {
		writeJSON(w, http.StatusConflict, map[string]string{
			"code": "stale_execution", "message": "The workspace lease is no longer current"})
		return
	}
	writeJSON(w, http.StatusOK, hubclient.WorkspaceHeartbeatResponse{
		Session: workspacesession.Session{ID: testWorkspaceID, State: state}})
}

func (h *leaseHub) unbind(w http.ResponseWriter, r *http.Request) {
	var request hubclient.WorkspaceUnbindRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	now := h.clock
	current := !h.expiresAt.IsZero() && now.Before(h.expiresAt) && request.FencingToken == testFencingToken
	if current {
		h.unbound = request.Reason
	}
	h.mu.Unlock()
	if !current {
		// An unbind under a lease the hub no longer holds is exactly the
		// 409 the dogfood run saw; refusing it here is what would make an
		// orderly close visible as a failure.
		writeJSON(w, http.StatusConflict, map[string]string{
			"code": "stale_execution", "message": "The workspace lease is no longer current"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *leaseHub) relay(w http.ResponseWriter, r *http.Request) {
	socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	socket.SetReadLimit(workspacesession.MaxFrameBytes + (16 << 10))
	h.sockets <- socket
	<-r.Context().Done()
}

func (h *leaseHub) report() (renewals int, lapsed bool, unbound string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.renewals, h.lapsed, h.unbound
}

func (h *leaseHub) setState(state string) {
	h.mu.Lock()
	h.state = state
	h.mu.Unlock()
}

// awaitBeats blocks until the hub has answered count heartbeats.
func (h *leaseHub) awaitBeats(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(time.Duration(count+10) * time.Second)
	for range count {
		select {
		case <-h.beats:
		case <-deadline:
			t.Fatalf("the session sent fewer than %d heartbeats", count)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// testFencingToken is the workspace tuple's token in these tests.
const testFencingToken tracker.FencingToken = 7

// nativeClientFor points the real native client at the test hub.
func nativeClientFor(t *testing.T, server *httptest.Server) *hubclient.NativeClient {
	t.Helper()
	client, err := hubclient.New(hubclient.Config{
		URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	return native
}

// TestSessionHeartbeatKeepsTheHubLeaseAlive drives the session's own heartbeat
// loop against a hub that enforces a lease window, for longer than one window,
// and requires the hub never to have seen it lapse.
func TestSessionHeartbeatKeepsTheHubLeaseAlive(t *testing.T) {
	t.Parallel()
	hub := newLeaseHub(t)
	worktree := &fixedWorktree{path: worktreeWith(t)}
	session, err := workspacerunner.New(workspacerunner.Config{
		WorkspaceID: testWorkspaceID,
		Identity:    hubclient.WorkspaceIdentity{LeaseID: tracker.LeaseID("lease-1"), FencingToken: testFencingToken},
		Hub:         nativeClientFor(t, hub.server), Worktree: worktree, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { finished <- session.Run(ctx) }()

	// Four beats move the hub's clock two minutes past the bind, which is
	// more than one 90-second lease window: without the renewal the third
	// would already have been refused.
	hub.awaitBeats(t, 4)
	if renewals, lapsed, _ := hub.report(); lapsed || renewals < 4 {
		t.Fatalf("renewals = %d, lapsed = %v, want an unbroken run of renewals", renewals, lapsed)
	}

	// The surface still works after the window the defect ended it at.
	socket := <-hub.sockets
	relay := &sessionFixture{socket: socket}
	relay.send(t, filesFrame(t, workspacesession.TypeFilesList, "conn:1", workspacesession.FilesRequest{}))
	if answer := relay.receive(t); answer.Type != workspacesession.TypeFilesListed {
		t.Fatalf("list answered %+v after more than one lease TTL", answer)
	}

	// An orderly close then unbinds under a lease that is still the hub's
	// own, so the runner and the hub agree about how it ended.
	hub.setState(workspacesession.StateClosing)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the session returned %v", err)
		}
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the session did not stop after the hub asked for the workspace back")
	}
	cancel()
	if _, lapsed, unbound := hub.report(); lapsed || unbound != workspacesession.ReasonClosedByActor {
		t.Fatalf("lapsed = %v, unbind reason = %q, want a clean closed_by_actor unbind", lapsed, unbound)
	}
}

// TestSessionStopsWhenTheHubLeaseIsGone is the other half: a hub that does
// refuse the heartbeat ends the session rather than letting it keep serving a
// worktree under a dead generation.
func TestSessionStopsWhenTheHubLeaseIsGone(t *testing.T) {
	t.Parallel()
	hub := newLeaseHub(t)
	worktree := &fixedWorktree{path: worktreeWith(t)}
	session, err := workspacerunner.New(workspacerunner.Config{
		WorkspaceID: testWorkspaceID,
		// A token the hub does not recognise is the same refusal an expired
		// lease produces, and it needs no wall-clock wait to arrange.
		Identity: hubclient.WorkspaceIdentity{LeaseID: tracker.LeaseID("lease-1"), FencingToken: testFencingToken + 1},
		Hub:      nativeClientFor(t, hub.server), Worktree: worktree, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- session.Run(ctx) }()
	select {
	case err := <-finished:
		// The first heartbeat is the ready report, which is refused, so Run
		// reports the stale lease rather than pretending it opened.
		if !errors.Is(err, hubclient.ErrStaleWorkspace) {
			t.Fatalf("the session returned %v, want a stale workspace", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the session kept serving a workspace whose lease the hub refused")
	}
}
