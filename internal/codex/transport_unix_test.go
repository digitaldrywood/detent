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

const lifecycleWaitTimeout = 10 * time.Second

func TestLocalTransportReapsChildAfterParentExits(t *testing.T) {
	t.Parallel()

	for _, startup := range []struct {
		name  string
		delay string
	}{
		{name: "immediate", delay: "0"},
		{name: "delayed readiness", delay: "1.2"},
	} {
		t.Run(startup.name, func(t *testing.T) {
			t.Parallel()

			pidPath := t.TempDir() + "/child.pid"
			factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "sleep "+startup.delay+"; sleep 3600 >/dev/null 2>&1 & printf '%s\n' \"$!\" > "+shellQuote(pidPath)+"; read release")
			})
			if err != nil {
				t.Fatalf("NewLocalTransportFactory() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			transport, err := factory.NewTransport(ctx)
			if err != nil {
				t.Fatalf("NewTransport() error = %v", err)
			}
			t.Cleanup(func() {
				cancel()
				closeCtx, closeCancel := context.WithTimeout(context.Background(), lifecycleWaitTimeout)
				defer closeCancel()
				_ = transport.Close(closeCtx)
				select {
				case <-transport.(*localTransport).done:
				case <-closeCtx.Done():
					t.Error("transport cleanup did not finish")
				}
			})
			pid := waitForPIDFile(t, pidPath)

			if !processAlive(pid) {
				t.Fatalf("child process %d exited before parent release", pid)
			}
			if _, err := transport.(*localTransport).stdin.Write([]byte("release\n")); err != nil {
				t.Fatal(err)
			}
			closeCtx, closeCancel := context.WithTimeout(context.Background(), lifecycleWaitTimeout)
			defer closeCancel()
			if err := transport.Close(closeCtx); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if processAlive(pid) {
				t.Fatalf("Close() returned while child process %d was still alive", pid)
			}
		})
	}
}

func TestLocalTransportCloseKillsChildProcessGroup(t *testing.T) {
	t.Parallel()

	for _, startup := range []struct {
		name  string
		delay string
	}{
		{name: "immediate", delay: "0"},
		{name: "delayed readiness", delay: "1.2"},
	} {
		t.Run(startup.name, func(t *testing.T) {
			t.Parallel()

			pidPath := t.TempDir() + "/child.pid"
			factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "sleep "+startup.delay+"; sleep 3600 & printf '%s\n' \"$!\" > "+shellQuote(pidPath)+"; wait")
			})
			if err != nil {
				t.Fatalf("NewLocalTransportFactory() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			transport, err := factory.NewTransport(ctx)
			if err != nil {
				t.Fatalf("NewTransport() error = %v", err)
			}
			t.Cleanup(func() {
				cancel()
				closeCtx, closeCancel := context.WithTimeout(context.Background(), lifecycleWaitTimeout)
				defer closeCancel()
				_ = transport.Close(closeCtx)
				select {
				case <-transport.(*localTransport).done:
				case <-closeCtx.Done():
					t.Error("transport cleanup did not finish")
				}
			})
			pid := waitForPIDFile(t, pidPath)
			if !processAlive(pid) {
				t.Fatalf("child process %d exited before Close", pid)
			}

			closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer closeCancel()
			if err := transport.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Close() error = %v, want context deadline exceeded", err)
			}
			waitForProcessExit(t, pid)
		})
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.After(lifecycleWaitTimeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	lastRaw := ""
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			lastRaw = string(raw)
			pidText := strings.TrimSpace(lastRaw)
			pid, parseErr := strconv.Atoi(pidText)
			if parseErr == nil && pid > 0 {
				t.Cleanup(func() {
					if !processAlive(pid) {
						return
					}
					if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
						t.Errorf("clean up child process %d: %v", pid, err)
						return
					}
					waitForProcessExit(t, pid)
				})
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
		case <-ticker.C:
		}
	}
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.After(lifecycleWaitTimeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !processAlive(pid) {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("process %d is still alive", pid)
		case <-ticker.C:
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
