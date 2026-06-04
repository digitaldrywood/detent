//go:build unix

package codex

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureCommandProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return terminateCommandProcessTree(cmd, commandProcessGroupID(cmd))
	}
}

func commandProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid <= 0 {
		return cmd.Process.Pid
	}
	return pgid
}

func terminateCommandProcessTree(cmd *exec.Cmd, processGroupID int) error {
	if processGroupID > 0 {
		err := syscall.Kill(-processGroupID, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func cleanupCommandProcessGroup(processGroupID int) error {
	if processGroupID <= 0 {
		return nil
	}
	err := syscall.Kill(-processGroupID, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
