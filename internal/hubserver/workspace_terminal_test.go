package hubserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"reflect"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The terminal channel on the hub.
//
// It is tested against real sockets for the reason the rest of the relay is:
// what the hub has to get right is what happens between two connections --- one
// PTY per stream, never shared, authority re-asked per frame, the exit and the
// close that give a stream back, and a resume that finds the same shell. None
// of that is visible from a handler call.

// terminalFrameOf builds one person-originated terminal frame.
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

// terminalOutputFrame is a runner's output span, base64 as the terminal contract
// requires: a PTY's bytes are text interleaved with escape sequences, and a
// span cut at a frame boundary routinely ends inside one.
func terminalOutputFrame(t *testing.T, stream, data string) workspacesession.Frame {
	t.Helper()
	return terminalFrameOf(t, workspacesession.TypeTerminalOutput, stream, workspacesession.TerminalOutput{
		Data: base64.StdEncoding.EncodeToString([]byte(data)), Encoding: workspacesession.EncodingBase64,
	})
}

// TestWorkspaceRelayCarriesATerminalSession walks one whole session over the
// relay: the open the hub allocates a stream for, the input it forwards to the
// runner, the resize, the output that comes back, the exit and the close that
// gives the stream's slot back.
func TestWorkspaceRelayCarriesATerminalSession(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
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
	f := newRelayFixture(t, withTerminal)
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
	f := newRelayFixture(t, withTerminal)
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

	f := newRelayFixture(t, withTerminal)
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

// TestWorkspaceRelayRefusesATerminalOnARunningAttempt is the workspace session contract's rule:
// a workspace on an attempt that is still running is read-only and refuses a
// terminal, so a person cannot type into a worktree the model is editing.
func TestWorkspaceRelayRefusesATerminalOnARunningAttempt(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	f.markReadOnly(t)
	f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeReadOnly {
		t.Fatalf("a terminal on a running attempt answered %+v, want read_only", payload)
	}
}

// TestWorkspaceRelayRefusesATerminalWhenTheSettingIsOff covers
// workspaces.terminal.enabled, which the terminal contract defaults to off. The runner
// here reports the capability and the workspace is not read-only: the setting
// is the only thing refusing.
func TestWorkspaceRelayRefusesATerminalWhenTheSettingIsOff(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminalReportedButDisabled)
	f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeForbidden {
		t.Fatalf("a terminal with the setting off answered %+v, want forbidden", payload)
	}
}

// TestWorkspaceRelayResumesATerminalWithinTheWindow is the relay protocol's resume
// applied to the one stream it was written for. A PTY survives its connection
// dropping for sixty seconds, so a person who comes back inside the window
// finds the same shell with the output they missed.
func TestWorkspaceRelayResumesATerminalWithinTheWindow(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	stream := runner.receive().Stream
	runner.send(terminalOutputFrame(t, stream, "before the drop"))
	if answer := person.receive(); answer.Stream != stream {
		t.Fatalf("output answered %+v", answer)
	}
	_ = person.socket.Close(websocket.StatusNormalClosure, "tab closed")
	waitFor(t, func() bool { return f.service.workspaces.relay.detachedCount(f.workspace) == 1 })

	// Inside the window the stream is parked rather than gone, and the runner
	// was never told to close it: its PTY is still there.
	f.advanceServing(t, workspacesession.ResumeWindow-time.Second)
	resumed := f.dialPerson(t)
	resumed.send(resumeFrame(t, workspacesession.ChannelTerminal, stream, 0))
	answer := resumed.receive()
	if answer.Type != workspacesession.TypeResumed || answer.Stream != stream {
		t.Fatalf("resume answered %+v, want the same stream back", answer)
	}
	// The replay is what the person missed, which for a terminal is the screen
	// they were looking at.
	replayed := resumed.receive()
	if replayed.Type != workspacesession.TypeTerminalOutput {
		t.Fatalf("replay = %+v, want the missed output", replayed)
	}
	// And the shell is still writable: an input on the resumed stream reaches
	// the same runner stream it did before.
	resumed.send(terminalFrameOf(t, workspacesession.TypeTerminalInput, stream,
		workspacesession.TerminalInput{Data: "still here\n"}))
	forwarded := runner.receive()
	if forwarded.Type != workspacesession.TypeTerminalInput || forwarded.Stream != stream {
		t.Fatalf("input after a resume forwarded as %+v", forwarded)
	}
}

// TestWorkspaceRelayLosesATerminalAfterTheWindow is the other half: past sixty
// seconds the stream is swept, the runner is told to close it, and a resume is
// refused. That close is what kills the PTY on the runner.
func TestWorkspaceRelayLosesATerminalAfterTheWindow(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	stream := runner.receive().Stream
	_ = person.socket.Close(websocket.StatusNormalClosure, "tab closed")
	waitFor(t, func() bool { return f.service.workspaces.relay.detachedCount(f.workspace) == 1 })

	f.advanceServing(t, workspacesession.ResumeWindow+time.Second)
	f.service.workspaces.relay.sweep(t.Context(), f.at())

	closed := runner.receive()
	if closed.Type != workspacesession.TypeClose || closed.Stream != stream {
		t.Fatalf("the sweep told the runner %+v, want a close on %q", closed, stream)
	}
	resumed := f.dialPerson(t)
	resumed.send(resumeFrame(t, workspacesession.ChannelTerminal, stream, 0))
	if payload := errorPayload(t, resumed.receive()); payload.Code != workspacesession.CodeResumeFailed {
		t.Fatalf("a resume past the window answered %+v, want resume_failed", payload)
	}
}

