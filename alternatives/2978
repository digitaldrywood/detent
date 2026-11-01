//go:build windows

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func Configure(_ context.Context, cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return TerminateTree(cmd, 0)
	}
}

type windowsJob struct {
	job     windows.Handle
	process windows.Handle
}

var windowsJobs = struct {
	sync.Mutex
	byPID map[int]windowsJob
}{byPID: make(map[int]windowsJob)}

func Start(cmd *exec.Cmd) error {
	return startWindows(cmd, windows.AssignProcessToJobObject, resumeWindowsProcess)
}

func Wait(cmd *exec.Cmd) error {
	groupID := GroupID(cmd)
	return errors.Join(cmd.Wait(), Cleanup(groupID))
}

func startWindows(cmd *exec.Cmd, assign func(windows.Handle, windows.Handle) error, resume func(int) error) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create worker job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return errors.Join(fmt.Errorf("configure worker job: %w", err), windows.CloseHandle(job))
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	flags := cmd.SysProcAttr.CreationFlags
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	defer func() { cmd.SysProcAttr.CreationFlags = flags }()

	windowsJobs.Lock()
	err = cmd.Start()
	if err != nil {
		windowsJobs.Unlock()
		return errors.Join(err, windows.CloseHandle(job))
	}
	process, err := openProcess(cmd.Process.Pid, windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE)
	if err == nil {
		err = assign(job, process)
	}
	if err == nil {
		windowsJobs.byPID[cmd.Process.Pid] = windowsJob{job: job, process: process}
		err = resume(cmd.Process.Pid)
	}
	if err != nil {
		delete(windowsJobs.byPID, cmd.Process.Pid)
	}
	windowsJobs.Unlock()
	if err != nil {
		killErr := cmd.Process.Kill()
		jobCloseErr := windows.CloseHandle(job)
		_ = cmd.Wait()
		var closeErr error
		if process != 0 {
			closeErr = windows.CloseHandle(process)
		}
		return errors.Join(fmt.Errorf("start worker job: %w", err), killErr, jobCloseErr, closeErr)
	}
	return nil
}

func resumeWindowsProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("inspect suspended worker threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snapshot, &entry); ; err = windows.Thread32Next(snapshot, &entry) {
		if err != nil {
			return fmt.Errorf("find suspended worker thread: %w", err)
		}
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open suspended worker thread: %w", err)
		}
		_, resumeErr := windows.ResumeThread(thread)
		return errors.Join(resumeErr, windows.CloseHandle(thread))
	}
}

func GroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	windowsJobs.Lock()
	defer windowsJobs.Unlock()
	if _, ok := windowsJobs.byPID[cmd.Process.Pid]; ok {
		return cmd.Process.Pid
	}
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

func TerminateTree(cmd *exec.Cmd, groupID int) error {
	if groupID <= 0 && cmd != nil && cmd.Process != nil {
		groupID = cmd.Process.Pid
	}
	windowsJobs.Lock()
	defer windowsJobs.Unlock()
	if job, ok := windowsJobs.byPID[groupID]; ok {
		return windows.TerminateJobObject(job.job, 1)
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func Cleanup(groupID int) error {
	windowsJobs.Lock()
	defer windowsJobs.Unlock()
	job, ok := windowsJobs.byPID[groupID]
	if !ok {
		return nil
	}
	if err := windows.CloseHandle(job.job); err != nil {
		return fmt.Errorf("close worker job: %w", err)
	}
	delete(windowsJobs.byPID, groupID)
	return windows.CloseHandle(job.process)
}

func inspectProcess(pid int) (Identity, error) {
	handle, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE)
	if err != nil {
		return Identity{}, err
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process %d exit state: %w", pid, err)
	}
	if event == windows.WAIT_OBJECT_0 {
		return Identity{}, ErrProcessNotRunning
	}
	startedAt, err := processStartedAt(handle)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process %d start time: %w", pid, err)
	}
	groupID := GroupID(&exec.Cmd{Process: &os.Process{Pid: pid}})
	return Identity{PID: pid, GroupID: groupID, StartedAt: startedAt}, nil
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
	groupID := GroupID(&exec.Cmd{Process: &os.Process{Pid: identity.PID}})
	if !sameIdentity(Identity{PID: identity.PID, GroupID: groupID, StartedAt: startedAt}, identity) {
		return TerminationOutcomeStaleIdentity, nil
	}
	windowsJobs.Lock()
	if job, ok := windowsJobs.byPID[identity.PID]; ok {
		err = windows.TerminateJobObject(job.job, 1)
	} else {
		err = windows.TerminateProcess(handle, 1)
	}
	windowsJobs.Unlock()
	if err != nil {
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
