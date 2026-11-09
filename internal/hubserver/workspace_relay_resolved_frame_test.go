package hubserver

import (
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceRelayResolvedFrameCannotMutateReplacement(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal bool
		close    bool
		overflow bool
	}{
		{name: "exec output"},
		{name: "terminal output", terminal: true},
		{name: "exec close", close: true},
		{name: "terminal close", terminal: true, close: true},
		{name: "exec overflow", overflow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			action := f.action(t, "Example replacement action", "printf example")
			oldRun := f.queuedRun(t, action)
			newRun := f.queuedRun(t, action)
			service.handlePersonFrame(t.Context(), person, record, execRunFrame(oldRun, action))
			oldExec := runner.receive()
			service.handlePersonFrame(t.Context(), person, record, terminalOpenFrame())
			oldTerminal := runner.receive()
			if oldExec.Type != workspacesession.TypeExecRun || oldExec.Stream == "" || oldTerminal.Type != workspacesession.TypeTerminalOpen || oldTerminal.Stream == "" {
				t.Fatalf("old runner frames: exec=%+v terminal=%+v", oldExec, oldTerminal)
			}
			const staleOutput = "old runner output after replacement"
			frame := execOutputFrame(oldExec.Stream, workspacesession.ExecOutput{Data: staleOutput})
			if test.terminal {
				frame = terminalOutputFrame(t, oldTerminal.Stream, staleOutput)
			}
			if test.close {
				frame.Type = workspacesession.TypeClosed
				frame.Payload = nil
			}
			type resolvedFrame struct {
				stream *relayStream
				person *relayConnection
			}
			resolved := make(chan resolvedFrame, 1)
			resume := make(chan struct{})
			finished := make(chan struct{})
			var resumeOnce sync.Once
			t.Cleanup(func() {
				resumeOnce.Do(func() { close(resume) })
				<-finished
			})
			go func() {
				defer close(finished)
				stream, recipient := relay.runnerStream(previous, frame.Stream)
				resolved <- resolvedFrame{stream: stream, person: recipient}
				<-resume
				if stream != nil {
					service.handleRunnerStreamFrame(t.Context(), previous, stream, recipient, frame)
				}
			}()
			if captured := <-resolved; captured.stream == nil || captured.person != person {
				t.Fatalf("old runner did not resolve its stream: %+v", captured)
			}
			current := &relayConnection{
				id: newNativeID("relayrunner"), workspaceID: f.workspace, runner: true,
				scope: previous.scope, owner: previous.owner,
				out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{}),
			}
			replacement, err := relay.attachRunner(current)
			if err != nil {
				t.Fatal(err)
			}
			service.finishRunnerReplacement(t.Context(), replacement)
			newExecFrame := execRunFrame(newRun, action)
			newExecFrame.Stream = oldExec.Stream
			service.handlePersonFrame(t.Context(), person, record, newExecFrame)
			newTerminalFrame := terminalOpenFrame()
			newTerminalFrame.Stream = oldTerminal.Stream
			service.handlePersonFrame(t.Context(), person, record, newTerminalFrame)
			for _, want := range []workspacesession.Frame{newExecFrame, newTerminalFrame} {
				select {
				case delivered := <-current.out:
					if delivered.frame.Type != want.Type || delivered.frame.Stream != want.Stream {
						t.Fatalf("replacement runner frame=%+v want %+v", delivered.frame, want)
					}
				default:
					t.Fatalf("replacement runner did not receive %s on the existing stream", want.Type)
				}
			}
			relay.appendExecOutput(f.workspace, oldExec.Stream, []byte("replacement exec output"))
			relay.appendTerminalEvent(f.workspace, oldTerminal.Stream, workspacesession.AsciicastOutput, "replacement terminal output", service.now())
			relay.mu.Lock()
			room := relay.rooms[f.workspace]
			execState := room.execRuns[oldExec.Stream]
			terminalState := room.terminals[oldTerminal.Stream]
			memory := relay.memory
			if test.overflow {
				service.config.RelayMemoryBytes = memory + 1
			}
			relay.mu.Unlock()
			if execState == nil || terminalState == nil {
				t.Fatal("replacement work was not admitted on the existing streams")
			}
			resumeOnce.Do(func() { close(resume) })
			<-finished
			relay.mu.Lock()
			room = relay.rooms[f.workspace]
			if room == nil || room.runner != current {
				relay.mu.Unlock()
				t.Fatal("old runner frame removed the replacement runner")
			}
			currentExec := room.execRuns[oldExec.Stream]
			currentTerminal := room.terminals[oldTerminal.Stream]
			if currentExec != execState || currentTerminal != terminalState {
				t.Errorf("old runner frame consumed replacement state: exec=%v terminal=%v", currentExec == execState, currentTerminal == terminalState)
			}
			if string(execState.output) != "replacement exec output" || strings.Contains(terminalState.recording.Cast(), staleOutput) {
				t.Errorf("old runner frame contaminated replacement output: exec=%q terminal=%q", execState.output, terminalState.recording.Cast())
			}
			if relay.memory != memory {
				t.Errorf("old runner frame changed replacement replay memory: got %d want %d", relay.memory, memory)
			}
			relay.mu.Unlock()
			if running := f.runRow(t, newRun); running.Status != workspacesession.RunRunning || running.FinishedAt != nil {
				t.Errorf("old runner frame completed replacement run: %+v", running)
			}
		})
	}
}
