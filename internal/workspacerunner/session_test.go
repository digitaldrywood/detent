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
	"runtime"
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
	// actions is the run-on-worktree-creation set bind hands back.
	actions []workspacesession.Action
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
		Actions:  h.actions,
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

func (w *fixedWorktree) Prepare(context.Context, hubclient.WorkspaceCheckout) (string, error) {
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
	return startSessionOver(t, root, checkout, nil, configure)
}

// startSessionWithActions starts a session over a checkout and an action set
// the test shaped, which is what the run-on-worktree-creation cases need: the
// actions arrive with the bind and run before the workspace is ever reported
// ready.
//
// It takes a whole checkout where startSessionWith takes an adjustment,
// because those cases build one from creationCheckout rather than amending the
// default. Both are thin over startSessionOver, so there is one place a session
// is actually started.
func startSessionWithActions(
	t *testing.T,
	checkout hubclient.WorkspaceCheckout,
	actions []workspacesession.Action,
	configure func(*workspacerunner.Config),
) *sessionFixture {
	t.Helper()
	return startSessionOver(t, worktreeWith(t), checkout, actions, configure)
}

// startSessionOver is the one place a fixture session is started.
func startSessionOver(
	t *testing.T,
	root string,
	checkout hubclient.WorkspaceCheckout,
	actions []workspacesession.Action,
	configure func(*workspacerunner.Config),
) *sessionFixture {
	t.Helper()
	hub := newScriptedHub(t, checkout)
	hub.actions = actions
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
	f := startSession(t, nil)
	f.send(t, workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: "conn:1", Type: workspacesession.TypeOpen})
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	// A runner that cannot serve a channel says so rather than going silent:
	// an unanswered request is indistinguishable from a slow one.
	if payload.Code != workspacesession.CodeUnsupported {
		t.Fatalf("terminal channel answered %+v", payload)
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
			if !capabilities.Exec {
				t.Fatal("the exec channel is served, so it must be reported")
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

// The exec channel (section 18.12). A run is not request/response, so what the
// tests below assert is the shape of a stream -- output spans, then one exited
// frame, then the closed frame that gives the stream's slot back -- and what
// stops a process that should not still be running.

// requirePOSIXShell skips a test whose action command is POSIX shell syntax.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the action command is POSIX shell syntax")
	}
}

