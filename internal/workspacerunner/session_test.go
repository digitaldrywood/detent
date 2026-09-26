package workspacerunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

// The session is tested against a scripted hub on a real WebSocket rather than
// by calling its methods: what it has to get right is the order of bind,
// heartbeat, serve and unbind, and a frame answered after a lost lease. None of
// that is visible from a method call.

const testWorkspaceID = "ws_00112233445566778899aabbccddeeff"

// scriptedHub stands in for the hub. It records what the session asked for and
// answers with whatever the test set up.
type scriptedHub struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	bindCalls int
	// binds records every bind request, because what the runner claims at bind
	// and what it narrows to on the first heartbeat are two different answers
	// and the difference is load-bearing (section 18.12).
	binds        []hubclient.WorkspaceBindRequest
	heartbeats   []hubclient.WorkspaceHeartbeatRequest
	unbindReason string
	unbound      bool
	// state is what the next heartbeat reports back, which is how a test asks
	// the session to close.
	state string
	// bindErr and heartbeatErr are injected failures.
	bindErr      error
	heartbeatErr error
	// checkout is what bind hands back.
	checkout hubclient.WorkspaceCheckout
	// accepted is closed once the runner's relay socket is connected.
	accepted chan *websocket.Conn
}

func newScriptedHub(t *testing.T, checkout hubclient.WorkspaceCheckout) *scriptedHub {
	t.Helper()
	hub := &scriptedHub{t: t, state: workspacesession.StateReady, checkout: checkout, accepted: make(chan *websocket.Conn, 4)}
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		socket.SetReadLimit(workspacesession.MaxFrameBytes + (16 << 10))
		hub.accepted <- socket
		// The handler must not return while the socket is in use, so it waits
		// for the request context, which the test cancels by closing the
		// server.
		<-r.Context().Done()
	}))
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *scriptedHub) BindWorkspace(_ context.Context, _ string, request hubclient.WorkspaceBindRequest) (hubclient.WorkspaceBindResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bindCalls++
	h.binds = append(h.binds, request)
	if h.bindErr != nil {
		return hubclient.WorkspaceBindResponse{}, h.bindErr
	}
	return hubclient.WorkspaceBindResponse{
		Checkout: h.checkout,
		Session:  workspacesession.Session{ID: testWorkspaceID, State: workspacesession.StateStarting},
	}, nil
}

func (h *scriptedHub) HeartbeatWorkspace(_ context.Context, _ string, request hubclient.WorkspaceHeartbeatRequest) (workspacesession.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heartbeats = append(h.heartbeats, request)
	if h.heartbeatErr != nil {
		return workspacesession.Session{}, h.heartbeatErr
	}
	return workspacesession.Session{ID: testWorkspaceID, State: h.state}, nil
}

func (h *scriptedHub) UnbindWorkspace(_ context.Context, _ string, request hubclient.WorkspaceUnbindRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unbound = true
	h.unbindReason = request.Reason
	return nil
}

func (h *scriptedHub) DialWorkspaceRelay(ctx context.Context, _ string, _ hubclient.WorkspaceIdentity) (*websocket.Conn, error) {
	socket, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http"), nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	socket.SetReadLimit(workspacesession.MaxFrameBytes + (16 << 10))
	return socket, nil
}

func (h *scriptedHub) setState(state string) {
	h.mu.Lock()
	h.state = state
	h.mu.Unlock()
}

func (h *scriptedHub) unbindWith() (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unbound, h.unbindReason
}

// waitForSocket returns the hub side of the runner's relay connection.
func (h *scriptedHub) waitForSocket() *websocket.Conn {
	h.t.Helper()
	select {
	case socket := <-h.accepted:
		return socket
	case <-time.After(10 * time.Second):
		h.t.Fatal("the session never opened a relay socket")
		return nil
	}
}

// fixedWorktree hands the session a directory the test prepared.
type fixedWorktree struct {
	path     string
	prepare  error
	released bool
	mu       sync.Mutex
}

func (w *fixedWorktree) Prepare(context.Context, string, hubclient.WorkspaceCheckout) (string, error) {
	if w.prepare != nil {
		return "", w.prepare
	}
	return w.path, nil
}

