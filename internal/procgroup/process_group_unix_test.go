//go:build unix

package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestConfigure(t *testing.T) {
	existing := &syscall.SysProcAttr{}

	tests := []struct {
		name     string
		cmd      *exec.Cmd
		wantAttr *syscall.SysProcAttr
	}{
		{
			name: "fresh command",
			cmd:  exec.Command("true"),
		},
		{
			name: "existing syscall attributes",
			cmd: &exec.Cmd{
				Path:        "true",
				SysProcAttr: existing,
			},
			wantAttr: existing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Configure(t.Context(), tt.cmd)

			if tt.cmd.SysProcAttr == nil {
				t.Fatal("SysProcAttr is nil, want configured attributes")
			}
			if !tt.cmd.SysProcAttr.Setpgid {
				t.Fatal("SysProcAttr.Setpgid is false, want true")
			}
			if tt.cmd.Cancel == nil {
				t.Fatal("Cancel is nil, want process tree termination hook")
			}
			if tt.wantAttr != nil && tt.cmd.SysProcAttr != tt.wantAttr {
				t.Fatal("SysProcAttr was replaced, want existing attributes reused")
			}
		})
	}
}

func TestGroupID(t *testing.T) {
	tests := []struct {
		name string
		cmd  func(t *testing.T) *exec.Cmd
		want func(t *testing.T, cmd *exec.Cmd, got int)
	}{
		{
			name: "nil command",
			cmd: func(*testing.T) *exec.Cmd {
				return nil
			},
			want: func(t *testing.T, _ *exec.Cmd, got int) {
				if got != 0 {
					t.Fatalf("GroupID() = %d, want 0", got)
				}
			},
		},
		{
			name: "nil process",
			cmd: func(*testing.T) *exec.Cmd {
				return exec.Command("true")
			},
			want: func(t *testing.T, _ *exec.Cmd, got int) {
				if got != 0 {
					t.Fatalf("GroupID() = %d, want 0", got)
				}
			},
		},
		{
			name: "started command",
			cmd: func(t *testing.T) *exec.Cmd {
				return startSleep(t).cmd
			},
			want: func(t *testing.T, cmd *exec.Cmd, got int) {
				if got <= 0 {
					t.Fatalf("GroupID() = %d, want positive group ID", got)
				}
				if got != cmd.Process.Pid {
					t.Fatalf("GroupID() = %d, want child pid %d", got, cmd.Process.Pid)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.cmd(t)
			tt.want(t, cmd, GroupID(cmd))
		})
	}
}

func TestDeprioritize(t *testing.T) {
	proc := startSleep(t)
	parentPriority, err := processNice(os.Getpid())
	if err != nil {
		t.Fatalf("Getpriority(parent) error = %v", err)
	}
	if err := Deprioritize(proc.cmd); err != nil {
		t.Fatalf("Deprioritize() error = %v", err)
	}
	workerPriority, err := processNice(proc.cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpriority(worker) error = %v", err)
	}
	if want := min(19, parentPriority+workerNiceDelta); workerPriority != want {
		t.Fatalf("worker priority = %d, want %d", workerPriority, want)
	}
}

func TestDeprioritizeExitedProcess(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	Configure(t.Context(), cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := Deprioritize(cmd); err != nil {
		t.Fatalf("Deprioritize() error = %v, want exited process ignored", err)
	}
}

func TestAliveRejectsStaleProcessIdentity(t *testing.T) {
	proc := startSleep(t)
	identity, err := Inspect(proc.cmd)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}

	tests := []struct {
		name     string
		identity Identity
		want     bool
	}{
		{name: "matching identity", identity: identity, want: true},
		{name: "stale start time", identity: Identity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt.Add(time.Second)}},
		{name: "missing start time", identity: Identity{PID: identity.PID, GroupID: identity.GroupID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Alive(tt.identity)
			if err != nil {
				t.Fatalf("Alive() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Alive() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObserveMeasuresMatchingProcessGroupAndRejectsStaleIdentity(t *testing.T) {
	proc := startSleepGroup(t)
	identity, err := Inspect(proc.cmd)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	tests := []struct {
		name      string
		identity  Identity
		wantAlive bool
		wantStale bool
	}{
		{name: "matching process group", identity: identity, wantAlive: true},
		{name: "stale process identity", identity: Identity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt.Add(time.Second)}, wantStale: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observations, err := Observe([]Identity{tt.identity})
			if err != nil {
				t.Fatalf("Observe() error = %v", err)
			}
			if len(observations) != 1 {
				t.Fatalf("Observe() len = %d, want 1", len(observations))
			}
			got := observations[0]
			if got.Alive != tt.wantAlive || got.Stale != tt.wantStale {
				t.Fatalf("Observe() = %#v, want alive=%t stale=%t", got, tt.wantAlive, tt.wantStale)
			}
			if tt.wantAlive && (got.ProcessCount < 2 || got.RSSBytes <= 0) {
				t.Fatalf("Observe() = %#v, want group count and RSS", got)
			}
		})
	}
}

func TestTerminateTree(t *testing.T) {
	tests := []struct {
		name string
		cmd  func(t *testing.T) (*exec.Cmd, int, func(t *testing.T))
	}{
		{
			name: "nil command and zero group",
			cmd: func(*testing.T) (*exec.Cmd, int, func(t *testing.T)) {
				return nil, 0, nil
			},
		},
		{
			name: "live process group",
			cmd: func(t *testing.T) (*exec.Cmd, int, func(t *testing.T)) {
				proc := startSleepGroup(t)
				return proc.cmd, GroupID(proc.cmd), func(t *testing.T) {
					assertProcessGroupKilled(t, proc)
				}
			},
		},
		{
			name: "already exited process group",
			cmd: func(t *testing.T) (*exec.Cmd, int, func(t *testing.T)) {
				cmd, pgid := startExitedCommand(t)
				return cmd, pgid, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, pgid, wait := tt.cmd(t)

			if err := TerminateTree(cmd, pgid); err != nil {
				t.Fatalf("TerminateTree() error = %v, want nil", err)
			}
			if wait != nil {
				wait(t)
			}
		})
	}
}

func TestTerminateTreeDisambiguatesEPERM(t *testing.T) {
	inspectErr := errors.New("inspect failed")
	tests := []struct {
		name       string
		members    []processGroupMember
		inspectErr error
		probeErr   error
		wantProbe  bool
		wantErr    error
	}{
		{name: "exited process group", probeErr: syscall.ESRCH, wantProbe: true},
		{name: "zombie-only process group", members: []processGroupMember{{state: "Z"}}},
		{name: "live unauthorized process group", members: []processGroupMember{{state: "S"}}, wantErr: syscall.EPERM},
		{name: "hidden live unauthorized process group", probeErr: syscall.EPERM, wantProbe: true, wantErr: syscall.EPERM},
		{name: "empty snapshot with existing process group", wantProbe: true, wantErr: syscall.EPERM},
		{name: "failed process group inspection", inspectErr: inspectErr, wantErr: syscall.EPERM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var signals []syscall.Signal
			signal := func(pid int, signal syscall.Signal) error {
				if pid != -123 {
					t.Fatalf("signal target = %d, want -123", pid)
				}
				signals = append(signals, signal)
				switch signal {
				case syscall.SIGKILL:
					return syscall.EPERM
				case 0:
					return tt.probeErr
				default:
					t.Fatalf("signal = %v, want %v or 0", signal, syscall.SIGKILL)
					return nil
				}
			}
			inspect := func(processGroupID int) ([]processGroupMember, error) {
				if processGroupID != 123 {
					t.Fatalf("inspect process group = %d, want 123", processGroupID)
				}
				return tt.members, tt.inspectErr
			}

			err := terminateTree(nil, 123, signal, inspect)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("terminateTree() error = %v, want %v", err, tt.wantErr)
			}
			wantSignals := 1
			if tt.wantProbe {
				wantSignals++
			}
			if len(signals) != wantSignals {
				t.Fatalf("signal calls = %v, want %d calls", signals, wantSignals)
			}
			if tt.wantProbe && signals[1] != 0 {
				t.Fatalf("second signal = %v, want 0", signals[1])
			}
		})
	}
}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name string
		pgid func(t *testing.T) (int, func(t *testing.T))
	}{
		{
			name: "zero group",
			pgid: func(*testing.T) (int, func(t *testing.T)) {
				return 0, nil
			},
		},
		{
			name: "negative group",
			pgid: func(*testing.T) (int, func(t *testing.T)) {
				return -1, nil
			},
		},
		{
			name: "nonexistent group",
			pgid: func(*testing.T) (int, func(t *testing.T)) {
				return 1 << 30, nil
			},
		},
		{
			name: "live group",
			pgid: func(t *testing.T) (int, func(t *testing.T)) {
				proc := startSleepGroup(t)
				go func() { _ = proc.Wait() }()
				go func() { _ = proc.WaitGroupMember() }()
				return GroupID(proc.cmd), func(t *testing.T) {
					assertProcessGroupKilled(t, proc)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgid, wait := tt.pgid(t)

			if err := Cleanup(pgid); err != nil {
				t.Fatalf("Cleanup() error = %v, want nil", err)
			}
			if wait != nil {
				wait(t)
			}
		})
	}
}

