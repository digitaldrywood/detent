//go:build !unix

package procgroup

import "os/exec"

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
