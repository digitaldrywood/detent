package hubserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceTerminalOverflowRetainsDeliveredOutput(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withTerminal)
	f.service.workspaces.config.RelayMemoryBytes = 64
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	type deliveredRecording struct{ stream, output string }
	var delivered []deliveredRecording
	for index := range 2 {
		person.send(terminalOpenFrame())
		opened := runner.receive()
		if opened.Type != workspacesession.TypeTerminalOpen || opened.Stream == "" {
			t.Fatalf("terminal open=%+v", opened)
		}
		output := fmt.Sprintf("delivered terminal output %d", index)
		runner.send(terminalOutputFrame(t, opened.Stream, output))
		frame := person.receive()
		if frame.Type != workspacesession.TypeTerminalOutput || frame.Stream != opened.Stream {
			t.Fatalf("delivered output=%+v", frame)
		}
		var terminal workspacesession.TerminalOutput
		if err := json.Unmarshal(frame.Payload, &terminal); err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(terminal.Data)
		if err != nil || string(decoded) != output {
			t.Fatalf("delivered terminal output=%q error=%v", decoded, err)
		}
		failure := person.receive()
		var problem workspacesession.ErrorPayload
		if err := json.Unmarshal(failure.Payload, &problem); err != nil {
			t.Fatal(err)
		}
		if failure.Type != workspacesession.TypeError || problem.Code != workspacesession.CodeOverflow {
			t.Fatalf("overflow=%+v", failure)
		}
		closed := runner.receive()
		if closed.Type != workspacesession.TypeClose || closed.Stream != opened.Stream {
			t.Fatalf("terminal close=%+v", closed)
		}
		delivered = append(delivered, deliveredRecording{opened.Stream, output})
	}
	verify := func() {
		t.Helper()
		for _, record := range delivered {
			recordings := readTerminalRecordingsForTest(t, f, record.stream)
			if len(recordings) != 1 || !strings.Contains(recordings[0].Cast, record.output) {
				t.Errorf("delivered terminal output missing from recording: stream=%s recordings=%+v", record.stream, recordings)
			}
		}
	}
	verify()
	f.service = reopenWorkspaceHub(t, f.service)
	verify()
}