func TestCleanupWaitsForOrphanedGroupMembers(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "single descendant", command: "sleep 30 &"},
		{name: "multiple descendants", command: "sleep 30 & sleep 30 &"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "sh", "-c", tt.command)
			Configure(t.Context(), cmd)
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			pgid := GroupID(cmd)
			if err := cmd.Wait(); err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
			t.Cleanup(func() {
				_ = TerminateTree(nil, pgid)
			})

			if !processTargetAlive(0, pgid) {
				t.Fatal("process group exited before Cleanup()")
			}
			if err := Cleanup(pgid); err != nil {
				t.Fatalf("Cleanup() error = %v", err)
			}
			if processTargetAlive(0, pgid) {
				t.Fatal("Cleanup() returned while process group was still alive")
			}
		})
	}
}

func TestCleanupTreatsZombieOnlyGroupAsExited(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pgid := GroupID(cmd)
	t.Cleanup(func() { _ = cmd.Wait() })

	if err := Cleanup(pgid); err != nil {
		t.Fatalf("Cleanup() error = %v, want zombie-only group ignored", err)
	}
	if !processTargetAlive(0, pgid) {
		t.Fatal("process group no longer exists, want unreaped zombie to remain observable")
	}
	assertKilled(t, cmd.Wait())
}

func TestProcessGroupExited(t *testing.T) {
	tests := []struct {
		name    string
		members []processGroupMember
		want    bool
	}{
		{name: "no members", want: true},
		{name: "single zombie", members: []processGroupMember{{state: "Z"}}, want: true},
		{name: "zombie with flags", members: []processGroupMember{{state: "Z+"}}, want: true},
		{name: "multiple zombies", members: []processGroupMember{{state: "Z"}, {state: "Zs"}}, want: true},
		{name: "running member", members: []processGroupMember{{state: "R"}}},
		{name: "mixed group", members: []processGroupMember{{state: "Z"}, {state: "S"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processGroupExited(tt.members); got != tt.want {
				t.Fatalf("processGroupExited() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInspectAndTerminate(t *testing.T) {
	tests := []struct {
		name        string
		identity    func(Identity) Identity
		wantOutcome TerminationOutcome
		wantAlive   bool
	}{
		{
			name:        "matching process group",
			identity:    func(identity Identity) Identity { return identity },
			wantOutcome: TerminationOutcomeTerminated,
		},
		{
			name: "stale process start time",
			identity: func(identity Identity) Identity {
				identity.StartedAt = identity.StartedAt.Add(time.Second)
				return identity
			},
			wantOutcome: TerminationOutcomeStaleIdentity,
			wantAlive:   true,
		},
		{
			name: "stale process group",
			identity: func(identity Identity) Identity {
				identity.GroupID++
				return identity
			},
			wantOutcome: TerminationOutcomeStaleIdentity,
			wantAlive:   true,
		},
		{
			name: "missing process group",
			identity: func(identity Identity) Identity {
				identity.GroupID = 0
				return identity
			},
			wantOutcome: TerminationOutcomeStaleIdentity,
			wantAlive:   true,
		},
		{
			name: "missing process start time",
			identity: func(identity Identity) Identity {
				identity.StartedAt = time.Time{}
				return identity
			},
			wantOutcome: TerminationOutcomeStaleIdentity,
			wantAlive:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := startSleepGroup(t)
			identity, err := Inspect(proc.cmd)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if identity.PID != proc.cmd.Process.Pid || identity.GroupID != GroupID(proc.cmd) || identity.StartedAt.IsZero() {
				t.Fatalf("Inspect() = %#v", identity)
			}
			if !tt.wantAlive {
				go func() { _ = proc.Wait() }()
				go func() { _ = proc.WaitGroupMember() }()
			}

			outcome, err := Terminate(context.Background(), tt.identity(identity), 50*time.Millisecond)
			if err != nil {
				t.Fatalf("Terminate() error = %v", err)
			}
			if outcome != tt.wantOutcome {
				t.Fatalf("Terminate() outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if tt.wantAlive {
				if !processTargetAlive(identity.PID, identity.GroupID) {
					t.Fatal("Terminate() killed a process with a stale identity")
				}
				return
			}
			_ = waitForExit(t, proc.Wait)
			_ = waitForExit(t, proc.WaitGroupMember)
		})
	}
}

func TestTerminateEscalatesSurvivingProcessGroup(t *testing.T) {
	proc := startIgnoringTerminationGroup(t)
	identity, err := Inspect(proc.cmd)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	go func() { _ = proc.Wait() }()
	go func() { _ = proc.WaitGroupMember() }()

	outcome, err := Terminate(context.Background(), identity, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}
	if outcome != TerminationOutcomeKilled {
		t.Fatalf("Terminate() outcome = %q, want %q", outcome, TerminationOutcomeKilled)
	}
	assertKilled(t, waitForExit(t, proc.Wait))
	assertKilled(t, waitForExit(t, proc.WaitGroupMember))
}

type startedProcess struct {
	cmd           *exec.Cmd
	wait          sync.Once
	waitErr       error
	groupMember   *exec.Cmd
	memberWait    sync.Once
	memberWaitErr error
}

func startSleep(t *testing.T) *startedProcess {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}

	proc := &startedProcess{cmd: cmd}
	t.Cleanup(func() {
		if cmd.ProcessState == nil || (proc.groupMember != nil && proc.groupMember.ProcessState == nil) {
			if err := TerminateTree(cmd, GroupID(cmd)); err != nil {
				t.Fatalf("TerminateTree() cleanup error = %v, want nil", err)
			}
			if proc.groupMember != nil && proc.groupMember.ProcessState == nil {
				_ = proc.groupMember.Process.Kill()
			}
		}
		_ = proc.Wait()
		if proc.groupMember != nil {
			_ = proc.WaitGroupMember()
		}
	})

	return proc
}

func startSleepGroup(t *testing.T) *startedProcess {
	t.Helper()

	proc := startSleep(t)
	pgid := GroupID(proc.cmd)

	member := exec.CommandContext(context.Background(), "sleep", "30")
	member.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    pgid,
	}
	if err := member.Start(); err != nil {
		if killErr := TerminateTree(proc.cmd, pgid); killErr != nil {
			t.Fatalf("TerminateTree() cleanup error = %v, want nil", killErr)
		}
		_ = proc.Wait()
		t.Fatalf("Start() group member error = %v, want nil", err)
	}

	proc.groupMember = member
	if got := GroupID(member); got != pgid {
		if killErr := TerminateTree(proc.cmd, pgid); killErr != nil {
			t.Fatalf("TerminateTree() cleanup error = %v, want nil", killErr)
		}
		_ = proc.Wait()
		_ = proc.WaitGroupMember()
		t.Fatalf("group member pgid = %d, want %d", got, pgid)
	}

	return proc
}

func startIgnoringTerminationGroup(t *testing.T) *startedProcess {
	t.Helper()

	readyDir := t.TempDir()
	leaderReady := readyDir + "/leader.ready"
	memberReady := readyDir + "/member.ready"
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "trap '' TERM; : > \"$READY_PATH\"; exec sleep 30")
	cmd.Env = append(os.Environ(), "READY_PATH="+leaderReady)
	Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	proc := &startedProcess{cmd: cmd}
	pgid := GroupID(cmd)
	member := exec.CommandContext(context.Background(), "sh", "-c", "trap '' TERM; : > \"$READY_PATH\"; exec sleep 30")
	member.Env = append(os.Environ(), "READY_PATH="+memberReady)
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := member.Start(); err != nil {
		_ = TerminateTree(cmd, pgid)
		_ = proc.Wait()
		t.Fatalf("Start() group member error = %v", err)
	}
	proc.groupMember = member
	waitForProcessReadyFile(t, leaderReady)
	waitForProcessReadyFile(t, memberReady)
	t.Cleanup(func() {
		_ = TerminateTree(cmd, pgid)
		_ = proc.Wait()
		_ = proc.WaitGroupMember()
	})
	return proc
}

func waitForProcessReadyFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for process readiness at %s", path)
}

func (p *startedProcess) Wait() error {
	p.wait.Do(func() {
		p.waitErr = p.cmd.Wait()
	})
	return p.waitErr
}

func (p *startedProcess) WaitGroupMember() error {
	p.memberWait.Do(func() {
		p.memberWaitErr = p.groupMember.Wait()
	})
	return p.memberWaitErr
}

func startExitedCommand(t *testing.T) (*exec.Cmd, int) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "true")
	Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}

	pgid := GroupID(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	return cmd, pgid
}

func assertProcessGroupKilled(t *testing.T, proc *startedProcess) {
	t.Helper()

	assertKilled(t, waitForExit(t, proc.Wait))
	assertKilled(t, waitForExit(t, proc.WaitGroupMember))
}

func waitForExit(t *testing.T, wait func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() timed out, want process exit")
		return nil
	}
}

func assertKilled(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("Wait() error = nil, want process killed by signal")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait() error = %T, want *exec.ExitError", err)
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("Wait() status = %T, want syscall.WaitStatus", exitErr.Sys())
	}
	if !status.Signaled() {
		t.Fatalf("Wait() status = %v, want signaled process", status)
	}
	if status.Signal() != syscall.SIGKILL {
		t.Fatalf("Wait() signal = %v, want %v", status.Signal(), syscall.SIGKILL)
	}
}
