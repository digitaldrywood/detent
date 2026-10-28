//go:build !linux

package releaseaudit

import (
	"errors"
	"os/exec"
)

func checkSandboxMemory() error {
	return errors.New("release audit requires a delegated cgroup v2 memory controller")
}

func runSandboxCommand(*exec.Cmd) error {
	return checkSandboxMemory()
}
