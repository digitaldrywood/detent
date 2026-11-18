//go:build unix

package workspaceexec

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// exitSignal names the signal that ended a process, or the empty string when
// the process exited on its own.
//
// The name is the signal's own ("SIGKILL") rather than Go's prose for it
// ("killed"), because it is carried to a person as the reason a run has no exit
// code and a reason a reader can search for is worth more than one they cannot.
func exitSignal(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	if name := unix.SignalName(status.Signal()); name != "" {
		return name
	}
	return status.Signal().String()
}
