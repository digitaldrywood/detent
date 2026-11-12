package workspaceterminal

import (
	"context"
	"errors"
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
