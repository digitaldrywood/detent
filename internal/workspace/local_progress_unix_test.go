//go:build unix

package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func assertProcessRetired(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	if err == nil && strings.HasPrefix(stat, "Z") {
		return
	}
	if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("descendant pid %d still alive (psErr=%v stat=%q)", pid, err, stat)
}

func TestGitProgressPatchRetiresDiffDescendantHoldingStdout(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("look up git: %v", err)
	}
	repo := initSourceRepo(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	naturalExitFile := filepath.Join(t.TempDir(), "natural-exit")
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" diff "*)
    ( sleep 60; printf 'exited\n' > %q ) </dev/null 2>/dev/null &
    printf '%%s\n' "$!" > %q
    printf 'diff --git a/file.txt b/file.txt\nindex 1111111..2222222 100644\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n'
    exit 0
    ;;
  *)
    exec %q "$@"
    ;;
esac
`, naturalExitFile, pidFile, realGit)
	wrapper := filepath.Join(binDir, "git")
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := gitProgressPatchBounded(t.Context(), repo, localProgressPatchLimit, "HEAD")
	if err != nil {
		t.Fatalf("gitProgressPatchBounded() error = %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("gitProgressPatchBounded() returned empty patch id")
	}
	if _, err := os.Stat(naturalExitFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diff descendant exited naturally instead of being retired: %v", err)
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child pid %q: %v", string(rawPID), err)
	}
	assertProcessRetired(t, pid)
}
