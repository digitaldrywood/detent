//go:build windows

package gitexec

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestIsLocalGitCommandRejectsRawWindowsCommandLine(t *testing.T) {
	t.Parallel()
	trustedGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("look up git: %v", err)
	}
	cmd := &exec.Cmd{
		Path:        trustedGit,
		Args:        []string{"git", "rev-parse", "HEAD"},
		SysProcAttr: &syscall.SysProcAttr{CmdLine: "git fetch origin"},
	}
	if IsLocalGitCommand(cmd) {
		t.Fatal("IsLocalGitCommand accepted non-empty Windows SysProcAttr.CmdLine")
	}
}
