//go:build unix

package gitexec

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

func TestWithinLimitRetiresGroupWhenLeaderExitsBeforeTruncation(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := fmt.Sprintf(`#!/bin/sh
leader=$$
(
	while kill -0 "$leader" 2>/dev/null; do
		:
	done
	printf '0123456789abcdef\n'
	exec sleep 30 >/dev/null 2>&1
) &
printf '%%s' "$!" > %q
exit 0
`, pidFile)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, truncated, err := WithinLimit(t.Context(), dir, nil, 4, "ls-remote", "--heads", "--refs", "origin")
	if err != nil || !truncated {
		t.Fatalf("WithinLimit() = (truncated=%t, err=%v), want (true, nil)", truncated, err)
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", rawPID, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		stateOut, psErr := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(stateOut))
		if psErr == nil && strings.HasPrefix(state, "Z") {
			return
		}
		if retryErr := syscall.Kill(pid, 0); !errors.Is(retryErr, syscall.ESRCH) {
			t.Fatalf("descendant pid %d still alive after WithinLimit returned: kill(0)=%v psErr=%v state=%q", pid, retryErr, psErr, state)
		}
	}
}
