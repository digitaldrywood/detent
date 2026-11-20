package procgroup

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func inspectProcess(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, unix.ESRCH) || err != nil && errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
		return Identity{}, ErrProcessNotRunning
	}
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process: %w", err)
	}
	if int(info.Proc.P_pid) != pid {
		return Identity{}, ErrProcessNotRunning
	}
	return Identity{PID: pid, GroupID: int(info.Eproc.Pgid), StartedAt: time.Unix(info.Proc.P_starttime.Sec, int64(info.Proc.P_starttime.Usec)*int64(time.Microsecond)).UTC()}, nil
}

func inspectProcessGroup(group int) ([]processGroupMember, error) {
	table, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("inspect process group: %w", err)
	}
	var members []processGroupMember
	for _, info := range table {
		if int(info.Eproc.Pgid) != group {
			continue
		}
		state := "alive"
		if info.Proc.P_stat == 5 {
			state = "Z"
		}
		members = append(members, processGroupMember{pid: strconv.Itoa(int(info.Proc.P_pid)), ppid: strconv.Itoa(int(info.Eproc.Ppid)), state: state, command: unix.ByteSliceToString(info.Proc.P_comm[:])})
	}
	return members, nil
}
