package workspaceterminal

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// requirePTY skips a test on a build with no pseudo-terminal. Section 18.3 is
// written in terms Windows does not have, so the surface is a port there rather
// than a build tag, and a test that asserted otherwise would be asserting
// something the platform cannot do.
func requirePTY(t *testing.T) {
	t.Helper()
	if !Supported {
		t.Skip("this platform has no pseudo-terminal")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("this platform has no /bin/sh to open a terminal on")
	}
}

// collector gathers the output spans a terminal produces, decoding them, so a
// test can wait for what a person would have seen on their screen.
type collector struct {
	mu      sync.Mutex
	builder strings.Builder
	refuse  error
}

func (c *collector) emit(span workspacesession.TerminalOutput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse != nil {
		return c.refuse
	}
	if span.Encoding != workspacesession.EncodingBase64 {
		return errors.New("a terminal output span must be base64")
	}
	decoded, err := base64.StdEncoding.DecodeString(span.Data)
	if err != nil {
		return err
	}
	c.builder.Write(decoded)
	return nil
}

func (c *collector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builder.String()
}

func (c *collector) refuseSpans(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refuse = err
}

// waitFor polls until condition holds or the deadline passes. A terminal is
// asynchronous by nature -- the shell writes when it feels like it -- so a test
// that read once would be testing the scheduler.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestService(t *testing.T, worktree string) *Service {
	t.Helper()
	service, err := New(worktree, "/bin/sh", workspacesession.IsolationUser,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service
}

func TestServiceNew(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	file := filepath.Join(directory, "notes.md")
	if err := os.WriteFile(file, []byte("notes"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tests := []struct {
		name      string
		worktree  string
		isolation string
		wantErr   error
		wantOK    bool
	}{
		{name: "user isolation opens", worktree: directory, isolation: workspacesession.IsolationUser, wantOK: true},
		{name: "empty isolation defaults to user", worktree: directory, isolation: "", wantOK: true},
		{
			name:      "container isolation is refused rather than approximated",
			worktree:  directory,
			isolation: workspacesession.IsolationContainer,
			wantErr:   ErrContainerIsolation,
		},
		{name: "unknown isolation is refused", worktree: directory, isolation: "sandbox"},
		{name: "missing worktree is refused", worktree: filepath.Join(directory, "absent"), isolation: workspacesession.IsolationUser},
		{name: "a file is not a worktree", worktree: file, isolation: workspacesession.IsolationUser},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service, err := New(tt.worktree, "/bin/sh", tt.isolation, slog.New(slog.NewTextHandler(io.Discard, nil)))
			switch {
			case tt.wantOK && err != nil:
				t.Fatalf("New() error = %v, want nil", err)
			case tt.wantOK:
				if service.Isolation() != workspacesession.IsolationUser {
					t.Fatalf("Isolation() = %q, want %q", service.Isolation(), workspacesession.IsolationUser)
				}
				if service.Shell() != "/bin/sh" {
					t.Fatalf("Shell() = %q, want /bin/sh", service.Shell())
				}
			case err == nil:
				t.Fatal("New() error = nil, want a refusal")
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
			}
		})
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

	// A refused span means the reader's connection has gone. Section 18.2 gives
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

	tests := []struct {
		name     string
		request  workspacesession.TerminalOpen
		wantCode string
	}{
		{name: "a directory inside the worktree opens", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "pkg"}},
		{name: "an absolute cwd is refused", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "/etc"}, wantCode: workspacesession.CodeForbidden},
		{name: "a cwd that escapes is refused", request: workspacesession.TerminalOpen{Cols: 80, Rows: 24, Cwd: "../elsewhere"}, wantCode: workspacesession.CodeForbidden},
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

func TestOpenWithoutASinkIsRefused(t *testing.T) {
	t.Parallel()

	service := newTestService(t, t.TempDir())
	_, err := service.Open(t.Context(), workspacesession.TerminalOpen{Cols: 80, Rows: 24}, nil)
	if got := ErrorCode(err); got != workspacesession.CodeInvalidFrame {
		t.Fatalf("ErrorCode() = %q, want %q", got, workspacesession.CodeInvalidFrame)
	}
}

func TestTerminalEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ []string
		want    map[string]string
		dropped []string
	}{
		{
			name: "credentials are dropped and the window is exported",
			environ: []string{
				"PATH=/bin",
				"DETENT_HUB_TOKEN=secret",
				"ANTHROPIC_API_KEY=secret",
				"OPENAI_BASE_URL=https://example.test",
				"CODEX_HOME=/home/runner/.codex",
				"CLAUDE_CONFIG_DIR=/home/runner/.claude",
				"API_KEY=secret",
				"GITHUB_API_KEY=secret",
				"HOME=/home/runner",
			},
			want: map[string]string{
				"PATH":             "/bin",
				"HOME":             "/home/runner",
				"DETENT_WORKSPACE": "/work",
				"TERM":             "xterm-256color",
				"COLUMNS":          "100",
				"LINES":            "30",
			},
			dropped: []string{"DETENT_HUB_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "API_KEY", "GITHUB_API_KEY"},
		},
		{
			name:    "an inherited window does not shadow the allocated one",
			environ: []string{"TERM=dumb", "COLUMNS=9", "LINES=9"},
			want:    map[string]string{"TERM": "xterm-256color", "COLUMNS": "100", "LINES": "30"},
		},
		{
			name:    "a lowercase credential name is dropped too",
			environ: []string{"anthropic_api_key=secret", "path=/bin"},
			want:    map[string]string{"path": "/bin"},
			dropped: []string{"anthropic_api_key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := environmentMap(terminalEnvironment(tt.environ, "/work", 100, 30))
			for key, want := range tt.want {
				if got[key] != want {
					t.Fatalf("environment[%q] = %q, want %q", key, got[key], want)
				}
			}
			for _, key := range tt.dropped {
				if _, present := got[key]; present {
					t.Fatalf("environment kept %q, which must be dropped", key)
				}
			}
		})
	}
}

