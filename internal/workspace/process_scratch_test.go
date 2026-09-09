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
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			root := t.TempDir()
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
			select {
			case <-exited:
			case <-time.After(30 * time.Second):
				t.Fatal("reaping returned before descendant closed its pipe")
			}
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
	if err := parent.Run(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = input.Close()
	buffered := bufio.NewReader(reader)
	ready := make(chan int, 1)
	go func() {
		for {
			line, err := buffered.ReadString('\n')
			if err != nil {
				ready <- 0
				return
			}
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ready:"); ok {
				pid, _ := strconv.Atoi(value)
				ready <- pid
				return
			}
		}
	}()
	var pid int
	select {
	case pid = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("escaped descendant did not become ready")
	}
	if pid <= 0 {
		t.Fatal("escaped descendant closed its pipe before readiness")
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Kill()
		_ = child.Release()
	})
	identity, err := procgroup.Inspect(&exec.Cmd{Process: child})
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
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
		procgroup.Configure(context.Background(), child)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Chdir(os.Getenv("DETENT_SCRATCH_OUTSIDE")); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "ready:%d\n", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}
