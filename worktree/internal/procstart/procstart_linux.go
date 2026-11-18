//go:build linux

package procstart

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

const statStartTimeField = 19

func identity(pid int) (string, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotRunning
	}
	if err != nil {
		return "", fmt.Errorf("read process %d stat: %w", pid, err)
	}
	start, err := statStartTime(string(stat))
	if err != nil {
		return "", err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	return "linux:" + strings.TrimSpace(string(bootID)) + ":" + start, nil
}

func statStartTime(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("unexpected process stat %q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) <= statStartTimeField {
		return "", fmt.Errorf("unexpected process stat %q", stat)
	}
	if _, err := strconv.ParseUint(fields[statStartTimeField], 10, 64); err != nil {
		return "", fmt.Errorf("parse process start time: %w", err)
	}
	return fields[statStartTimeField], nil
}
