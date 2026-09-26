//go:build unix

package workspaceterminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestTerminalCloseKillsChildrenThatIgnoreTheHangup(t *testing.T) {
	t.Parallel()
	requirePTY(t)
	tests := []struct {
		name          string
		child         bool
		cancel        bool
		wantFullGrace bool
	}{
		{name: "close with a child ignoring SIGHUP", child: true, wantFullGrace: true},
		{name: "context cancellation with a child ignoring SIGHUP", child: true, cancel: true, wantFullGrace: true},
		{name: "close with no children", child: false},
		{name: "context cancellation with no children", child: false, cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()
			service := newTestService(t, worktree)
			sink := &collector{}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			terminal, err := service.Open(ctx, workspacesession.TerminalOpen{Cols: 80, Rows: 24}, sink.emit)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(terminal.Close)

			childPID := 0
			if test.child {
				pidFile := filepath.Join(worktree, "child.pid")
				command := "set +m; sh -c 'trap \"\" HUP; echo $$ > " + pidFile + "; exec sleep 60' &\n"
				if err := terminal.Write([]byte(command)); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
				waitFor(t, "the child's pid", func() bool {
					data, err := os.ReadFile(pidFile)
					if err != nil {
						return false
					}
					childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
					return err == nil && childPID > 0
				})
			} else {
				if err := terminal.Write([]byte("echo ready\n")); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
				waitFor(t, "the shell", func() bool { return strings.Contains(sink.text(), "ready") })
			}

			started := time.Now()
			if test.cancel {
				cancel()
				select {
				case <-terminal.Done():
				case <-time.After(KillGrace + 20*time.Second):
					t.Fatal("a cancelled context did not end the shell")
				}
				if childPID > 0 {
					waitFor(t, "the child to be killed", func() bool {
						return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
					})
				}
			} else {
				terminal.Close()
			}
			elapsed := time.Since(started)
			if test.wantFullGrace && elapsed < KillGrace {
				t.Fatalf("Close() returned after %v, before the grace ran out on a surviving child", elapsed)
			}
			if !test.wantFullGrace && elapsed >= KillGrace {
				t.Fatalf("Close() took %v with nothing left to kill", elapsed)
			}
			if childPID > 0 {
				waitFor(t, "the child to be killed", func() bool {
					return errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH)
				})
			}
		})
	}
}