func execRunFrame(t *testing.T, stream, command string) workspacesession.Frame {
	t.Helper()
	payload, err := workspacesession.Encode(workspacesession.ExecRun{
		RunID: "run_1", ActionID: "act_1", Command: command,
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{
		Channel: workspacesession.ChannelExec, Stream: stream,
		Type: workspacesession.TypeExecRun, Payload: payload,
	}
}

// receiveUntil reads frames until one of kind arrives and reports all of them,
// because what a run has to get right is the order they came in.
func (f *sessionFixture) receiveUntil(t *testing.T, kind string) []workspacesession.Frame {
	t.Helper()
	frames := make([]workspacesession.Frame, 0, 8)
	for range 512 {
		frame := f.receive(t)
		frames = append(frames, frame)
		if frame.Type == kind {
			return frames
		}
	}
	t.Fatalf("never saw a %s frame in %d frames", kind, len(frames))
	return nil
}

// execOutput reassembles the output frames of a run.
func execOutput(t *testing.T, frames []workspacesession.Frame) string {
	t.Helper()
	var out strings.Builder
	for _, frame := range frames {
		if frame.Type != workspacesession.TypeExecOutput {
			continue
		}
		var span workspacesession.ExecOutput
		if err := json.Unmarshal(frame.Payload, &span); err != nil {
			t.Fatal(err)
		}
		out.WriteString(span.Data)
	}
	return out.String()
}

// execExited reports the one exited frame a run ends with.
func execExited(t *testing.T, frames []workspacesession.Frame) workspacesession.ExecExited {
	t.Helper()
	for _, frame := range frames {
		if frame.Type != workspacesession.TypeExecExited {
			continue
		}
		var exited workspacesession.ExecExited
		if err := json.Unmarshal(frame.Payload, &exited); err != nil {
			t.Fatal(err)
		}
		return exited
	}
	t.Fatalf("no exited frame in %d frames", len(frames))
	return workspacesession.ExecExited{}
}

func TestSessionRunsAnActionOnTheExecChannel(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	f := startSession(t, nil)

	f.send(t, execRunFrame(t, "conn:1", "printf ready; printf oops >&2"))
	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if len(frames) < 3 {
		t.Fatalf("a run answered %d frames, want output, exited and closed", len(frames))
	}
	// One ordered stream: stderr is interleaved with stdout the way the shell
	// interleaved it, so a reader sees what a terminal would have shown.
	if got := execOutput(t, frames); !strings.Contains(got, "ready") || !strings.Contains(got, "oops") {
		t.Fatalf("output = %q, want both streams", got)
	}
	if exited := execExited(t, frames); exited.Code != 0 || exited.Signal != "" {
		t.Fatalf("exited = %+v, want a clean exit", exited)
	}
	// The hub releases a stream's slot on close or closed and never on exited,
	// so a run that stopped at exited would leak one slot each time until the
	// workspace refused the next run.
	last := frames[len(frames)-1]
	if last.Type != workspacesession.TypeClosed || last.Stream != "conn:1" {
		t.Fatalf("the last frame was %+v, want closed on the run's own stream", last)
	}
	for index, frame := range frames {
		if frame.Channel != workspacesession.ChannelExec || frame.Stream != "conn:1" {
			t.Fatalf("frame %d = %+v, want it on the exec channel's own stream", index, frame)
		}
	}
}

func TestSessionAnswersAnUnknownExecFrame(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	f.send(t, workspacesession.Frame{Channel: workspacesession.ChannelExec, Stream: "conn:1", Type: "cancel"})
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeUnknownFrame {
		t.Fatalf("unknown exec frame answered %+v", payload)
	}
}

// TestSessionRefusesARunAfterLeaseLoss is the files path's rule applied to the
// one thing on this surface that outlives the frame that asked for it: the
// lease is validated immediately before the process starts, so a run under a
// lost lease never becomes a process at all.
func TestSessionRefusesARunAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	expired := time.Now()
	f := startSession(t, func(config *workspacerunner.Config) {
		config.Now = func() time.Time { return expired }
	})
	witness := filepath.Join(f.worktree.path, "witness")
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)

	f.send(t, execRunFrame(t, "conn:1", "printf ran > "+witness))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("a run after lease loss answered %+v, want stale_execution", payload)
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the command never to have run", err)
	}
}

// TestSessionStopsARunWhenTheStreamIsClosed covers the process the person
// stopped: the close ends their stream and the whole process group goes with
// it, not the shell alone -- the grandchild here is what an installer or a
// build server would be.
func TestSessionStopsARunWhenTheStreamIsClosed(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	f := startSession(t, nil)
	witness := filepath.Join(f.worktree.path, "witness")

	f.send(t, execRunFrame(t, "conn:1", "printf started; sh -c 'sleep 2; : > "+witness+"' & sleep 30"))
	if frame := f.receive(t); frame.Type != workspacesession.TypeExecOutput {
		t.Fatalf("the run's first frame was %+v, want output", frame)
	}
	f.send(t, workspacesession.Frame{
		Channel: workspacesession.ChannelExec, Stream: "conn:1", Type: workspacesession.TypeClose,
	})
	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if exited := execExited(t, frames); exited.Code == 0 {
		t.Fatalf("exited = %+v, want a killed run to say so", exited)
	}

	// Past the grandchild's own sleep: if the group had survived the close, the
	// witness would be there by now.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the process group to have died with the stream", err)
	}
}