// TestWorkspaceRelayRecordsATerminalSession covers the terminal contract's recording:
// the asciicast holds what the shell wrote and what the person typed, marked so
// a reader can tell them apart, and the relay session row points at where it
// may be read.
func TestWorkspaceRelayRecordsATerminalSession(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	stream := runner.receive().Stream
	person.send(terminalFrameOf(t, workspacesession.TypeTerminalInput, stream,
		workspacesession.TerminalInput{Data: "whoami\n"}))
	runner.receive()
	runner.send(terminalOutputFrame(t, stream, "runner\r\n"))
	person.receive()
	person.send(terminalFrameOf(t, workspacesession.TypeTerminalResize, stream,
		workspacesession.TerminalResize{Cols: 132, Rows: 43}))
	runner.receive()

	// The close is what completes the recording: a closed stream carries no
	// more events.
	person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: stream, Type: workspacesession.TypeClose})
	person.receive()

	recordings := readTerminalRecordingsForTest(t, f, stream)
	if len(recordings) != 1 {
		t.Fatalf("recordings = %d, want one per terminal stream", len(recordings))
	}
	recording := recordings[0]
	if recording.Isolation != workspacesession.IsolationSandbox {
		t.Fatalf("recording isolation = %q, want the level the organization set", recording.Isolation)
	}
	lines := strings.Split(strings.TrimSpace(recording.Cast), "\n")
	if len(lines) < 4 {
		t.Fatalf("cast = %q, want a header and three events", recording.Cast)
	}
	var header workspacesession.AsciicastHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("cast header %q: %v", lines[0], err)
	}
	if header.Version != workspacesession.AsciicastVersion || header.Width != 80 || header.Height != 24 {
		t.Fatalf("cast header = %+v, want asciicast v2 at the opened window", header)
	}
	codes := map[string]string{}
	for _, line := range lines[1:] {
		var event []any
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event) != 3 {
			t.Fatalf("cast event %q: %v", line, err)
		}
		code, _ := event[1].(string)
		data, _ := event[2].(string)
		codes[code] = data
	}
	// Recording cannot remove what the person typed, so input
	// is kept and marked rather than dropped.
	if codes[workspacesession.AsciicastInput] != "whoami\n" {
		t.Fatalf("cast input = %q, want what the person typed", codes[workspacesession.AsciicastInput])
	}
	if codes[workspacesession.AsciicastOutput] != "runner\r\n" {
		t.Fatalf("cast output = %q, want what the shell wrote", codes[workspacesession.AsciicastOutput])
	}
	if codes[workspacesession.AsciicastResize] != "132x43" {
		t.Fatalf("cast resize = %q, want the new window", codes[workspacesession.AsciicastResize])
	}

	// The relay session row references the recording, which is what the terminal contract
	// asks for.
	var artifact string
	if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
		rows, err := reader.(postgres.RelayActionReader).RelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkspaceID: f.workspace, Limit: 2})
		if err == nil {
			for _, row := range rows {
				if row.RecordingArtifact != "" {
					artifact = row.RecordingArtifact
				}
			}
		}
		return err
	}); err != nil {
		t.Fatalf("read recording_artifact: %v", err)
	}
	if !strings.Contains(artifact, "/terminal-recordings?relay_session=") {
		t.Fatalf("recording_artifact = %q, want the listing for this connection", artifact)
	}
	t.Run("longer second teardown corrects the recording", func(t *testing.T) {
		readUsage := func() int64 {
			t.Helper()
			var used int64
			if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
				var err error
				used, err = reader.CollaborationBytes(t.Context())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return used
		}
		initialUsage := readUsage()
		state := &terminalStreamState{ID: hubapi.NewID("termrec"), WorkspaceID: f.workspace, OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID), RelaySessionID: recording.RelaySessionID, StreamID: recording.StreamID + "-correction", PrincipalID: recording.PrincipalID, Subject: recording.Subject, Isolation: recording.Isolation, SupportReason: recording.SupportReason, StartedAt: recording.StartedAt, Cols: recording.Cols, Rows: recording.Rows, Recording: workspacesession.NewRecording(recording.Cols, recording.Rows, recording.StartedAt)}
		state.Recording.Append(workspacesession.AsciicastOutput, "first output", recording.StartedAt)
		shorter := workspacesession.NewRecording(recording.Cols, recording.Rows, recording.StartedAt)
		shorter.Append(workspacesession.AsciicastOutput, "first output", recording.StartedAt)
		f.service.workspaces.finishTerminalRecording(t.Context(), state)
		state.Recording.Append(workspacesession.AsciicastOutput, "second output", recording.StartedAt.Add(time.Microsecond))
		f.service.workspaces.finishTerminalRecording(t.Context(), state)
		corrected := readTerminalRecordingsForTest(t, f, state.StreamID)
		if len(corrected) != 1 || corrected[0].Cast != state.Recording.Cast() || corrected[0].Bytes != int64(len(state.Recording.Cast())) || !strings.Contains(corrected[0].Cast, "second output") {
			t.Fatalf("longer teardown was not retained: %+v", corrected)
		}
		beforeUsage := readUsage()
		state.Recording = shorter
		f.service.workspaces.finishTerminalRecording(t.Context(), state)
		if used := readUsage(); used != beforeUsage {
			t.Fatalf("discarded recording charged bytes: got=%d want=%d", used, beforeUsage)
		}
		again := readTerminalRecordingsForTest(t, f, state.StreamID)
		if !reflect.DeepEqual(again, corrected) {
			t.Fatalf("shorter teardown replaced complete recording: got=%+v want=%+v", again, corrected)
		}
		if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(reader tenantstore.Reader) error {
			row, err := reader.(postgres.RelayActionReader).TerminalRecording(t.Context(), f.project.ID, state.ID)
			if err != nil {
				return err
			}
			used, err := reader.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			if row.CastBytes <= 0 || used != initialUsage+row.CastBytes {
				t.Fatalf("accepted recording charge=%d want=%d", used, initialUsage+row.CastBytes)
			}
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='workspace-terminal-recording' AND owner_id=$2", f.project.OrganizationID, actionstore.ArchiveOwnerID(f.project.ID, state.ID)).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatalf("corrective recording retained stale archive owners=%d want1", count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

}

// TestWorkspaceRelaySkipsRecordingWhenTheSettingIsOff covers
// workspaces.terminal.record: a terminal with it off is still a terminal, and
// it leaves nothing behind.
func TestWorkspaceRelaySkipsRecordingWhenTheSettingIsOff(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal, withoutRecording)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	stream := runner.receive().Stream
	runner.send(terminalOutputFrame(t, stream, "unrecorded"))
	person.receive()
	person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: stream, Type: workspacesession.TypeClose})
	person.receive()

	if recordings := readTerminalRecordingsForTest(t, f, ""); len(recordings) != 0 {
		t.Fatalf("recordings = %d, want none with the setting off", len(recordings))
	}
}

