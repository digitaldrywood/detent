package releaseaudit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type sandboxCgroup struct {
	directory string
	file      *os.File
}

func newSandboxCgroup() (*sandboxCgroup, error) {
	content, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, fmt.Errorf("read release audit cgroup: %w", err)
	}
	var parent string
	for _, line := range strings.Split(string(content), "\n") {
		if name, ok := strings.CutPrefix(line, "0::/"); ok && (name == "" || filepath.IsLocal(name)) {
			parent = filepath.Join("/sys/fs/cgroup", name)
			break
		}
	}
	if parent == "" {
		return nil, errors.New("release audit requires a delegated cgroup v2 memory controller")
	}
	directory, err := os.MkdirTemp(parent, "detent-release-audit-")
	if err != nil {
		return nil, fmt.Errorf("create release audit memory cgroup: %w", err)
	}
	group, err := configureSandboxCgroup(directory)
	if err != nil {
		return nil, errors.Join(err, os.Remove(directory))
	}
	return group, nil
}

func configureSandboxCgroup(directory string) (*sandboxCgroup, error) {
	for _, control := range []struct{ name, value string }{
		{"memory.max", "1073741824"},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
	} {
		if err := writeSandboxControl(directory, control.name, control.value); err != nil {
			return nil, err
		}
		content, err := os.ReadFile(filepath.Join(directory, control.name))
		if err != nil {
			return nil, fmt.Errorf("read release audit memory control: %w", err)
		}
		if strings.TrimSpace(string(content)) != control.value {
			return nil, errors.New("release audit memory control was not applied")
		}
	}
	kill, err := os.OpenFile(filepath.Join(directory, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open release audit cgroup cleanup control: %w", err)
	}
	if err := kill.Close(); err != nil {
		return nil, err
	}
	file, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	return &sandboxCgroup{directory: directory, file: file}, nil
}

func writeSandboxControl(directory, name, value string) error {
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("open release audit resource control: %w", err)
	}
	_, writeErr := file.WriteString(value)
	return errors.Join(writeErr, file.Close())
}

func (g *sandboxCgroup) kill() error {
	return writeSandboxControl(g.directory, "cgroup.kill", "1")
}

func (g *sandboxCgroup) close() error {
	return errors.Join(g.kill(), g.file.Close(), os.Remove(g.directory))
}

func checkSandboxMemory() error {
	group, err := newSandboxCgroup()
	if err != nil {
		return err
	}
	return group.close()
}

func runSandboxCommand(command *exec.Cmd) error {
	group, err := newSandboxCgroup()
	if err != nil {
		return err
	}
	return runSandboxCgroupCommand(command, group)
}

func runSandboxCgroupCommand(command *exec.Cmd, group *sandboxCgroup) error {
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(group.file.Fd())}
	command.Cancel = group.kill
	return errors.Join(command.Run(), group.memoryError(), group.close())
}

func (g *sandboxCgroup) memoryError() error {
	content, err := os.ReadFile(filepath.Join(g.directory, "memory.events"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[0] == "oom" || fields[0] == "oom_kill") {
			count, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || count > 0 {
				return errors.New("release audit fixture exceeded its memory budget")
			}
		}
	}
	return nil
}
