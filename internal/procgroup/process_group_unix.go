//go:build unix

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const workerNiceDelta = 5

func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return TerminateTree(cmd, GroupID(cmd))
	}
}

func GroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid <= 0 {
		return cmd.Process.Pid
	}
	return pgid
}

func Deprioritize(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return ErrProcessNotRunning
	}
	current, err := processNice(os.Getpid())
	if err != nil {
		return fmt.Errorf("read Detent process priority: %w", err)
	}
	target := min(19, current+workerNiceDelta)
	if err := unix.Setpriority(unix.PRIO_PROCESS, cmd.Process.Pid, target); errors.Is(err, syscall.ESRCH) {
		return nil
	} else if err != nil {
		return fmt.Errorf("set worker process %d priority: %w", cmd.Process.Pid, err)
	}
	return nil
}

func TerminateTree(cmd *exec.Cmd, processGroupID int) error {
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

func Cleanup(processGroupID int) error {
	if processGroupID <= 0 {
		return nil
	}
	err := syscall.Kill(-processGroupID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	if waitForProcessGroupExit(processGroupID, DefaultTerminationGrace) {
		return nil
	}
	survivingProcesses := describeProcessGroup(processGroupID)
	if !processTargetAlive(0, processGroupID) {
		return nil
	}
	return fmt.Errorf(
		"process group %d remained alive after SIGKILL: surviving_processes=%s",
		processGroupID,
		survivingProcesses,
	)
}

func waitForProcessGroupExit(processGroupID int, grace time.Duration) bool {
	if grace <= 0 {
		grace = DefaultTerminationGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !processTargetAlive(0, processGroupID) {
			return true
		}
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

func describeProcessGroup(processGroupID int) string {
	path, err := exec.LookPath("ps")
	if err != nil {
		return "unavailable (locate ps: " + err.Error() + ")"
	}
	cmd := &exec.Cmd{
		Path: path,
		Args: []string{"ps", "-axo", "pid=,ppid=,pgid=,comm="},
		Env:  append(os.Environ(), "LC_ALL=C"),
	}
	output, err := cmd.Output()
	if err != nil {
		return "unavailable (inspect process group: " + err.Error() + ")"
	}
	members := make([]string, 0)
	for line := range strings.Lines(string(output)) {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != strconv.Itoa(processGroupID) {
			continue
		}
		members = append(members, fmt.Sprintf("pid=%s ppid=%s command=%s", fields[0], fields[1], strings.Join(fields[3:], " ")))
	}
	if len(members) == 0 {
		return "none listed"
	}
	return strings.Join(members, "; ")
}

func Terminate(ctx context.Context, identity Identity, grace time.Duration) (TerminationOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if identity.PID <= 0 {
		return TerminationOutcomeAlreadyExited, nil
	}
	if identity.GroupID <= 0 || identity.StartedAt.IsZero() {
		return TerminationOutcomeStaleIdentity, nil
	}

	current, err := inspectProcess(identity.PID)
	if err != nil && !errors.Is(err, ErrProcessNotRunning) {
		return "", err
	}
	if err == nil && !sameIdentity(current, identity) {
		return TerminationOutcomeStaleIdentity, nil
	}
	groupID := identity.GroupID
	if !processTargetAlive(identity.PID, groupID) {
		return TerminationOutcomeAlreadyExited, nil
	}
	if err := signalProcessTarget(identity.PID, groupID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return TerminationOutcomeAlreadyExited, nil
		}
		return "", err
	}
	if waitForProcessTargetExit(ctx, identity.PID, groupID, grace) {
		return TerminationOutcomeTerminated, nil
	}
	if err := signalProcessTarget(identity.PID, groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return "", err
	}
	if !waitForProcessTargetExit(context.Background(), identity.PID, groupID, grace) {
		return "", fmt.Errorf("process group %d remained alive after SIGKILL", groupID)
	}
	return TerminationOutcomeTerminated, nil
}

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

func signalProcessTarget(pid int, groupID int, signal syscall.Signal) error {
	if groupID > 0 {
		return syscall.Kill(-groupID, signal)
	}
	return syscall.Kill(pid, signal)
}

func processTargetAlive(pid int, groupID int) bool {
	err := signalProcessTarget(pid, groupID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitForProcessTargetExit(ctx context.Context, pid int, groupID int, grace time.Duration) bool {
	if grace <= 0 {
		grace = DefaultTerminationGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !processTargetAlive(pid, groupID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}
