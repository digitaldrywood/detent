package runnerruntime_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type terminalRelayFixture struct {
	project   hubfixture.Native
	server    *httptest.Server
	runner    hubfixture.Runner
	workspace string
	lease     tracker.NativeLease
}

func newTerminalRelayFixture(t *testing.T) terminalRelayFixture {
	t.Helper()
	clock := hubtest.NewClock(time.Now())
	project := hubfixture.NewDefaultNative(t, hubfixture.Config{
		Clock: clock.Now, Workspace: &workspacehub.Config{Enabled: true, Terminal: workspacehub.TerminalConfig{Enabled: true}},
	})
	hub := project.Hub
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)
	runner := hubfixture.PrepareRunner(t, project, hubtest.RunnerOperations()...)
	runner.Enroll(t)
	capabilities := workspacesession.Capabilities{Files: true, Diff: true, Terminal: true}
	response := hub.Request(t, http.MethodPost, project.Base+"/machines/"+string(runner.Binding.MachineID)+"/heartbeat", runner.Redemption.Credential, map[string]any{
		"display_name": "runner", "capacity": 2, "version": "test", "backend_isolation": runner.Redemption.BackendIsolation,
		"workspace_capabilities": capabilities, "workspace_isolation": workspacesession.IsolationContainer,
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: clock.Now()},
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)

	issue := project.Create(t, "Terminal subject")
	workspace := openWorkspaceRoute(t, project, string(issue.WorkItemID))
	var candidates claimstore.CandidatePage
	if err := hub.Repository.ReadTenant(t.Context(), project.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		candidates, err = reader.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: project.Project.ID, WorkspaceLane: true, AvailableAt: clock.Now(), Limit: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(candidates.Items) == 0 {
		t.Fatal("workspace has no dispatch candidate")
	}
	response = hub.Request(t, http.MethodPost, project.Base+"/claims", runner.Redemption.Credential, tracker.NativeClaim{
		PolicyID: hubfixture.Policy().ID, WorkItemID: candidates.Items[0].WorkItemID, MachineID: runner.Binding.MachineID,
		SessionID: "terminal-relay", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability},
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	identity := hubclient.WorkspaceIdentity{LeaseID: lease.ID, FencingToken: lease.FencingToken}
	worker := project.Base + "/workspaces/" + workspace.ID + "/worker"
	response = hub.Request(t, http.MethodPost, worker+"/bind", runner.Redemption.Credential, hubclient.WorkspaceBindRequest{
		WorkspaceIdentity: identity, Capabilities: capabilities, Isolation: workspacesession.IsolationContainer,
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	response = hub.Request(t, http.MethodPost, worker+"/heartbeat", runner.Redemption.Credential, hubclient.WorkspaceHeartbeatRequest{
		WorkspaceIdentity: identity, State: workspacesession.StateReady, HeadSHA: "abc123", Capabilities: capabilities, Isolation: workspacesession.IsolationContainer,
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	return terminalRelayFixture{project: project, server: server, runner: runner, workspace: workspace.ID, lease: lease}
}

func (f terminalRelayFixture) dialPerson(t *testing.T) *workspaceRelayClient {
	t.Helper()
	person := dialWorkspacePerson(t, f.project, f.server, f.workspace)
	person.awaitServing()
	return person
}

func (f terminalRelayFixture) dialRunner(t *testing.T) *workspaceRelayClient {
	t.Helper()
	url := "ws" + strings.TrimPrefix(f.server.URL, "http") + f.project.Base + "/workspaces/" + f.workspace + "/worker/relay"
	runner := dialWorkspaceSocket(t, url, http.Header{
		"Authorization":          {"Bearer " + f.runner.Redemption.Credential},
		"X-Detent-Lease":         {string(f.lease.ID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(f.lease.FencingToken), 10)},
	})
	runner.awaitServing()
	return runner
}

func terminalFrameOf(t *testing.T, kind, stream string, payload any) workspacesession.Frame {
	t.Helper()
	encoded, err := workspacesession.Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{
		Channel: workspacesession.ChannelTerminal, Stream: stream, Type: kind, Payload: encoded,
	}
}

func terminalOutputFrame(t *testing.T, stream, data string) workspacesession.Frame {
	t.Helper()
	return terminalFrameOf(t, workspacesession.TypeTerminalOutput, stream, workspacesession.TerminalOutput{
		Data: base64.StdEncoding.EncodeToString([]byte(data)), Encoding: workspacesession.EncodingBase64,
	})
}

func terminalOpenFrame() workspacesession.Frame {
	payload, err := workspacesession.Encode(map[string]any{"cols": 80, "rows": 24})
	if err != nil {
		panic(err)
	}
	return workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Type: workspacesession.TypeOpen, Payload: payload}
}

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

// TestWorkspaceRelayCarriesATerminalSession walks one whole session over the
// relay: the open the hub allocates a stream for, the input it forwards to the
// runner, the resize, the output that comes back, the exit and the close that
// gives the stream's slot back.
func TestWorkspaceRelayCarriesATerminalSession(t *testing.T) {
	t.Parallel()
	f := newTerminalRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	opened := runner.receive()
	if opened.Type != workspacesession.TypeOpen || opened.Stream == "" {
		t.Fatalf("open forwarded as %+v, want an open on an allocated stream", opened)
	}
	// The actor is the hub's stamp and never the client's, so a runner can
	// audit what it was asked to do and by whom.
	if opened.Actor == nil || opened.Actor.ConnectionID == "" {
		t.Fatalf("the open carried actor %+v, want the hub's stamp", opened.Actor)
	}
	stream := opened.Stream

	runner.send(terminalFrameOf(t, workspacesession.TypeTerminalOpened, stream,
		workspacesession.TerminalOpened{PID: 4242, Isolation: workspacesession.IsolationContainer, Cols: 80, Rows: 24}))
	if answer := person.receive(); answer.Type != workspacesession.TypeTerminalOpened || answer.Stream != stream {
		t.Fatalf("opened answered %+v, want it on %q", answer, stream)
	}

	person.send(terminalFrameOf(t, workspacesession.TypeTerminalInput, stream,
		workspacesession.TerminalInput{Data: "echo hi\n"}))
	forwarded := runner.receive()
	if forwarded.Type != workspacesession.TypeTerminalInput || forwarded.Stream != stream {
		t.Fatalf("input forwarded as %+v", forwarded)
	}

	person.send(terminalFrameOf(t, workspacesession.TypeTerminalResize, stream,
		workspacesession.TerminalResize{Cols: 132, Rows: 43}))
	if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeTerminalResize {
		t.Fatalf("resize forwarded as %+v", forwarded)
	}

	runner.send(terminalOutputFrame(t, stream, "hi\r\n"))
	answer := person.receive()
	if answer.Type != workspacesession.TypeTerminalOutput {
		t.Fatalf("output answered %+v", answer)
	}
	var span workspacesession.TerminalOutput
	if err := json.Unmarshal(answer.Payload, &span); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(span.Data)
	if err != nil || string(decoded) != "hi\r\n" {
		t.Fatalf("output span = %+v (%q), want the shell's bytes", span, decoded)
	}

	runner.send(terminalFrameOf(t, workspacesession.TypeTerminalExit, stream,
		workspacesession.TerminalExit{Code: 0}))
	if answer := person.receive(); answer.Type != workspacesession.TypeTerminalExit {
		t.Fatalf("exit answered %+v", answer)
	}
	runner.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: stream, Type: workspacesession.TypeClosed})
	if answer := person.receive(); answer.Type != workspacesession.TypeClosed {
		t.Fatalf("closed answered %+v", answer)
	}
}

// TestWorkspaceRelayNeverSharesATerminalStream is the relay protocol's rule in so
// many words: a second tab that wants the same shell opens its own stream and
// gets its own PTY, and neither connection ever sees the other's frames.
func TestWorkspaceRelayNeverSharesATerminalStream(t *testing.T) {
	t.Parallel()
	f := newTerminalRelayFixture(t)
	runner := f.dialRunner(t)
	first := f.dialPerson(t)
	second := f.dialPerson(t)

	first.send(terminalOpenFrame())
	firstStream := runner.receive().Stream
	second.send(terminalOpenFrame())
	secondStream := runner.receive().Stream
	if firstStream == secondStream || firstStream == "" || secondStream == "" {
		t.Fatalf("two opens shared stream %q", firstStream)
	}
	// Two opens on one connection allocate twice as well: every open is its
	// own PTY, so terminal never reuses the way files and diff do.
	first.send(terminalOpenFrame())
	firstAgain := runner.receive().Stream
	if firstAgain == firstStream {
		t.Fatalf("a second open on one connection reused stream %q", firstStream)
	}

	runner.send(terminalOutputFrame(t, firstStream, "only mine"))
	answer := first.receive()
	if answer.Stream != firstStream {
		t.Fatalf("output arrived on %q, want %q", answer.Stream, firstStream)
	}
	// The second connection sees nothing of the first's: its own stream is
	// what it reads, and a broadcast would be one person reading another's
	// shell.
	runner.send(terminalOutputFrame(t, secondStream, "only theirs"))
	if answer := second.receive(); answer.Stream != secondStream {
		t.Fatalf("the second connection read %+v, want its own stream", answer)
	}
}

// TestWorkspaceRelayRefusesAnUnknownTerminalFrame keeps the channel's
// vocabulary in front of stream allocation, so a client sending typos cannot
// spend its whole stream budget on them.
func TestWorkspaceRelayRefusesAnUnknownTerminalFrame(t *testing.T) {
	t.Parallel()
	f := newTerminalRelayFixture(t)
	f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Type: "detach"})
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeUnknownFrame {
		t.Fatalf("an unknown terminal frame answered %+v, want unknown_frame", payload)
	}
}

// TestWorkspaceRelayRefusesAnUnreadableTerminalOpen keeps the open's shape in
// front of stream allocation, beside its vocabulary.
//
// The hub reads this payload itself --- it is what a recording's header declares
// as the window --- so an open it cannot parse is one it must
// refuse rather than forward: forwarding would spend a stream and start a
// recording claiming a window nobody asked for.
func TestWorkspaceRelayRefusesAnUnreadableTerminalOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "a payload that is not an object", payload: json.RawMessage(`"eighty by twenty-four"`)},
		{name: "a window past the winsize field", payload: json.RawMessage(`{"cols":99999,"rows":24}`)},
	}

	f := newTerminalRelayFixture(t)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			person.send(workspacesession.Frame{
				Channel: workspacesession.ChannelTerminal,
				Type:    workspacesession.TypeOpen,
				Payload: tt.payload,
			})
			if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeInvalidFrame {
				t.Fatalf("an unreadable open answered %+v, want invalid_frame", payload)
			}
		})
	}
	person.send(terminalOpenFrame())
	if forwarded := runner.receive(); forwarded.Type != workspacesession.TypeOpen {
		t.Fatalf("the open after a refusal forwarded as %+v", forwarded)
	}
}