func (w *fixedWorktree) Release(context.Context, string, hubclient.WorkspaceCheckout) error {
	w.mu.Lock()
	w.released = true
	w.mu.Unlock()
	return nil
}

func (w *fixedWorktree) wasReleased() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.released
}

// worktreeWith builds a small tree for the session to serve.
func worktreeWith(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(root, "README.md"):          "# Detent\n",
		filepath.Join(root, "internal", "svc.go"): "package internal\n",
		filepath.Join(root, ".env"):               "TOKEN=secret\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// sessionFixture starts a session and gives the test the hub side of its relay.
//
// finished is closed rather than sent on, so both the test and the cleanup can
// wait for the same event: a channel that carried the result would let whoever
// read it first hide the session's exit from the other.
type sessionFixture struct {
	hub      *scriptedHub
	worktree *fixedWorktree
	socket   *websocket.Conn
	finished chan struct{}
	runErr   error
	cancel   context.CancelFunc
}

// wait blocks until the session has exited and reports what it returned.
func (f *sessionFixture) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-f.finished:
		return f.runErr
	case <-time.After(within):
		t.Fatal("the session did not stop")
		return nil
	}
}

func startSession(t *testing.T, configure func(*workspacerunner.Config)) *sessionFixture {
	t.Helper()
	return startSessionWith(t, worktreeWith(t), nil, configure)
}

// defaultCheckout is what a session binds with unless a test shapes it.
func defaultCheckout() hubclient.WorkspaceCheckout {
	return hubclient.WorkspaceCheckout{
		WorkItemID: "wi_1", Worktree: workspacesession.WorktreeFresh, Requires: []string{"files"},
		HeartbeatSeconds: 1, IdleTimeoutSeconds: 1800,
	}
}

// startSessionWith is startSession over a worktree the test built and a
// checkout it adjusted. The git channel needs both: its worktree has to be a
// real repository, and read_only is the whole difference between a person
// looking at one and a person changing it.
func startSessionWith(
	t *testing.T,
	root string,
	adjust func(*hubclient.WorkspaceCheckout),
	configure func(*workspacerunner.Config),
) *sessionFixture {
	t.Helper()
	checkout := defaultCheckout()
	if adjust != nil {
		adjust(&checkout)
	}
	return startSessionOver(t, root, checkout, configure)
}

// startSessionOver is the one place a fixture session is started.
func startSessionOver(
	t *testing.T,
	root string,
	checkout hubclient.WorkspaceCheckout,
	configure func(*workspacerunner.Config),
) *sessionFixture {
	t.Helper()
	hub := newScriptedHub(t, checkout)
	worktree := &fixedWorktree{path: root}
	config := workspacerunner.Config{
		WorkspaceID: testWorkspaceID,
		Identity:    hubclient.WorkspaceIdentity{LeaseID: tracker.LeaseID("lease-1"), FencingToken: 7},
		Hub:         hub, Worktree: worktree, Logger: discardLogger(), Hostname: "runner-host",
	}
	if configure != nil {
		configure(&config)
	}
	session, err := workspacerunner.New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	fixture := &sessionFixture{hub: hub, worktree: worktree, finished: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(fixture.finished)
		fixture.runErr = session.Run(ctx)
	}()
	fixture.socket = hub.waitForSocket()
	t.Cleanup(func() {
		cancel()
		select {
		case <-fixture.finished:
		case <-time.After(15 * time.Second):
			t.Error("the session did not stop when its context was cancelled")
		}
	})
	return fixture
}

func (f *sessionFixture) send(t *testing.T, frame workspacesession.Frame) {
	t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.socket.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func (f *sessionFixture) receive(t *testing.T) workspacesession.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, data, err := f.socket.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var frame workspacesession.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func filesFrame(t *testing.T, kind, stream string, request workspacesession.FilesRequest) workspacesession.Frame {
	t.Helper()
	payload, err := workspacesession.Encode(request)
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: stream, Type: kind, Payload: payload}
}

