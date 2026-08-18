//go:build windows

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

func Configure(_ context.Context, cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return TerminateTree(cmd, 0)
	}
}

func GroupID(*exec.Cmd) int {
	return 0
}

func Deprioritize(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return ErrProcessNotRunning
	}
	handle, err := openProcess(cmd.Process.Pid, windows.PROCESS_SET_INFORMATION)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetPriorityClass(handle, windows.BELOW_NORMAL_PRIORITY_CLASS); err != nil {
		return fmt.Errorf("set worker process %d priority: %w", cmd.Process.Pid, err)
	}
	return nil
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

func inspectProcess(pid int) (Identity, error) {
	handle, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return Identity{}, err
	}
	defer windows.CloseHandle(handle)
	startedAt, err := processStartedAt(handle)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process %d start time: %w", pid, err)
	}
	return Identity{PID: pid, StartedAt: startedAt}, nil
}

func observeProcesses(identities []Identity) ([]Observation, error) {
	observations := make([]Observation, 0, len(identities))
	for _, identity := range identities {
		alive, err := Alive(identity)
		if err != nil {
			return nil, err
		}
		count := 0
		if alive {
			count = 1
		}
		observations = append(observations, Observation{Identity: identity, Alive: alive, ProcessCount: count})
	}
	return observations, nil
}

func Terminate(_ context.Context, identity Identity, grace time.Duration) (TerminationOutcome, error) {
	if identity.PID <= 0 {
		return TerminationOutcomeAlreadyExited, nil
	}
	handle, err := openProcess(identity.PID, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE)
	if errors.Is(err, ErrProcessNotRunning) {
		return TerminationOutcomeAlreadyExited, nil
	}
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	startedAt, err := processStartedAt(handle)
	if err != nil {
		return "", fmt.Errorf("inspect process %d start time: %w", identity.PID, err)
	}
	if !sameIdentity(Identity{PID: identity.PID, StartedAt: startedAt}, identity) {
		return TerminationOutcomeStaleIdentity, nil
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return "", fmt.Errorf("terminate process %d: %w", identity.PID, err)
	}
	if grace <= 0 {
		grace = DefaultTerminationGrace
	}
	waitMillis := uint32(max(1, grace.Milliseconds()))
	event, err := windows.WaitForSingleObject(handle, waitMillis)
	if err != nil {
		return "", fmt.Errorf("wait for process %d exit: %w", identity.PID, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		return "", fmt.Errorf("process %d remained alive after termination", identity.PID)
	}
	return TerminationOutcomeTerminated, nil
}

func openProcess(pid int, access uint32) (windows.Handle, error) {
	if pid <= 0 {
		return 0, ErrProcessNotRunning
	}
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, ErrProcessNotRunning
	}
	if err != nil {
		return 0, fmt.Errorf("open process %d: %w", pid, err)
	}
	return handle, nil
}

func processStartedAt(handle windows.Handle) (time.Time, error) {
	var created windows.Filetime
	var exited windows.Filetime
	var kernel windows.Filetime
	var user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, created.Nanoseconds()).UTC(), nil
}
