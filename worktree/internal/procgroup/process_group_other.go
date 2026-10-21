//go:build !unix && !windows

package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

func Configure(_ context.Context, cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return TerminateTree(cmd, 0)
	}
}

func GroupID(*exec.Cmd) int {
	return 0
}

func Deprioritize(*exec.Cmd) error {
	return nil
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

func observeProcesses(identities []Identity) ([]Observation, error) {
	observations := make([]Observation, 0, len(identities))
	for _, identity := range identities {
		alive, err := Alive(identity)
		if err != nil {
			return nil, err
		}
		count := 0
		if alive {
			count = 1
		}
		observations = append(observations, Observation{Identity: identity, Alive: alive, ProcessCount: count})
	}
	return observations, nil
}

func Terminate(_ context.Context, identity Identity, _ time.Duration) (TerminationOutcome, error) {
	if identity.PID <= 0 {
		return TerminationOutcomeAlreadyExited, nil
	}
	return "", errors.New("validated worker process termination is unsupported on this platform")
}