func TestSessionServesTheFilesChannel(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)

	f.send(t, filesFrame(t, workspacesession.TypeFilesList, "conn:1", workspacesession.FilesRequest{}))
	answer := f.receive(t)
	if answer.Type != workspacesession.TypeFilesListed || answer.Stream != "conn:1" {
		t.Fatalf("list answered %+v", answer)
	}
	var listed workspacesession.FilesListed
	if err := json.Unmarshal(answer.Payload, &listed); err != nil {
		t.Fatal(err)
	}
	names := map[string]workspacesession.FilesEntry{}
	for _, entry := range listed.Entries {
		names[entry.Name] = entry
	}
	if _, ok := names["README.md"]; !ok {
		t.Fatalf("listing = %+v", listed.Entries)
	}
	// The denylist travels with the surface, so a secret is listed and marked
	// rather than hidden or served.
	if entry, ok := names[".env"]; !ok || !entry.Denied {
		t.Fatalf(".env entry = %+v, want it listed and denied", entry)
	}

	f.send(t, filesFrame(t, workspacesession.TypeFilesRead, "conn:1", workspacesession.FilesRequest{Path: "internal/svc.go"}))
	answer = f.receive(t)
	if answer.Type != workspacesession.TypeFilesContent {
		t.Fatalf("read answered %+v", answer)
	}
	var content workspacesession.FilesContent
	if err := json.Unmarshal(answer.Payload, &content); err != nil {
		t.Fatal(err)
	}
	if content.Data != "package internal\n" {
		t.Fatalf("content = %q", content.Data)
	}
}

func TestSessionRefusesADeniedRead(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	f.send(t, filesFrame(t, workspacesession.TypeFilesRead, "conn:1", workspacesession.FilesRequest{Path: ".env"}))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if answer.Type != workspacesession.TypeError || payload.Code != workspacesession.CodeDenied {
		t.Fatalf("denied read answered %+v / %+v", answer, payload)
	}
	// The refusal says what happened without saying what is there: the reason
	// a path was refused can itself disclose the thing it protects.
	if strings.Contains(payload.Message, "secret") && strings.Contains(payload.Message, "TOKEN") {
		t.Fatalf("the refusal leaked the file's content: %q", payload.Message)
	}
}

func TestSessionAnswersAnUnknownFilesFrame(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	f.send(t, workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: "conn:1", Type: "write"})
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeUnknownFrame {
		t.Fatalf("unknown files frame answered %+v", payload)
	}
}

func TestSessionAnswersAChannelItDoesNotServe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame workspacesession.Frame
	}{
		{
			name:  "terminal without support",
			frame: workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: "conn:1", Type: workspacesession.TypeOpen},
		},
		{
			name:  "exec",
			frame: workspacesession.Frame{Channel: workspacesession.ChannelExec, Stream: "conn:1", Type: workspacesession.TypeExecRun},
		},
		{
			name:  "diff",
			frame: workspacesession.Frame{Channel: workspacesession.ChannelDiff, Stream: "conn:1", Type: workspacesession.TypeOpen},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := startSession(t, nil)
			f.send(t, test.frame)
			answer := f.receive(t)
			var payload workspacesession.ErrorPayload
			if err := json.Unmarshal(answer.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			// A runner that cannot serve a channel says so rather than going
			// silent: an unanswered request is indistinguishable from a slow one.
			if payload.Code != workspacesession.CodeUnsupported {
				t.Fatalf("%s channel answered %+v", test.frame.Channel, payload)
			}
		})
	}
}

func TestSessionAnswersAWatchWithUnsupported(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	f.send(t, filesFrame(t, workspacesession.TypeFilesWatch, "conn:1", workspacesession.FilesRequest{Path: ""}))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	// Section 18.4 makes watching optional and says the client refreshes on
	// focus instead. Claiming it would leave a reader believing a stale tree
	// is current.
	if payload.Code != workspacesession.CodeUnsupported {
		t.Fatalf("watch answered %+v", payload)
	}
}

