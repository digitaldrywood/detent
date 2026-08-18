package procgroup

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

const DefaultTerminationGrace = 250 * time.Millisecond

var (
	ErrProcessNotRunning = errors.New("process is not running")
	ErrRSSUnsupported    = errors.New("process RSS inspection is unsupported on this platform")
)

type Identity struct {
	PID       int
	GroupID   int
	StartedAt time.Time
}

type TerminationOutcome string

const (
	TerminationOutcomeTerminated    TerminationOutcome = "terminated"
	TerminationOutcomeKilled        TerminationOutcome = "killed_after_timeout"
	TerminationOutcomeAlreadyExited TerminationOutcome = "already_exited"
	TerminationOutcomeStaleIdentity TerminationOutcome = "stale_identity"
)

func Inspect(cmd *exec.Cmd) (Identity, error) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	return inspectProcess(cmd.Process.Pid)
}

func Alive(identity Identity) (bool, error) {
	if identity.PID <= 0 || identity.StartedAt.IsZero() {
		return false, nil
	}
	current, err := inspectProcess(identity.PID)
	if errors.Is(err, ErrProcessNotRunning) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sameIdentity(current, identity), nil
}

func RSS(ctx context.Context, identity Identity) (uint64, error) {
	return processGroupRSS(ctx, identity)
}

func sameIdentity(current Identity, recorded Identity) bool {
	return current.PID == recorded.PID &&
		(recorded.GroupID <= 0 || current.GroupID == recorded.GroupID) &&
		!recorded.StartedAt.IsZero() && current.StartedAt.Equal(recorded.StartedAt)
}