// environmentMap turns an environ slice into a lookup, keeping the last value
// for a repeated key exactly as exec does.
func environmentMap(environ []string) map[string]string {
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func TestInteractiveArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		shell string
		want  []string
	}{
		{name: "sh is interactive", shell: "sh", want: []string{"-i"}},
		{name: "an absolute bash is interactive", shell: "/bin/bash", want: []string{"-i"}},
		{name: "zsh is interactive", shell: "zsh", want: []string{"-i"}},
		{name: "cmd takes no flag", shell: "cmd", want: nil},
		{name: "cmd.exe takes no flag", shell: `C:\Windows\System32\cmd.exe`, want: nil},
		{name: "powershell takes no flag", shell: "powershell", want: nil},
		{name: "pwsh takes no flag", shell: "pwsh", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := interactiveArgs(tt.shell)
			if len(got) != len(tt.want) {
				t.Fatalf("interactiveArgs(%q) = %v, want %v", tt.shell, got, tt.want)
			}
			for index, value := range tt.want {
				if got[index] != value {
					t.Fatalf("interactiveArgs(%q) = %v, want %v", tt.shell, got, tt.want)
				}
			}
		})
	}
}

func TestStartCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "a platform with no PTY is unsupported", err: ErrNoPTY, want: workspacesession.CodeUnsupported},
		{name: "a missing shell is unsupported", err: os.ErrNotExist, want: workspacesession.CodeUnsupported},
		{name: "anything else is forbidden", err: errors.New("boom"), want: workspacesession.CodeForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := startCode(tt.err); got != tt.want {
				t.Fatalf("startCode(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil is not ours", err: nil, want: ""},
		{name: "a foreign error is not ours", err: errors.New("boom"), want: ""},
		{name: "a refusal reports its code", err: refuse(workspacesession.CodeReadOnly, nil), want: workspacesession.CodeReadOnly},
		{
			name: "a wrapped refusal reports its code",
			err:  refuse(workspacesession.CodeNotFound, errors.New("absent")),
			want: workspacesession.CodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ErrorCode(tt.err); got != tt.want {
				t.Fatalf("ErrorCode(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
