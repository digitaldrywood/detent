//go:build darwin || linux

package hookproc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
)

func TestReapDaemonizedPostgresAfterMissedWriterSnapshot(t *testing.T) {
	if _, err := exec.LookPath("initdb"); err != nil {
		t.Skip("requires initdb on PATH")
	}
	if _, err := exec.LookPath("pg_ctl"); err != nil {
		t.Skip("requires pg_ctl on PATH")
	}
	t.Setenv("PGPORT", "invalid")

	root := t.TempDir()
	data := filepath.Join(root, "data")
	pidFile := filepath.Join(data, "postmaster.pid")
	logFile := filepath.Join(root, "postgres.log")
	fixtureEnv := []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "TMPDIR=" + root}
	t.Cleanup(func() {
		if _, err := os.Stat(pidFile); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(ctx, "pg_ctl", "-D", data, "-m", "immediate", "-w", "stop")
		stop.Env = fixtureEnv
		if output, err := stop.CombinedOutput(); err != nil {
			t.Errorf("stop PostgreSQL fixture: %v: %s", err, output)
		}
	})

	lifetime, err := NewLifetime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lifetime.Close(); err != nil {
			t.Error(err)
		}
	})
	const token = "example-hook-owner"
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", `
initdb -U detent_test -A trust --no-locale -D data >/dev/null
pg_ctl -D data -l postgres.log -o '-c listen_addresses= -c unix_socket_directories=. -c max_connections=10 -c shared_buffers=1MB' -w start
printf 'export FIXTURE_READY=1\n' > .envrc
`)
	cmd.Dir = root
	cmd.Env = append(fixtureEnv, HookOwnerKey+"="+token, "DETENT_WORKSPACE="+root)
	(&Owner{descriptor: lifetime}).ConfigureCommand(cmd)
	procgroup.Configure(t.Context(), cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		log, logErr := os.ReadFile(logFile)
		t.Fatalf("start PostgreSQL hook: %v: %s\nPostgreSQL server log (read error: %v):\n%s", err, output, logErr, log)
	}
	info, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pidText, _, _ := strings.Cut(string(info), "\n")
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 1 {
		t.Fatalf("invalid postmaster PID %q", pidText)
	}
	start, err := procstart.Identity(pid)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := lifetime.Capture(t.Context(), root, token); err != nil || !active || lifetime.starts[pid] != start {
		t.Fatalf("capture active PostgreSQL: active=%t, bound=%t, error=%v", active, lifetime.starts[pid] == start, err)
	}

	ctx := context.WithValue(t.Context(), writerSnapshotKey{}, []int{})
	if _, err := lifetime.Reap(ctx, root, token); err != nil {
		t.Fatalf("reap verified PostgreSQL service: %v", err)
	}
	if err := lifetime.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}
	if alive, err := procstart.Matches(pid, start); err != nil || alive {
		t.Fatalf("postmaster still running: alive=%t, error=%v", alive, err)
	}
}

func TestUnidentifiedWriterExitRemainsUnverified(t *testing.T) {
	lifetime, err := NewLifetime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lifetime.Close(); err != nil {
			t.Error(err)
		}
	})
	writer, err := os.OpenFile(lifetime.path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	path := t.TempDir()
	ctx := lifetime.withOwner(t.Context(), "example-hook-owner")
	scan := func(context.Context, string) ([]int, error) {
		return nil, writer.Close()
	}
	verified, _ := verifiedProcessTargets(ctx, path, scan, procstart.Identity, processEnvironment, nil)
	if _, err := verified(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := lifetime.verifyWriterExits(); err == nil {
		t.Fatal("accepted the exit of a writer whose identity was never verified")
	}
}

func TestWriterHandoffLeavesOriginalExitUnverified(t *testing.T) {
	lifetime, err := NewLifetime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lifetime.Close(); err != nil {
			t.Error(err)
		}
	})
	replacementInput, retireReplacement, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer replacementInput.Close()
	defer retireReplacement.Close()

	const token = "example-hook-owner"
	path := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", `
printf 'ready\n'
IFS= read -r handoff
/bin/sh -ec 'printf "%s\n" "$$"; IFS= read -r retire || :' <&3 &
replacement=$!
exec 4>&-
printf 'handed off\n'
wait "$replacement"
printf 'retired\n'
IFS= read -r release || :
`)
	cmd.Dir = path
	cmd.Env = append(os.Environ(), HookOwnerKey+"="+token, "DETENT_WORKSPACE="+path)
	cmd.ExtraFiles = []*os.File{replacementInput}
	(&Owner{descriptor: lifetime}).ConfigureCommand(cmd)
	procgroup.Configure(t.Context(), cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := procgroup.Start(cmd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		retireReplacement.Close()
		input.Close()
		if err := procgroup.Wait(cmd); err != nil && !t.Failed() {
			t.Error(err)
		}
	}()

	status := bufio.NewScanner(output)
	if !status.Scan() || status.Text() != "ready" {
		t.Fatalf("original writer ready: %q, %v", status.Text(), status.Err())
	}
	original := cmd.Process.Pid
	start, err := procstart.Identity(original)
	if err != nil {
		t.Fatal(err)
	}
	if writer, err := lifetime.holdsWriter(t.Context(), original); err != nil || !writer {
		t.Fatalf("original writer does not hold the FIFO: %t, %v", writer, err)
	}

	ctx := context.WithValue(lifetime.withOwner(t.Context(), token), writerSnapshotKey{}, []int{})
	scan := func(ctx context.Context, _ string) ([]int, error) {
		if _, err := io.WriteString(input, "handoff\n"); err != nil {
			t.Fatal(err)
		}
		var pid int
		var handedOff bool
		for range 2 {
			if !status.Scan() {
				t.Fatalf("writer handoff: %v", status.Err())
			}
			if status.Text() == "handed off" {
				handedOff = true
				continue
			}
			var err error
			pid, err = strconv.Atoi(status.Text())
			if err != nil || pid <= 1 || pid == original {
				t.Fatalf("invalid replacement PID %q", status.Text())
			}
		}
		if !handedOff || pid == 0 {
			t.Fatalf("incomplete handoff: released=%t replacement=%d", handedOff, pid)
		}
		if writer, err := lifetime.holdsWriter(ctx, original); err != nil || writer {
			t.Fatalf("original still holds the FIFO: %t, %v", writer, err)
		}
		if writer, err := lifetime.holdsWriter(ctx, pid); err != nil || !writer {
			t.Fatalf("replacement does not hold the FIFO: %t, %v", writer, err)
		}
		return []int{pid}, nil
	}
	verified, _ := verifiedProcessTargets(ctx, path, scan, procstart.Identity, processEnvironment, nil)
	if pids, err := verified(ctx, path); err != nil || len(pids) != 1 || pids[0] == original {
		t.Fatalf("verified replacement: %v, %v", pids, err)
	}
	if err := retireReplacement.Close(); err != nil {
		t.Fatal(err)
	}
	if !status.Scan() || status.Text() != "retired" {
		t.Fatalf("replacement retirement: %q, %v", status.Text(), status.Err())
	}
	if active, err := lifetime.Active(); err != nil || active {
		t.Fatalf("FIFO still active: %t, %v", active, err)
	}
	if alive, err := procstart.Matches(original, start); err != nil || !alive {
		t.Fatalf("original writer is still expected to be alive: %t, %v", alive, err)
	}
	if err := lifetime.verifyWriterExits(); err == nil {
		t.Fatal("accepted the original writer's exit after only the replacement retired")
	}
}
