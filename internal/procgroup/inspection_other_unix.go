//go:build unix && !darwin && !linux

package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func inspectProcess(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	path, err := exec.LookPath("ps")
	if err != nil {
		return Identity{}, fmt.Errorf("locate ps: %w", err)
	}
	cmd := &exec.Cmd{
		Path: path,
		Args: []string{"ps", "-p", strconv.Itoa(pid), "-o", "pgid=", "-o", "lstart="},
		Env:  append(os.Environ(), "LC_ALL=C"),
	}
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Identity{}, ErrProcessNotRunning
		}
		return Identity{}, fmt.Errorf("inspect process %d: %w", pid, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 6 {
		return Identity{}, fmt.Errorf("inspect process %d: unexpected ps output %q", pid, strings.TrimSpace(string(output)))
	}
	groupID, err := strconv.Atoi(fields[0])
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process %d group: %w", pid, err)
	}
	startedAt, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(fields[1:], " "), time.Local)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process %d start time: %w", pid, err)
	}
	return Identity{PID: pid, GroupID: groupID, StartedAt: startedAt.UTC()}, nil
}

func inspectProcessGroup(processGroupID int) ([]processGroupMember, error) {
	path, err := exec.LookPath("ps")
	if err != nil {
		return nil, fmt.Errorf("locate ps: %w", err)
	}
	cmd := &exec.Cmd{
		Path: path,
		Args: []string{"ps", "-axo", "pid=,ppid=,pgid=,stat=,comm="},
		Env:  append(os.Environ(), "LC_ALL=C"),
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect process group: %w", err)
	}
	members := make([]processGroupMember, 0)
	for line := range strings.Lines(string(output)) {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[2] != strconv.Itoa(processGroupID) {
			continue
		}
		members = append(members, processGroupMember{
			pid:     fields[0],
			ppid:    fields[1],
			state:   fields[3],
			command: strings.Join(fields[4:], " "),
		})
	}
	return members, nil
}
