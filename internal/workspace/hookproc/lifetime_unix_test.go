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

	"golang.org/x/sys/unix"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestReapDaemonizedPostgresAfterMissedWriterSnapshot(t *testing.T) {
	if os.Getenv("DETENT_TEST_POSTGRES") != "binary" {
		t.Skip("requires the PostgreSQL binary test mode")
	}

	root := t.TempDir()
	data := filepath.Join(root, "data")
	pidFile := filepath.Join(data, "postmaster.pid")
	t.Cleanup(func() {
		if _, err := os.Stat(pidFile); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "pg_ctl", "-D", data, "-m", "immediate", "-w", "stop").CombinedOutput(); err != nil {
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
initdb -A trust --no-locale -D data >/dev/null
pg_ctl -D data -l postgres.log -o '-c listen_addresses= -c unix_socket_directories=.' -w start
printf 'export FIXTURE_READY=1\n' > .envrc
`)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), HookOwnerKey+"="+token, "DETENT_WORKSPACE="+root)
	(&Owner{descriptor: lifetime}).ConfigureCommand(cmd)
	procgroup.Configure(t.Context(), cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start PostgreSQL hook: %v: %s", err, output)
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
/bin/cat <&3 >/dev/null &
replacement=$!
exec 4>&-
printf '%s\n' "$replacement"
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
		if !status.Scan() {
			t.Fatalf("replacement PID: %v", status.Err())
		}
		pid, err := strconv.Atoi(status.Text())
		if err != nil || pid <= 1 || pid == original {
			t.Fatalf("invalid replacement PID %q", status.Text())
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

func TestReplacementPreservesWorkspaceAfterUnverifiedWriterHandoff(t *testing.T) {
	root, path := t.TempDir(), t.TempDir()
	if err := MarkWorkspaceSetupPending(root, path, ":"); err != nil {
		t.Fatal(err)
	}

	registry := &Registry{}
	ctx := context.WithValue(t.Context(), workspaceSetupKey{}, &workspaceSetup{root: root})
	owner, err := registry.Start(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.descriptor.Close(); err != nil {
			t.Error(err)
		}
	})
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", "exec 4>\"$1\"; printf 'ready\\n'; IFS= read -r release || :", "hook", owner.descriptor.path())
	cmd.Dir = path
	cmd.Env = append(os.Environ(), HookOwnerKey+"="+owner.Token(), "DETENT_WORKSPACE="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(output).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("original hook readiness: %q", line)
		}
	case <-time.After(hookCleanupTimeout):
		t.Fatal("original hook did not open the FIFO")
	}
	if err := owner.CommandFinished(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !owner.setup.retained.Load() {
		t.Fatal("original hook did not record retained setup")
	}

	writer, err := os.OpenFile(owner.descriptor.path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	ctx = context.WithValue(t.Context(), writerSnapshotKey{}, []int{})
	var unverified *HookExitUnverifiedError
	if err := registry.Stop(ctx, path); !errors.As(err, &unverified) || !owner.descriptor.unidentifiedWriter {
		t.Fatalf("teardown did not record an unidentified writer: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if active, err := owner.descriptor.Active(); err != nil || active {
		t.Fatalf("FIFO still active: %t, %v", active, err)
	}

	replacement := &Registry{}
	if retry, err := replacement.CheckWorkspaceSetup(t.Context(), root, path); retry || !errors.Is(err, ErrWorkspacePreserved) {
		t.Errorf("replacement could reuse the workspace: retry cleanup=%t, error=%v", retry, err)
	}
	if err := replacement.CheckWorkspaceSetupRemoval(t.Context(), root, path); !errors.Is(err, ErrWorkspacePreserved) {
		t.Errorf("replacement could remove the workspace: %v", err)
	}
}

func TestReplacementRejectsNonRegularReadyMarker(t *testing.T) {
	for _, markerType := range []string{"fifo", "symlink"} {
		t.Run(markerType, func(t *testing.T) {
			root, path := t.TempDir(), t.TempDir()
			if err := MarkWorkspaceSetupPending(root, path, ":"); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(root, pendingWorkspaceSetupPath(path), "ready")
			fifo := ready
			if markerType == "symlink" {
				fifo = filepath.Join(filepath.Dir(ready), "fifo")
			}
			if err := unix.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			if markerType == "symlink" {
				if err := os.Symlink(filepath.Base(fifo), ready); err != nil {
					t.Fatal(err)
				}
			}

			result := make(chan error, 1)
			go func() {
				_, err := (&Registry{}).CheckWorkspaceSetup(t.Context(), root, path)
				result <- err
			}()
			select {
			case err := <-result:
				var unverified *HookExitUnverifiedError
				if !errors.As(err, &unverified) || !errors.Is(err, ErrWorkspacePreserved) {
					t.Fatalf("invalid marker did not preserve the workspace: %v", err)
				}
			case <-time.After(hookCleanupTimeout):
				writer, err := os.OpenFile(fifo, os.O_RDWR|unix.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				if _, err := io.WriteString(writer, "{}\n"); err != nil {
					t.Fatal(err)
				}
				<-result
				t.Fatal("reading an invalid ready marker blocked workspace setup")
			}
		})
	}
}

func TestReplacementPreservesWorkspaceWithLaterHookHelper(t *testing.T) {
	root, path := t.TempDir(), t.TempDir()
	if err := MarkWorkspaceSetupPending(root, path, ":"); err != nil {
		t.Fatal(err)
	}
	first := &Registry{}
	ctx := context.WithValue(t.Context(), workspaceSetupKey{}, &workspaceSetup{root: root})
	owner, err := first.Start(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.descriptor.Close(); err != nil {
			t.Error(err)
		}
	})

	helperInput, retireHelper, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer helperInput.Close()
	defer retireHelper.Close()

	const script = "exec 4>\"$1\"; printf 'ready\\n'; IFS= read -r handoff; " +
		"/bin/sh -c 'exec 4>&-; printf \"%s\\n\" \"$$\"; IFS= read -r release || :' <&3 &"
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", script, "hook", owner.descriptor.path())
	cmd.Dir = path
	cmd.Env = append(os.Environ(), HookOwnerKey+"="+owner.Token(), "DETENT_WORKSPACE="+path)
	cmd.ExtraFiles = []*os.File{helperInput}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			cmd.Wait()
		}
	})
	lines := make(chan string, 2)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	readLine := func() string {
		select {
		case line := <-lines:
			return line
		case <-time.After(hookCleanupTimeout):
			t.Fatal("timed out waiting for hook fixture")
			return ""
		}
	}
	if line := readLine(); line != "ready" {
		t.Fatalf("original hook readiness: %q", line)
	}
	if err := owner.CommandFinished(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !owner.setup.retained.Load() {
		t.Fatal("original hook did not record retained setup")
	}
	if _, err := io.WriteString(input, "handoff\n"); err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(readLine())
	if err != nil || pid <= 0 || pid == cmd.Process.Pid {
		t.Fatalf("invalid later hook helper PID %d: %v", pid, err)
	}
	start, err := procstart.Identity(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if active, err := owner.descriptor.Active(); err != nil || active {
		t.Fatalf("expected FIFO closure while helper lives: %t, %v", active, err)
	}

	replacement := &Registry{}
	var unverified *HookExitUnverifiedError
	if retry, err := replacement.CheckWorkspaceSetup(t.Context(), root, path); retry || !errors.As(err, &unverified) || !errors.Is(err, ErrWorkspacePreserved) {
		t.Errorf("replacement could reuse the workspace: retry cleanup=%t, error=%v", retry, err)
	}
	if err := replacement.CheckWorkspaceSetupRemoval(t.Context(), root, path); !errors.As(err, &unverified) || !errors.Is(err, ErrWorkspacePreserved) {
		t.Errorf("replacement could remove the workspace: %v", err)
	}
	if alive, err := procstart.Matches(pid, start); err != nil || !alive {
		t.Fatalf("replacement disturbed the later hook helper: alive=%t, error=%v", alive, err)
	}

	if err := retireHelper.Close(); err != nil {
		t.Fatal(err)
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		alive, err := procstart.Matches(pid, start)
		if alive {
			return errors.New("waiting for later hook helper to exit")
		}
		return err
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := replacement.CheckWorkspaceSetup(canceled, root, path); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrWorkspacePreserved) {
		t.Errorf("replacement did not preserve the workspace after canceled inspection: %v", err)
	}
	if retry, err := replacement.CheckWorkspaceSetup(t.Context(), root, path); retry || err != nil {
		t.Errorf("replacement refused verified reuse: retry cleanup=%t, error=%v", retry, err)
	}
	if err := replacement.CheckWorkspaceSetupRemoval(t.Context(), root, path); err != nil {
		t.Errorf("replacement refused verified removal: %v", err)
	}
}

func TestOpenRetainedFileRefusesSymlinkSwappedAfterInspection(t *testing.T) {
	root := t.TempDir()
	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.WriteFile(filepath.Join(root, "ready"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := dir.Lstat("ready")
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Rename("ready", "original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original", filepath.Join(root, "ready")); err != nil {
		t.Fatal(err)
	}
	file, err := openRetainedFile(dir, "ready", expected)
	if err == nil {
		file.Close()
		t.Fatal("opened the inspected inode through a substituted symlink")
	}
}
