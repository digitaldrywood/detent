//go:build !unix

package procgroup

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func Configure(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return TerminateTree(cmd, 0)
	}
}

func GroupID(*exec.Cmd) int {
	return 0
}

func TerminateTree(cmd *exec.Cmd, _ int) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func Cleanup(int) error {
	return nil
}

func inspectProcess(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	return Identity{PID: pid, StartedAt: time.Now().UTC()}, nil
}

func Terminate(_ context.Context, identity Identity, _ time.Duration) (TerminationOutcome, error) {
	if identity.PID <= 0 {
		return TerminationOutcomeAlreadyExited, nil
	}
	process, err := os.FindProcess(identity.PID)
	if err != nil {
		return TerminationOutcomeAlreadyExited, nil
	}
	if err := process.Kill(); err != nil {
		return "", err
	}
	return TerminationOutcomeTerminated, nil
}
