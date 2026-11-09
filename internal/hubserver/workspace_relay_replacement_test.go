package hubserver

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceRelayReplacementPreservesNewRunnerWork(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec, withTerminal)
	runner := f.dialRunner(t)
	f.dialPerson(t)
	service := f.service.workspaces
	relay := service.relay
	record := f.record(t)
	relay.mu.Lock()
	previous := relay.rooms[f.workspace].runner
	var person *relayConnection
	for _, connection := range relay.rooms[f.workspace].people {
		person = connection
	}
	relay.mu.Unlock()
	if previous == nil || person == nil {
		t.Fatal("runner and person must be attached")
	}
	action := f.action(t, "Example action", "printf example")
	oldRun := f.queuedRun(t, action)
	newRun := f.queuedRun(t, action)
	service.handlePersonFrame(t.Context(), person, record, execRunFrame(oldRun, action))
	oldExec := runner.receive()
	if oldExec.Type != workspacesession.TypeExecRun || oldExec.Stream == "" {
		t.Fatalf("old runner exec frame=%+v", oldExec)
	}
	relay.appendExecOutput(f.workspace, oldExec.Stream, []byte("old run output"))
	service.handlePersonFrame(t.Context(), person, record, terminalOpenFrame())
	oldTerminal := runner.receive()
	if oldTerminal.Type != workspacesession.TypeTerminalOpen || oldTerminal.Stream == "" {
		t.Fatalf("old runner terminal frame=%+v", oldTerminal)
	}
	relay.appendTerminalEvent(f.workspace, oldTerminal.Stream, workspacesession.AsciicastOutput, "old terminal output", service.now())
	current := &relayConnection{
		id: newNativeID("relayrunner"), workspaceID: f.workspace, runner: true,
		scope: previous.scope, owner: previous.owner,
		out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{}),
	}
	replacement, err := relay.attachRunner(current)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.previous != previous {
		t.Fatal("replacement did not capture the previous runner")
	}
	forwarded := func(frame workspacesession.Frame) workspacesession.Frame {
		t.Helper()
		service.handlePersonFrame(t.Context(), person, record, frame)
		select {
		case outbound := <-current.out:
			if outbound.frame.Type != frame.Type || outbound.frame.Stream == "" {
				t.Fatalf("new runner frame=%+v, want %s", outbound.frame, frame.Type)
			}
			return outbound.frame
		default:
			t.Fatalf("new runner did not receive %s", frame.Type)
			return workspacesession.Frame{}
		}
	}
	newExec := forwarded(execRunFrame(newRun, action))
	newTerminal := forwarded(terminalOpenFrame())
	relay.appendExecOutput(f.workspace, newExec.Stream, []byte("new run output"))
	relay.appendTerminalEvent(f.workspace, newTerminal.Stream, workspacesession.AsciicastOutput, "new terminal output", service.now())
	relay.mu.Lock()
	newExecState := relay.rooms[f.workspace].execRuns[newExec.Stream]
	newTerminalState := relay.rooms[f.workspace].terminals[newTerminal.Stream]
	relay.mu.Unlock()
	if newExecState == nil || newTerminalState == nil {
		t.Fatal("replacement runner work was not admitted before old-state publication")
	}
	service.finishRunnerReplacement(t.Context(), replacement)
	if failed := f.runRow(t, oldRun); failed.Status != workspacesession.RunFailed || failed.Reason != workspacesession.RunReasonStreamClosed || failed.Output != "old run output" {
		t.Errorf("old runner run=%+v", failed)
	}
	if running := f.runRow(t, newRun); running.Status != workspacesession.RunRunning || running.FinishedAt != nil {
		t.Errorf("replacement runner run was consumed by old-state publication: %+v", running)
	}
	oldRecordings := readTerminalRecordingsForTest(t, f, oldTerminal.Stream)
	if len(oldRecordings) != 1 || !strings.Contains(oldRecordings[0].Cast, "old terminal output") {
		t.Errorf("old runner recordings=%+v", oldRecordings)
	}
	if recordings := readTerminalRecordingsForTest(t, f, newTerminal.Stream); len(recordings) != 0 {
		t.Errorf("replacement runner recording was published prematurely: %+v", recordings)
	}
	relay.mu.Lock()
	room := relay.rooms[f.workspace]
	currentExec := room.execRuns[newExec.Stream]
	currentTerminal := room.terminals[newTerminal.Stream]
	_, oldExecLive := room.execRuns[oldExec.Stream]
	_, oldTerminalLive := room.terminals[oldTerminal.Stream]
	relay.mu.Unlock()
	if currentExec != newExecState || currentTerminal != newTerminalState || oldExecLive || oldTerminalLive {
		t.Errorf("replacement changed current state: exec=%v terminal=%v old_exec=%v old_terminal=%v", currentExec == newExecState, currentTerminal == newTerminalState, oldExecLive, oldTerminalLive)
	}
}