func TestSessionRefusesASecondRunOnAStream(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	f := startSession(t, nil)

	f.send(t, execRunFrame(t, "conn:1", "printf started; sleep 30"))
	if frame := f.receive(t); frame.Type != workspacesession.TypeExecOutput {
		t.Fatalf("the run's first frame was %+v, want output", frame)
	}
	f.send(t, execRunFrame(t, "conn:1", "printf second"))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	// One stream is one run: two runs interleaved on one stream would leave the
	// person with a single exited frame to explain both of them.
	if answer.Type != workspacesession.TypeError || payload.Code != workspacesession.CodeInvalidFrame {
		t.Fatalf("a second run answered %+v / %+v, want invalid_frame", answer, payload)
	}
}

// A run the hub dispatched itself (section 18.12). The eighth dogfood run
// queued one through the API with no browser attached and it never executed:
// the process only started when a person opened the exec channel for it, and
// this session's lane runs a project's actions at worktree creation and never
// again. The hub now hands such a run to the runner on the socket the runner is
// already holding, and these are the tests that say the session serves it under
// the same rules a person's run gets -- whoever opened the stream it arrives
// on.
//
// The stream id is the whole difference on this side. A hub stream is
// "relayhub:N" where a person's is their connection id; the session does not
// read the owner, and that is the property being pinned down, because a session
// that treated a person's stream as special would leave every headless run
// exactly as stuck as it was.

func TestSessionRunsAnActionTheHubDispatched(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	f := startSession(t, nil)

	f.send(t, execRunFrame(t, "relayhub:1", "printf ready; printf oops >&2"))
	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if got := execOutput(t, frames); !strings.Contains(got, "ready") || !strings.Contains(got, "oops") {
		t.Fatalf("output = %q, want both streams of a hub-dispatched run", got)
	}
	if exited := execExited(t, frames); exited.Code != 0 || exited.Signal != "" {
		t.Fatalf("exited = %+v, want a clean exit", exited)
	}
	// The stream's slot goes back on closed and never on exited, exactly as it
	// does for a person's run: the hub counts its own streams against the
	// workspace's budget, so a leaked one would refuse the next dispatch.
	last := frames[len(frames)-1]
	if last.Type != workspacesession.TypeClosed || last.Stream != "relayhub:1" {
		t.Fatalf("the last frame was %+v, want closed on the hub's own stream", last)
	}
}

// TestSessionRefusesAHubDispatchedRunAfterLeaseLoss is the lease rule on the
// path that has no person behind it. A dispatched run is still a command in a
// worktree, so it is validated immediately before the process starts and never
// executed under a lease this runner no longer holds.
func TestSessionRefusesAHubDispatchedRunAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	expired := time.Now()
	f := startSession(t, func(config *workspacerunner.Config) {
		config.Now = func() time.Time { return expired }
	})
	witness := filepath.Join(f.worktree.path, "witness")
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)

	f.send(t, execRunFrame(t, "relayhub:1", "printf ran > "+witness))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("a dispatched run after lease loss answered %+v, want stale_execution", payload)
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the command never to have run", err)
	}
}

