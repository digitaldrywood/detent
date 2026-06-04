//go:build unix

package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocalTransportReapsChildAfterParentExits(t *testing.T) {
	t.Parallel()

	pidPath := t.TempDir() + "/child.pid"
	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 3600 >/dev/null 2>&1 & printf '%s\n' \"$!\" > "+shellQuote(pidPath))
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	pid := waitForPIDFile(t, pidPath)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitForProcessExit(t, pid)
}

func TestLocalTransportCloseKillsChildProcessGroup(t *testing.T) {
	t.Parallel()

	pidPath := t.TempDir() + "/child.pid"
	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 3600 & printf '%s\n' \"$!\" > "+shellQuote(pidPath)+"; wait")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	pid := waitForPIDFile(t, pidPath)

	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := transport.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context deadline exceeded", err)
	}
	waitForProcessExit(t, pid)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.After(time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr != nil {
				t.Fatalf("parse pid file: %v", parseErr)
			}
			if pid <= 0 {
				t.Fatalf("pid = %d, want positive", pid)
			}
			return pid
		}

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for pid file: %v", err)
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
