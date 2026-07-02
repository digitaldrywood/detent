//go:build unix

package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestRunTurnCancellationKillsChildProcessGroup(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	backend := newTestBackend(t, Options{
		CommandFactory: func(ctx context.Context) *exec.Cmd {
			script := "sleep 3600 & printf '%s\n' \"$!\" > " + shellQuote(pidPath) + "; wait"
			return exec.CommandContext(ctx, "sh", "-c", script)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	workspace := t.TempDir()
	errCh := make(chan error, 1)
	go func() {
		_, err := backend.RunTurn(ctx, runner.AgentTurnRequest{
			Workspace: workspace,
			Prompt:    "cancel",
			Model:     "fable",
		}, nil)
		errCh <- err
	}()

	pid := waitForPIDFile(t, pidPath)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunTurn() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunTurn() did not return after context cancellation")
	}
	waitForProcessExit(t, pid)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.After(time.Second)
	var lastErr error
	var lastRaw string
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			lastRaw = string(raw)
			pid, parseErr := strconv.Atoi(strings.TrimSpace(lastRaw))
			if parseErr == nil && pid > 0 {
				return pid
			}
			if parseErr != nil {
				lastErr = parseErr
			} else {
				lastErr = errors.New("pid is not positive")
			}
		} else {
			lastErr = err
		}

		select {
		case <-deadline:
			if lastRaw != "" {
				t.Fatalf("timed out waiting for parseable pid file, last value %q: %v", lastRaw, lastErr)
			}
			t.Fatalf("timed out waiting for pid file: %v", lastErr)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		if !processAlive(pid) {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("process %d is still alive", pid)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
