//go:build unix

package workspaceterminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func requirePTY(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Fatal("this platform has no /bin/sh to open a terminal on")
	}
}

func TestTerminalOpenAndInput(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	service := newTestService(t, t.TempDir())
	sink := &collector{}
	terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{Cols: 80, Rows: 24}, sink.emit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(terminal.Close)

	if terminal.PID() <= 0 {
		t.Fatalf("PID() = %d, want a live process", terminal.PID())
	}
	if terminal.Isolation() != workspacesession.IsolationUser {
		t.Fatalf("Isolation() = %q, want user", terminal.Isolation())
	}
	if cols, rows := terminal.Size(); cols != 80 || rows != 24 {
		t.Fatalf("Size() = %d x %d, want 80 x 24", cols, rows)
	}

	if err := terminal.Write([]byte("echo detent-hi\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	waitFor(t, "the echoed output", func() bool { return strings.Contains(sink.text(), "detent-hi") })
}

func TestTerminalExitCodes(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	tests := []struct {
		name     string
		input    string
		wantCode int
	}{
		{name: "exit zero", input: "exit 0\n", wantCode: 0},
		{name: "exit non-zero", input: "exit 7\n", wantCode: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := newTestService(t, t.TempDir())
			sink := &collector{}
			terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{Cols: 80, Rows: 24}, sink.emit)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(terminal.Close)

			if err := terminal.Write([]byte(tt.input)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			result := terminal.Wait()
			if result.ExitCode != tt.wantCode {
				t.Fatalf("Wait().ExitCode = %d, want %d", result.ExitCode, tt.wantCode)
			}
			if result.Signal != "" {
				t.Fatalf("Wait().Signal = %q, want none for a shell that exited", result.Signal)
			}
			// A terminal that has ended refuses further input rather than
			// writing into a closed PTY, so a client that types after the exit
			// frame is told rather than ignored.
			if err := terminal.Write([]byte("echo late\n")); ErrorCode(err) != workspacesession.CodeNotFound {
				t.Fatalf("Write() after exit code = %q, want %q", ErrorCode(err), workspacesession.CodeNotFound)
			}
		})
	}
}

func TestTerminalResize(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	service := newTestService(t, t.TempDir())
	sink := &collector{}
	terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{Cols: 80, Rows: 24}, sink.emit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(terminal.Close)

	if err := terminal.Resize(120, 40); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if cols, rows := terminal.Size(); cols != 120 || rows != 40 {
		t.Fatalf("Size() = %d x %d, want 120 x 40", cols, rows)
	}
	// The resize is a real ioctl and not bookkeeping: the shell is asked what
	// the kernel thinks the window is, which is the only answer that proves it.
	if err := terminal.Write([]byte("stty size\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	waitFor(t, "the resized window to be visible to the shell", func() bool {
		return strings.Contains(sink.text(), "40 120")
	})
}

func TestTerminalCloseEndsTheShell(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	service := newTestService(t, t.TempDir())
	sink := &collector{}
	terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{}, sink.emit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	// The default window is what an open with no size takes, so a client that
	// has not measured its container yet still gets a usable terminal.
	if cols, rows := terminal.Size(); cols != workspacesession.DefaultTerminalCols || rows != workspacesession.DefaultTerminalRows {
		t.Fatalf("Size() = %d x %d, want the defaults", cols, rows)
	}

	terminal.Close()
	select {
	case <-terminal.Done():
	case <-time.After(KillGrace + 20*time.Second):
		t.Fatal("Close() did not end the shell")
	}
	// Close is idempotent: a workspace ending while a person is closing a tab
	// reaches it twice, and the second call must not signal a pid the operating
	// system may have handed to somebody else.
	terminal.Close()
	if err := terminal.Resize(80, 24); ErrorCode(err) != workspacesession.CodeNotFound {
		t.Fatalf("Resize() after close code = %q, want %q", ErrorCode(err), workspacesession.CodeNotFound)
	}
}

func TestTerminalContextCancellationEndsTheShell(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	service := newTestService(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	terminal, err := service.Open(ctx, workspacesession.TerminalOpen{Cols: 80, Rows: 24}, (&collector{}).emit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(terminal.Close)

	cancel()
	select {
	case <-terminal.Done():
	case <-time.After(KillGrace + 20*time.Second):
		t.Fatal("a cancelled context did not end the shell")
	}
}

func TestTerminalRefusedOutputLeavesTheShellRunning(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	service := newTestService(t, t.TempDir())
	sink := &collector{}
	terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{Cols: 80, Rows: 24}, sink.emit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(terminal.Close)

	// A refused span means the reader's connection has gone. The relay protocol gives
	// them 60 seconds to come back, so the shell must survive it: killing here
	// would make the resume window a promise the runner does not keep.
	sink.refuseSpans(errors.New("the person's connection has gone"))
	if err := terminal.Write([]byte("echo gone\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	select {
	case <-terminal.Done():
		t.Fatal("a refused output span ended the shell")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestServiceOpenRefusals(t *testing.T) {
	t.Parallel()
	requirePTY(t)

	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, "pkg"), 0o750); err != nil {
		t.Fatalf("make directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(worktree, "escape")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		request  workspacesession.TerminalOpen
		wantCode string
	}{
		{name: "a directory inside the worktree opens", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "pkg"}},
		{name: "an absolute cwd is refused", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "/etc"}, wantCode: workspacesession.CodeForbidden},
		{name: "a cwd that escapes is refused", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "../elsewhere"}, wantCode: workspacesession.CodeForbidden},
		{name: "a symlink cwd escape is refused", request: workspacesession.TerminalOpen{Cwd: "escape"}, wantCode: workspacesession.CodeForbidden},
		{name: "a home-relative cwd is refused", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "~/secrets"}, wantCode: workspacesession.CodeForbidden},
		{name: "a missing cwd is not found", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "absent"}, wantCode: workspacesession.CodeNotFound},
		{name: "a file is not a cwd", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "go.mod"}, wantCode: workspacesession.CodeNotFound},
		{name: "an oversized window is refused", request: workspacesession.TerminalOpen{Cols: 99999, Rows: 24}, wantCode: workspacesession.CodeInvalidFrame},
		{name: "a negative window is refused", request: workspacesession.TerminalOpen{Cols: -1, Rows: 24}, wantCode: workspacesession.CodeInvalidFrame},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := newTestService(t, worktree)
			terminal, err := service.Open(t.Context(), tt.request, (&collector{}).emit)
			if terminal != nil {
				t.Cleanup(terminal.Close)
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Open() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Open() error = nil, want a refusal")
			}
			if got := ErrorCode(err); got != tt.wantCode {
				t.Fatalf("ErrorCode() = %q, want %q (error %v)", got, tt.wantCode, err)
			}
		})
	}
}
