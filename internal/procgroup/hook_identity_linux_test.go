package procgroup

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHookOwnerRejectsAnotherWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	workspace := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "echo $PPID; sleep 30 >/dev/null 2>&1 &")
	cmd.Dir = workspace
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	Configure(t.Context(), cmd)
	pid := 0
	t.Cleanup(func() {
		if pid > 0 {
			_, _ = TerminateHookOwner(context.Background(), pid, workspace)
		}
	})
	if err := RunHook(t.Context(), cmd, nil); err != nil {
		t.Fatal(err)
	}
	var err error
	pid, err = strconv.Atoi(strings.TrimSpace(output.String()))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if owned, err := TerminateHookOwner(t.Context(), pid, t.TempDir()); !owned || err != nil {
		t.Fatalf("mismatched workspace owner = %t, error = %v", owned, err)
	}
	state := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err := unix.Poll(state, 0); err != nil || state[0].Revents != 0 {
		t.Fatalf("another workspace's owner exited: state=%v error=%v", state, err)
	}
	if owned, err := TerminateHookOwner(t.Context(), pid, workspace); !owned || err != nil {
		t.Fatalf("matching workspace owner = %t, error = %v", owned, err)
	}
	if _, err := unix.Poll(state, 0); err != nil || state[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("matching owner remained alive: state=%v error=%v", state, err)
	}
}