// TestSessionDropsAFrameAfterLeaseLoss is the rule that makes it safe for a
// worktree to be served by exactly one generation at a time: the lease is
// validated immediately before acting, never once at the start of the session.
func TestSessionDropsAFrameAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	expired := time.Now()
	f := startSession(t, func(config *workspacerunner.Config) {
		// The clock jumps past the lease window the bind opened, so the very
		// next frame arrives under a lease this runner no longer holds.
		config.Now = func() time.Time { return expired }
	})
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)
	f.send(t, filesFrame(t, workspacesession.TypeFilesRead, "conn:1", workspacesession.FilesRequest{Path: "README.md"}))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("a frame after lease loss answered %+v, want stale_execution", payload)
	}
}

func TestSessionStopsWhenTheHubAsksForTheWorkspaceBack(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	f.hub.setState(workspacesession.StateClosing)
	if err := f.wait(t, 15*time.Second); err != nil {
		t.Fatalf("session ended with %v", err)
	}
	unbound, reason := f.hub.unbindWith()
	if !unbound || reason != workspacesession.ReasonClosedByActor {
		t.Fatalf("unbind = %v, %q", unbound, reason)
	}
	if !f.worktree.wasReleased() {
		t.Fatal("the worktree must be released when the session ends")
	}
}

func TestSessionStopsWhenTheHubReportsAStaleLease(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	f.hub.mu.Lock()
	f.hub.heartbeatErr = fmt.Errorf("%w: gone", hubclient.ErrStaleWorkspace)
	f.hub.mu.Unlock()
	f.wait(t, 15*time.Second)
	unbound, reason := f.hub.unbindWith()
	if !unbound || reason != workspacesession.ReasonLeaseLost {
		t.Fatalf("unbind = %v, %q, want lease_lost", unbound, reason)
	}
}

func TestSessionReportsCheckoutFailureWithoutServing(t *testing.T) {
	t.Parallel()
	hub := newScriptedHub(t, hubclient.WorkspaceCheckout{WorkItemID: "wi_1", HeartbeatSeconds: 1})
	worktree := &fixedWorktree{prepare: errors.New("no disk")}
	session, err := workspacerunner.New(workspacerunner.Config{
		WorkspaceID: testWorkspaceID,
		Identity:    hubclient.WorkspaceIdentity{LeaseID: tracker.LeaseID("lease-1"), FencingToken: 7},
		Hub:         hub, Worktree: worktree, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run(t.Context()); err == nil {
		t.Fatal("a failed checkout must fail the session")
	}
	unbound, reason := hub.unbindWith()
	if !unbound || reason != workspacesession.ReasonCheckoutFailed {
		t.Fatalf("unbind = %v, %q, want checkout_failed", unbound, reason)
	}
}

func TestCapabilitiesReportOnlyWhatIsServed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		support      workspacerunner.Support
		wantTerminal bool
	}{
		{
			name:         "the default support serves a terminal where the platform has a PTY",
			support:      workspacerunner.DefaultSupport(),
			wantTerminal: workspaceterminal.Supported,
		},
		{
			name:         "an operator who wants no terminal reports none",
			support:      workspacerunner.Support{},
			wantTerminal: false,
		},
		{
			// A caller cannot turn on a surface the build has no way to serve:
			// the platform is ANDed in, so a Windows runner asked for a
			// terminal still reports none rather than being handed workspaces
			// it would refuse frame by frame.
			name:         "a terminal asked for is still bounded by the platform",
			support:      workspacerunner.Support{Terminal: true},
			wantTerminal: workspaceterminal.Supported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			capabilities := workspacerunner.Capabilities(tt.support)
			if !capabilities.Files {
				t.Fatal("the files channel is what this slice serves")
			}
			if capabilities.Exec {
				t.Fatal("project actions are not served, so exec must not be reported")
			}
			if !capabilities.Git {
				t.Fatal("the git channel is served, so it must be reported")
			}
			if capabilities.Terminal != tt.wantTerminal {
				t.Fatalf("capabilities.Terminal = %v, want %v", capabilities.Terminal, tt.wantTerminal)
			}
			// Reporting a channel this runner cannot serve would have the hub
			// hand it workspaces it would then refuse frame by frame, which
			// looks broken; reporting nothing means the workspace fails with
			// no_runner and says so.
			if capabilities.Diff || capabilities.Preview {
				t.Fatalf("capabilities = %+v, want no diff or preview until those channels exist", capabilities)
			}
		})
	}
}
