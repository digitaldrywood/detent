//go:build unix

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerSessionCancellationReapsEscapedWorkspaceProcess(t *testing.T) {
	workspacePath := t.TempDir()
	lockPath := filepath.Join(workspacePath, "gate.lock")
	durationLimit := &controlledDurationLimit{}
	command, readyReader, readyWriter := workspaceLockCommand(t, workspacePath, lockPath)
	backend := &orphanLockAgentBackend{
		command:       command,
		readyReader:   readyReader,
		readyWriter:   readyWriter,
		expireSession: durationLimit.Expire,
	}
	t.Cleanup(func() {
		if backend.command.Process != nil {
			_ = backend.command.Process.Kill()
		}
		if backend.waitDone != nil {
			<-backend.waitDone
		}
	})
	var reaped int
	var reapErr error

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{Agent: config.Agent{MaxSessionDurationMS: 25}},
			Prompt: "Work",
		},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: workspacePath, Key: "issue-orphan-lock"},
		},
		AgentBackend:    backend,
		sessionLimit:    durationLimit.Context,
		WorkerReapGrace: 5 * time.Second,
		ReapWorkspaceProcesses: func(ctx context.Context, path string, grace time.Duration) (int, error) {
			reaped, reapErr = workspace.ReapProcesses(ctx, path, grace)
			return reaped, reapErr
		},
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, runErr := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-orphan-lock",
			Identifier: "digitaldrywood/detent#2081",
		},
	})
	if !errors.Is(runErr, ErrSessionDurationExceeded) {
		t.Fatalf("Run() error = %v, want ErrSessionDurationExceeded", runErr)
	}
	if reapErr != nil || reaped != 1 {
		t.Fatalf("workspace reap = (%d, %v), want (1, nil)", reaped, reapErr)
	}

	select {
	case <-backend.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace lock holder survived session cancellation")
	}
	assertLockAvailable(t, lockPath)
}

func TestWorkspaceLockProcess(t *testing.T) {
	if os.Getenv("DETENT_WORKSPACE_LOCK_HELPER") != "1" {
		return
	}

	lockFile, err := os.OpenFile(os.Getenv("DETENT_WORKSPACE_LOCK_PATH"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	ready := os.NewFile(3, "ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatalf("signal readiness: %v", err)
	}
	if err := ready.Close(); err != nil {
		t.Fatalf("close readiness pipe: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

type orphanLockAgentBackend struct {
	command       *exec.Cmd
	readyReader   *os.File
	readyWriter   *os.File
	expireSession func()
	waitDone      chan struct{}
}

func (b *orphanLockAgentBackend) RunTurn(ctx context.Context, _ AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	if err := b.command.Start(); err != nil {
		return AgentTurnResult{}, err
	}
	if err := b.readyWriter.Close(); err != nil {
		return AgentTurnResult{}, err
	}
	b.waitDone = make(chan struct{})
	go func() {
		_ = b.command.Wait()
		close(b.waitDone)
	}()
	if _, err := io.ReadFull(b.readyReader, make([]byte, 1)); err != nil {
		return AgentTurnResult{}, err
	}
	b.expireSession()
	<-ctx.Done()
	return AgentTurnResult{}, ctx.Err()
}

func workspaceLockCommand(t *testing.T, workspacePath string, lockPath string) (*exec.Cmd, *os.File, *os.File) {
	t.Helper()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create readiness pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = readyReader.Close()
		_ = readyWriter.Close()
	})
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestWorkspaceLockProcess$")
	cmd.Dir = workspacePath
	cmd.Env = append(os.Environ(),
		"DETENT_WORKSPACE_LOCK_HELPER=1",
		"DETENT_WORKSPACE_LOCK_PATH="+lockPath,
	)
	cmd.ExtraFiles = []*os.File{readyWriter}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, readyReader, readyWriter
}

func assertLockAvailable(t *testing.T, path string) {
	t.Helper()
	lockFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open gate lock: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("gate lock is unavailable after cancellation: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock gate file: %v", err)
	}
}
