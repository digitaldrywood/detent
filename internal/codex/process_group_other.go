//go:build !unix

package codex

import "os/exec"

func configureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return terminateCommandProcessTree(cmd, 0)
	}
}

func commandProcessGroupID(*exec.Cmd) int {
	return 0
}

func terminateCommandProcessTree(cmd *exec.Cmd, _ int) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func cleanupCommandProcessGroup(int) error {
	return nil
}
