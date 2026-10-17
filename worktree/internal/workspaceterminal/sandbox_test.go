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

func TestSandboxUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	err := ProbeSandbox(ctx)
	if err == nil {
		if AvailableIsolation() != workspacesession.IsolationSandbox {
			t.Fatal("a successful enforcement probe must advertise sandbox")
		}
		return
	}
	if AvailableIsolation() != workspacesession.IsolationUser {
		t.Fatal("failed enforcement must not advertise sandbox")
	}
	t.Logf("actual host sandbox unavailable: %v", err)
	service, err := New(ctx, t.TempDir(), "/bin/sh", workspacesession.IsolationSandbox, nil)
	if err != nil {
		if !errors.Is(err, ErrSandboxIsolation) {
			t.Fatal(err)
		}
		return
	}
	terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{}, (&collector{}).emit)
	if terminal != nil {
		terminal.Close()
		t.Fatal("an unsupported sandbox opened a terminal")
	}
	if ErrorCode(err) != workspacesession.CodeUnsupported {
		t.Fatalf("unavailable launch = %v, want unsupported", err)
	}
}

func TestSandboxTerminalContainment(t *testing.T) {
	if AvailableIsolation() != workspacesession.IsolationSandbox {
		t.Skip("actual host enforcement is unavailable; this is not sandbox acceptance evidence")
	}
	base := t.TempDir()
	root := filepath.Join(base, "worktree")
	outside := filepath.Join(base, "runner-home")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(outside, "credentials")
	if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	service, err := New(t.Context(), root, "/bin/sh", workspacesession.IsolationSandbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		command string
	}{
		{name: "worktree writes", command: "printf allowed > allowed && test \"$(cat allowed)\" = allowed"},
		{name: "relative credential read", command: "! cat ../runner-home/credentials"},
		{name: "absolute credential read", command: "! cat " + shellQuote(private)},
		{name: "relative write escape", command: "! (printf escaped > ../runner-home/credentials)"},
		{name: "symlink read escape", command: "! cat escape/credentials"},
		{name: "symlink write escape", command: "! (printf escaped > escape/credentials)"},
		{name: "hardlink escape", command: "! ln ../runner-home/credentials stolen"},
		{name: "child inherits confinement", command: "/bin/sh -c '! cat ../runner-home/credentials && ! (printf escaped > escape/credentials)'"},
		{name: "terminal resize", command: "test \"$(stty size)\" = '40 120'"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{}, (&collector{}).emit)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(terminal.Close)
			if terminal.Isolation() != workspacesession.IsolationSandbox {
				t.Fatal("the opened PTY lost its declared isolation")
			}
			if err := terminal.Resize(120, 40); err != nil {
				t.Fatal(err)
			}
			if err := terminal.Write([]byte(test.command + "; exit $?\n")); err != nil {
				t.Fatal(err)
			}
			if result := terminal.Wait(); result.ExitCode != 0 {
				t.Fatalf("escape regression failed: %+v", result)
			}
			contents, err := os.ReadFile(private)
			if err != nil || string(contents) != "private" {
				t.Fatalf("private file changed: %q, %v", contents, err)
			}
		})
	}
}

func TestSandboxEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "runner-credential")
	t.Setenv("SSH_AUTH_SOCK", "/runner/ssh-agent")
	t.Setenv("BASH_ENV", "/runner/startup")
	got := environmentMap(sandboxEnvironment("/worktree", 80, 24))
	for _, key := range []string{"GITHUB_TOKEN", "SSH_AUTH_SOCK", "BASH_ENV"} {
		if _, found := got[key]; found {
			t.Fatalf("sandbox inherited %s", key)
		}
	}
	if got["HOME"] != "/worktree" || got["TMPDIR"] != "/worktree" || got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("sandbox environment = %v", got)
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