// TestSessionStopsAHubDispatchedRunWhenTheLeaseIsLost covers the half a
// pre-start check cannot: a run outlives the frame that asked for it, and a
// dispatched run has no person whose closing tab would end it. The lease is
// re-checked while the process runs, and the whole process group goes when it
// fails -- the grandchild here is what a build server would be.
func TestSessionStopsAHubDispatchedRunWhenTheLeaseIsLost(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	var mu sync.Mutex
	now := time.Now()
	// The heartbeat is pushed out of the way rather than left at a second: a
	// successful heartbeat renews the lease from the runner's own clock, so a
	// loop ticking beside the clock this test moves would keep handing the
	// lease back and the run would never be stopped by anything.
	f := startSessionWith(t, worktreeWith(t), func(checkout *hubclient.WorkspaceCheckout) {
		checkout.HeartbeatSeconds = 3600
	}, func(config *workspacerunner.Config) {
		config.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
	})
	witness := filepath.Join(f.worktree.path, "witness")

	f.send(t, execRunFrame(t, "relayhub:1", "printf started; sh -c 'sleep 3; : > "+witness+"' & sleep 30"))
	if frame := f.receive(t); frame.Type != workspacesession.TypeExecOutput {
		t.Fatalf("the run's first frame was %+v, want output", frame)
	}
	mu.Lock()
	now = now.Add(workspacesession.LeaseTTL + time.Minute)
	mu.Unlock()

	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if exited := execExited(t, frames); exited.Code == 0 {
		t.Fatalf("exited = %+v, want a killed run to say so", exited)
	}
	// Past the grandchild's own sleep: if the group had survived the lease
	// loss, the witness would be there by now and a worktree this runner no
	// longer owns would still be being written to.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the process group to have died with the lease", err)
	}
}

// Run-on-worktree-creation (section 18.12). The set arrives with the bind and
// runs before the workspace is reported ready, so what these tests assert is
// what a person finds when their panel first opens.

// recordingReporter stands in for the hub's action-runs endpoint. It hands out
// a run id on a run's first report, the way the hub does when it creates the
// row.
type recordingReporter struct {
	mu      sync.Mutex
	reports []workspacerunner.ActionRun
	created int
}

func (r *recordingReporter) ReportActionRun(
	_ context.Context,
	workspaceID string,
	identity hubclient.WorkspaceIdentity,
	run workspacerunner.ActionRun,
) (workspacesession.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if workspaceID != testWorkspaceID || identity.LeaseID != tracker.LeaseID("lease-1") {
		// The report goes under the workspace lease and not under nothing: a
		// report the hub cannot fence is a report it has to refuse.
		return workspacesession.Run{}, fmt.Errorf("unfenced report for %q", workspaceID)
	}
	r.reports = append(r.reports, run)
	if run.RunID == "" {
		r.created++
		return workspacesession.Run{ID: fmt.Sprintf("run_%d", r.created)}, nil
	}
	return workspacesession.Run{ID: run.RunID}, nil
}

func (r *recordingReporter) snapshot() []workspacerunner.ActionRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]workspacerunner.ActionRun{}, r.reports...)
}

func creationCheckout(worktree string) hubclient.WorkspaceCheckout {
	return hubclient.WorkspaceCheckout{
		WorkItemID: "wi_1", Worktree: worktree, Requires: []string{"files"},
		HeartbeatSeconds: 1, IdleTimeoutSeconds: 1800,
	}
}

// TestSessionRunsFreshWorktreeActionsInOrder is why the set is run one at a
// time: the order is the author's meaning, and an author who wants install
// before build writes install first.
func TestSessionRunsFreshWorktreeActionsInOrder(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	reporter := &recordingReporter{}
	f := startSessionWithActions(t, creationCheckout(workspacesession.WorktreeFresh), []workspacesession.Action{
		{ID: "act_install", Name: "Install", Command: "printf install >> actions.log", RunOnWorktreeCreation: true},
		{ID: "act_build", Name: "Build", Command: "printf build >> actions.log", RunOnWorktreeCreation: true},
	}, func(config *workspacerunner.Config) { config.Reporter = reporter })

	logged, err := os.ReadFile(filepath.Join(f.worktree.path, "actions.log"))
	if err != nil {
		t.Fatalf("read the actions' own log: %v", err)
	}
	if string(logged) != "installbuild" {
		t.Fatalf("the actions wrote %q, want them run in authoring order", logged)
	}
	reports := reporter.snapshot()
	want := []struct {
		action string
		status string
	}{
		{action: "act_install", status: workspacesession.RunRunning},
		{action: "act_install", status: workspacesession.RunSucceeded},
		{action: "act_build", status: workspacesession.RunRunning},
		{action: "act_build", status: workspacesession.RunSucceeded},
	}
	if len(reports) != len(want) {
		t.Fatalf("reports = %+v, want %d of them", reports, len(want))
	}
	for index, expected := range want {
		if reports[index].ActionID != expected.action || reports[index].Status != expected.status {
			t.Fatalf("report %d = %+v, want %s %s", index, reports[index], expected.action, expected.status)
		}
	}
	// A run's first report carries no id, because the hub creates the row and
	// answers with one; every later report of that run carries it back.
	if reports[0].RunID != "" || reports[1].RunID != "run_1" || reports[3].RunID != "run_2" {
		t.Fatalf("run ids = %q, %q, %q; want the hub's own ids threaded through",
			reports[0].RunID, reports[1].RunID, reports[3].RunID)
	}
	if reports[1].ExitCode == nil || *reports[1].ExitCode != 0 {
		t.Fatalf("exit code = %v, want the command's own 0", reports[1].ExitCode)
	}
}

