//go:build darwin

package procstart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func identity(pid int) (string, error) {
	if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
		return "", ErrNotRunning
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return "", ErrNotRunning
		}
		return "", fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	if int(info.Proc.P_pid) != pid {
		return "", ErrNotRunning
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("darwin:%d.%06d", start.Sec, start.Usec), nil
}