// readTerminalRecordingsForTest reads the workspace's recordings, optionally
// narrowed to one stream.
func readTerminalRecordingsForTest(t *testing.T, f *relayFixture, stream string) []terminalRecordingRecord {
	t.Helper()
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	rows, err := f.service.readRecordings(t.Context(), scope, f.workspace, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	matched := []terminalRecordingRecord{}
	for _, row := range rows {
		if stream != "" && row.StreamID != stream {
			continue
		}
		recording, err := f.service.readRecording(t.Context(), scope, f.workspace, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		matched = append(matched, recording)
	}
	return matched
}

// TestWorkspaceTerminalRecordingEndpoints covers the two routes a recording is
// read through, including the audience narrowing the router cannot express.
func TestWorkspaceTerminalRecordingEndpoints(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)

	person.send(terminalOpenFrame())
	stream := runner.receive().Stream
	runner.send(terminalOutputFrame(t, stream, "recorded output"))
	person.receive()
	person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: stream, Type: workspacesession.TypeClose})
	person.receive()

	response := performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/workspaces/"+f.workspace+"/terminal-recordings", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var listing struct {
		Recordings []TerminalRecording `json:"recordings"`
	}
	decodeHubResponse(t, response, &listing)
	if len(listing.Recordings) != 1 {
		t.Fatalf("listing = %+v, want one recording", listing.Recordings)
	}
	recording := listing.Recordings[0]
	if recording.Artifact == "" || recording.Bytes <= 0 {
		t.Fatalf("recording = %+v, want a receipt and bytes behind it", recording)
	}

	response = performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/workspaces/"+f.workspace+"/terminal-recordings/"+recording.ID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), "recorded output") {
		t.Fatalf("recording body = %q, want the shell's output", response.Body.String())
	}
	// The bytes are the recording rather than an envelope around it: a reader
	// plays them or pipes them into asciinema.
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "asciicast") {
		t.Fatalf("content type = %q, want an asciicast", contentType)
	}

	response = performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/workspaces/"+f.workspace+"/terminal-recordings/termrec_absent", f.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
}

// resumeFrame is a person's resume for one stream.
func resumeFrame(t *testing.T, channel, stream string, lastSeq int64) workspacesession.Frame {
	t.Helper()
	payload, err := workspacesession.Encode(workspacesession.ResumePayload{Stream: stream, LastSeq: lastSeq})
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{Channel: channel, Type: workspacesession.TypeResume, Payload: payload}
}