// TestSessionRunsNoActionsForARetainedWorktree is the other half of the rule:
// the setup already ran when this worktree was created, and running it again
// would redo that work on a tree someone may be reading.
func TestSessionRunsNoActionsForARetainedWorktree(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	reporter := &recordingReporter{}
	f := startSessionWithActions(t, creationCheckout(workspacesession.WorktreeRetained), []workspacesession.Action{
		{ID: "act_install", Name: "Install", Command: "printf install >> actions.log", RunOnWorktreeCreation: true},
	}, func(config *workspacerunner.Config) { config.Reporter = reporter })

	if _, err := os.Stat(filepath.Join(f.worktree.path, "actions.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat the actions' log = %v, want a retained worktree to run nothing", err)
	}
	if reports := reporter.snapshot(); len(reports) != 0 {
		t.Fatalf("reports = %+v, want none for a retained worktree", reports)
	}
}

// TestSessionReachesReadyWhenAnActionFails is the judgement a failed setup
// command gets: it is information, recorded with its code and its output, and
// not a reason to deny the reader the worktree they asked for.
func TestSessionReachesReadyWhenAnActionFails(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)
	reporter := &recordingReporter{}
	f := startSessionWithActions(t, creationCheckout(workspacesession.WorktreeFresh), []workspacesession.Action{
		{ID: "act_install", Name: "Install", Command: "printf boom >&2; exit 7", RunOnWorktreeCreation: true},
	}, func(config *workspacerunner.Config) { config.Reporter = reporter })

	reports := reporter.snapshot()
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want a running and a terminal one", reports)
	}
	terminal := reports[1]
	if terminal.Status != workspacesession.RunFailed {
		t.Fatalf("status = %q, want failed", terminal.Status)
	}
	if terminal.ExitCode == nil || *terminal.ExitCode != 7 {
		t.Fatalf("exit code = %v, want the command's own 7", terminal.ExitCode)
	}
	if !strings.Contains(terminal.Output, "boom") {
		t.Fatalf("output = %q, want what the command said", terminal.Output)
	}
	// The workspace still reached ready and still serves what it was opened
	// for: the checkout succeeded and the files are there.
	f.hub.mu.Lock()
	states := make([]string, 0, len(f.hub.heartbeats))
	for _, beat := range f.hub.heartbeats {
		states = append(states, beat.State)
	}
	f.hub.mu.Unlock()
	if len(states) == 0 || states[0] != workspacesession.StateReady {
		t.Fatalf("heartbeat states = %v, want the workspace to have reached ready", states)
	}
	f.send(t, filesFrame(t, workspacesession.TypeFilesList, "conn:1", workspacesession.FilesRequest{}))
	if answer := f.receive(t); answer.Type != workspacesession.TypeFilesListed {
		t.Fatalf("listing answered %+v, want the files channel still served", answer)
	}
}
