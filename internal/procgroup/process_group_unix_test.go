//go:build unix

package procgroup

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"testing"
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
			Configure(tt.cmd)

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

func TestTerminateTree(t *testing.T) {
	tests := []struct {
		name string
		cmd  func(t *testing.T) (*exec.Cmd, int, func() error)
	}{
		{
			name: "nil command and zero group",
			cmd: func(*testing.T) (*exec.Cmd, int, func() error) {
				return nil, 0, nil
			},
		},
		{
			name: "live process group",
			cmd: func(t *testing.T) (*exec.Cmd, int, func() error) {
				proc := startSleep(t)
				return proc.cmd, GroupID(proc.cmd), proc.Wait
			},
		},
		{
			name: "already exited process group",
			cmd: func(t *testing.T) (*exec.Cmd, int, func() error) {
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
				assertKilled(t, wait())
			}
		})
	}
}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name string
		pgid func(t *testing.T) (int, func() error)
	}{
		{
			name: "zero group",
			pgid: func(*testing.T) (int, func() error) {
				return 0, nil
			},
		},
		{
			name: "negative group",
			pgid: func(*testing.T) (int, func() error) {
				return -1, nil
			},
		},
		{
			name: "nonexistent group",
			pgid: func(*testing.T) (int, func() error) {
				return 1 << 30, nil
			},
		},
		{
			name: "live group",
			pgid: func(t *testing.T) (int, func() error) {
				proc := startSleep(t)
				return GroupID(proc.cmd), proc.Wait
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
				assertKilled(t, wait())
			}
		})
	}
}

type startedProcess struct {
	cmd     *exec.Cmd
	wait    sync.Once
	waitErr error
}

func startSleep(t *testing.T) *startedProcess {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}

	proc := &startedProcess{cmd: cmd}
	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}
		if err := TerminateTree(cmd, GroupID(cmd)); err != nil {
			t.Fatalf("TerminateTree() cleanup error = %v, want nil", err)
		}
		_ = proc.Wait()
	})

	return proc
}

func (p *startedProcess) Wait() error {
	p.wait.Do(func() {
		p.waitErr = p.cmd.Wait()
	})
	return p.waitErr
}

func startExitedCommand(t *testing.T) (*exec.Cmd, int) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "true")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}

	pgid := GroupID(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	return cmd, pgid
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
