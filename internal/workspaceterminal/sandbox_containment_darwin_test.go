package workspaceterminal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestSandboxTerminalContainment(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "alias", "worktree")
	outside := filepath.Join(base, "alias", "runner-home")
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
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("metadata"), 0o600); err != nil {
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
		{name: "worktree alias path writes", command: "printf alias > " + shellQuote(filepath.Join(root, "via-alias")) + " && test \"$(cat via-alias)\" = alias"},
		{name: "relative credential read", command: "! cat ../runner-home/credentials"},
		{name: "absolute credential read", command: "! cat " + shellQuote(private)},
		{name: "relative write escape", command: "! (printf escaped > ../runner-home/credentials)"},
		{name: "symlink read escape", command: "! cat escape/credentials"},
		{name: "symlink write escape", command: "! (printf escaped > escape/credentials)"},
		{name: "hardlink escape", command: "! ln ../runner-home/credentials stolen"},
		{name: "git entry is protected", command: "! touch .git && ! mv .git moved && ! ln .git linked"},
		{name: "git metadata is protected under alternate casing", command: "! touch .GIT"},
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

func TestTerminalCloseKillsConfinedChild(t *testing.T) {
	t.Parallel()
	requirePTY(t)
	testTerminalCloseCases(t, []terminalCloseCase{
		{name: "confined child is killed on close", child: true, sandbox: true, wantFullGrace: true},
	})
}
