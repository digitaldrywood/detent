package procgroup

import (
	"errors"
	"os/exec"
	"time"
)

const DefaultTerminationGrace = 250 * time.Millisecond

var ErrProcessNotRunning = errors.New("process is not running")

type Identity struct {
	PID       int
	GroupID   int
	StartedAt time.Time
}

type TerminationOutcome string

const (
	TerminationOutcomeTerminated    TerminationOutcome = "terminated"
	TerminationOutcomeAlreadyExited TerminationOutcome = "already_exited"
	TerminationOutcomeStaleIdentity TerminationOutcome = "stale_identity"
)

func Inspect(cmd *exec.Cmd) (Identity, error) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	return inspectProcess(cmd.Process.Pid)
}
