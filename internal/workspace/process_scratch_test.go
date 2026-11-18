//go:build darwin || linux || windows

package workspace

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestReapScratchProcessesOutsideWorkspace(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			root := t.TempDir()
			useRealProcessScan(t, root)
			scratch := filepath.Join(root, ".detent", "worker-tmp", "attempt-example")
			if legacy {
				scratch = filepath.Join(root, ".detent", "tmp")
			}
			if err := os.MkdirAll(scratch, 0o700); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(scratch, "fixture")
			if err := os.WriteFile(fixture, []byte("still owned"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, exited := startScratchEnvironmentDescendant(t, scratch, legacy)
			neighbor, _ := startScratchEnvironmentDescendant(t, t.TempDir(), false)
			reaped, err := ReapWorkerArtifactProcesses(t.Context(), root, scratch, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if reaped == 0 {
				t.Fatal("escaped descendant was not reaped")
			}
			<-exited
			if alive, err := procgroup.Alive(neighbor); err != nil || !alive {
				t.Fatalf("unrelated descendant alive = %t, error = %v", alive, err)
			}
			if _, err := os.Stat(fixture); err != nil {
				t.Fatalf("fixture removed by process reaping: %v", err)
			}
			if err := CleanupOwnedPath(root, scratch); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func startScratchEnvironmentDescendant(t *testing.T, scratch string, legacy bool) (procgroup.Identity, <-chan struct{}) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	input, control, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = control.Close()
		_ = input.Close()
		_ = writer.Close()
		_ = reader.Close()
	})
	parent := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestScratchEnvironmentProcess$")
	parent.Dir = scratch
	parent.Stdin, parent.Stdout = input, writer
	parent.Stderr = os.Stderr
	parent.Env = append(os.Environ(), "DETENT_SCRATCH_ENV_HELPER=parent", "DETENT_SCRATCH_OUTSIDE="+t.TempDir(), "DETENT_SCRATCH_CHILD_COVER="+t.TempDir(), "GOCOVERDIR="+t.TempDir())
	procgroup.SetTempDir(parent, scratch)
	if legacy {
		var environment []string
		for _, entry := range parent.Env {
			if !strings.HasPrefix(entry, "DETENT_WORKER_SCRATCH=") {
				environment = append(environment, entry)
			}
		}
		parent.Env = environment
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parentDone := make(chan struct{})
	var parentErr error
	go func() {
		parentErr = parent.Wait()
		_ = writer.Close()
		_ = input.Close()
		close(parentDone)
	}()
	t.Cleanup(func() {
		_ = control.Close()
		_ = parent.Process.Kill()
		<-parentDone
	})
	buffered := bufio.NewReader(reader)
	line, err := buffered.ReadString('\n')
	if err != nil {
		t.Fatalf("read descendant identity: %v", err)
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(line), "started:")
	pid, err := strconv.Atoi(value)
	if !ok || err != nil || pid <= 0 {
		t.Fatalf("invalid descendant identity: %q", line)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	var drained <-chan struct{}
	t.Cleanup(func() {
		_ = child.Release()
		_ = reader.Close()
		if drained != nil {
			<-drained
		}
	})
	line, err = buffered.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != fmt.Sprintf("ready:%d", pid) {
		t.Fatalf("descendant readiness = %q, error = %v", line, err)
	}
	<-parentDone
	if parentErr != nil {
		t.Fatalf("descendant parent: %v", parentErr)
	}
	identity, err := procgroup.Inspect(&exec.Cmd{Process: child})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := procgroup.Terminate(context.Background(), identity, 5*time.Second); err != nil {
			t.Errorf("terminate descendant %d: %v", identity.PID, err)
		}
	})
	exited := make(chan struct{})
	drained = exited
	go func() {
		_, _ = io.Copy(io.Discard, buffered)
		close(exited)
	}()
	return identity, exited
}

func TestScratchEnvironmentProcess(t *testing.T) {
	role := os.Getenv("DETENT_SCRATCH_ENV_HELPER")
	if role == "" {
		return
	}
	if role == "parent" {
		child := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestScratchEnvironmentProcess$")
		child.Env = append(os.Environ(), "DETENT_SCRATCH_ENV_HELPER=child", "GOCOVERDIR="+os.Getenv("DETENT_SCRATCH_CHILD_COVER"))
		child.Stdin, child.Stdout = os.Stdin, os.Stdout
		ready, err := child.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer ready.Close()
		procgroup.Configure(context.Background(), child)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(os.Stdout, "started:%d\n", child.Process.Pid); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(ready).ReadString('\n')
		if err != nil {
			t.Fatalf("read child readiness: %v", err)
		}
		if _, err := io.WriteString(os.Stdout, line); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Chdir(os.Getenv("DETENT_SCRATCH_OUTSIDE")); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(os.Stderr, "ready:%d\n", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}
