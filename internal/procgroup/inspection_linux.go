package procgroup

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

func linuxProcessStat(pid int) (string, []string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return "", nil, ErrProcessNotRunning
	}
	if err != nil {
		return "", nil, fmt.Errorf("read process stat: %w", err)
	}
	stat := string(data)
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end <= open {
		return "", nil, errors.New("invalid process stat command")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) <= 19 {
		return "", nil, errors.New("incomplete process stat")
	}
	return stat[open+1 : end], fields, nil
}

func inspectProcess(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	_, fields, err := linuxProcessStat(pid)
	if err != nil {
		return Identity{}, err
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return Identity{}, fmt.Errorf("read process group: %w", err)
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return Identity{}, fmt.Errorf("read process start ticks: %w", err)
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return Identity{}, fmt.Errorf("read kernel boot time: %w", err)
	}
	for line := range strings.Lines(string(stat)) {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return Identity{}, fmt.Errorf("read kernel boot seconds: %w", err)
			}
			return Identity{PID: pid, GroupID: group, StartedAt: time.Unix(boot+ticks/int64(cpu.ClocksPerSec), ticks%int64(cpu.ClocksPerSec)*int64(time.Second)/int64(cpu.ClocksPerSec)).UTC()}, nil
		}
	}
	return Identity{}, errors.New("kernel boot time is unavailable")
}

func inspectProcessGroup(group int) ([]processGroupMember, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var members []processGroupMember
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		pgid, err := syscall.Getpgid(pid)
		if err != nil || pgid != group {
			continue
		}
		command, fields, err := linuxProcessStat(pid)
		if errors.Is(err, ErrProcessNotRunning) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if fields[2] == strconv.Itoa(group) {
			members = append(members, processGroupMember{pid: entry.Name(), ppid: fields[1], state: fields[0], command: command})
		}
	}
	return members, nil
}
