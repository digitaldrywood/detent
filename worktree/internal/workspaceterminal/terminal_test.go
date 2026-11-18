package workspaceterminal

import (
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
	service, err := New(t.Context(), worktree, "/bin/sh", workspacesession.IsolationUser,
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
		{name: "unknown isolation is refused", worktree: directory, isolation: "vm"},
		{name: "missing worktree is refused", worktree: filepath.Join(directory, "absent"), isolation: workspacesession.IsolationUser},
		{name: "a file is not a worktree", worktree: file, isolation: workspacesession.IsolationUser},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service, err := New(t.Context(), tt.worktree, "/bin/sh", tt.isolation, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
