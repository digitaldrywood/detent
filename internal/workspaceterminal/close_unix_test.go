//go:build unix

package workspaceterminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
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
		exit          bool
		sandbox       bool
		wantFullGrace bool
	}{
		{name: "close with a child ignoring SIGHUP", child: true, wantFullGrace: true},
		{name: "context cancellation with a child ignoring SIGHUP", child: true, cancel: true, wantFullGrace: true},
		{name: "close with no children", child: false},
		{name: "context cancellation with no children", child: false, cancel: true},
		{name: "shell exit kills a surviving child", child: true, exit: true, wantFullGrace: true},
		{name: "confined child is killed on close", child: true, sandbox: true, wantFullGrace: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()
			service := newTestService(t, worktree)
			if test.sandbox {
				if AvailableIsolation() != workspacesession.IsolationSandbox {
					t.Skip("actual host sandbox enforcement is unavailable")
				}
				var err error
				service, err = New(t.Context(), worktree, "/bin/sh", workspacesession.IsolationSandbox, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
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
				// Split the marker so echoed input cannot report readiness before
				// the shell has executed the command.
				if err := terminal.Write([]byte("printf '%s%s\\n' rea dy\n")); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
				waitFor(t, "the shell", func() bool { return strings.Contains(sink.text(), "ready") })
			}

			started := time.Now()
			if test.exit {
				if err := terminal.Write([]byte("exit\n")); err != nil {
					t.Fatal(err)
				}
				terminal.Wait()
			} else if test.cancel {
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

// TestTerminalCloseReleasesInputAfterHangup models a shell that receives SIGHUP
// but cannot complete its exit until a terminal read returns.
func TestTerminalCloseReleasesInputAfterHangup(t *testing.T) {
	t.Parallel()
	requirePTY(t)
	tests := []struct {
		name   string
		cancel bool
	}{
		{name: "explicit close"},
		{name: "context cancellation", cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			// A script accepts the service's interactive flag as a positional
			// argument, then exec replaces it with the helper: no child remains.
			helper := filepath.Join(worktree, "shell")
			quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
			script := "#!/bin/sh\nexec " + quoted + " -test.run='^TestTerminalCloseInputHelper$' -- detent-terminal-input-helper\n"
			if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			service := newTestService(t, worktree)
			service.shell = helper
			sink := &collector{}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			terminal, err := service.Open(ctx, workspacesession.TerminalOpen{Cols: 80, Rows: 24}, sink.emit)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(terminal.Close)
			waitFor(t, "the helper's hangup handler", func() bool {
				return strings.Contains(sink.text(), "helper-ready")
			})

			started := time.Now()
			if test.cancel {
				cancel()
			} else {
				terminal.Close()
			}
			select {
			case <-terminal.Done():
			case <-time.After(KillGrace + 20*time.Second):
				t.Fatal("the helper did not exit")
			}
			if result := terminal.Wait(); result.ExitCode != 0 || result.Signal != "" {
				t.Fatalf("Wait() = %+v, want the helper to finish after its terminal read", result)
			}
			if elapsed := time.Since(started); elapsed >= KillGrace {
				t.Fatalf("terminal took %v to release input with no child", elapsed)
			}
		})
	}
}

func TestTerminalCloseInputHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "detent-terminal-input-helper" {
		return
	}
	hangups := make(chan os.Signal, 2)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	if _, err := fmt.Fprintln(os.Stdout, "helper-ready"); err != nil {
		t.Fatal(err)
	}
	<-hangups
	var input [1]byte
	if _, err := os.Stdin.Read(input[:]); err == nil {
		t.Fatal("terminal input stayed open after the hangup")
	}
	// The PTY is closed now. testing's coverage teardown writes to stdout and
	// exits 2 if that write fails, even though the terminal read was released.
	// Keep the replacement open until this helper process exits.
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = output
}
