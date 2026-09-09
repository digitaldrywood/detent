//go:build unix

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRestartScratchCleanupWaitsForEscapedDescendant(t *testing.T) {
	for _, tt := range []struct {
		legacy  bool
		outside bool
	}{{legacy: true}, {}, {legacy: true, outside: true}, {outside: true}} {
		t.Run(fmt.Sprintf("legacy=%t/outside=%t", tt.legacy, tt.outside), func(t *testing.T) {
			root := t.TempDir()
			scratch, err := workspace.PrepareWorkerScratch(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if tt.legacy {
				scratch = filepath.Join(root, ".detent", "tmp")
				if err := os.MkdirAll(scratch, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for name, contents := range map[string]string{"fixture": "scratch intact\n", "executable": "#!/bin/sh\ncat \"$1\"\n"} {
				if err := os.WriteFile(filepath.Join(scratch, name), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			readyRead, readyWrite := scratchHandshakePipe(t)
			commandRead, commandWrite := scratchHandshakePipe(t)
			lockPath := filepath.Join(root, "gate.lock")
			parent := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestScratchDescendantProcess$")
			parent.Dir = root
			parent.Env = append(os.Environ(), "DETENT_SCRATCH_HELPER=parent", "DETENT_SCRATCH_PATH="+scratch, "DETENT_SCRATCH_LOCK="+lockPath, "DETENT_SCRATCH_CHILD_COVER="+t.TempDir(), "GOCOVERDIR="+t.TempDir())
			if tt.outside {
				parent.Env = append(parent.Env, "DETENT_SCRATCH_CHDIR="+t.TempDir())
			}
			procgroup.SetTempDir(parent, scratch)
			parent.ExtraFiles = []*os.File{readyWrite, commandRead}
			procgroup.Configure(t.Context(), parent)
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = parent.Process.Kill() })
			identity, err := procgroup.Inspect(parent)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := commandWrite.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			if err := parent.Wait(); err != nil {
				t.Fatal(err)
			}
			_ = readyWrite.Close()
			_ = commandRead.Close()
			reader := bufio.NewReader(readyRead)
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("parent exited; descendant %d ready in %s", pid, scratch)
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			resumed, err := workspace.PrepareWorkerScratch(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			processStore := &shutdownWorkerProcessStore{processes: []store.WorkerProcess{{
				SessionID:             2354,
				WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt},
				CleanupRoot:           root, CleanupPath: scratch,
			}}}
			finished := make(chan error, 1)
			reapCtx, cancelReap := context.WithCancel(t.Context())
			joined := false
			t.Cleanup(func() {
				cancelReap()
				if !joined {
					select {
					case <-finished:
					case <-time.After(10 * time.Second):
						t.Error("reaper did not stop after cancellation")
					}
				}
			})
			go func() {
				finished <- reapWorkerProcesses(reapCtx, processStore, slog.New(slog.NewTextHandler(io.Discard, nil)), "startup", time.Minute, time.Now, func(ctx context.Context, identity procgroup.Identity, grace time.Duration) (procgroup.TerminationOutcome, error) {
					outcome, err := procgroup.Terminate(ctx, identity, grace)
					t.Logf("parent group reap: %s, %v", outcome, err)
					return outcome, err
				})
			}()
			checked := make(chan string, 1)
			go func() {
				line, err := reader.ReadString('\n')
				if err != nil {
					line = err.Error()
				}
				checked <- line
			}()
			select {
			case line := <-checked:
				if line != "scratch intact\n" {
					t.Fatalf("descendant could not reexecute scratch fixture during shutdown: %q", line)
				}
			case err := <-finished:
				joined = true
				t.Fatalf("cleanup returned while descendant still owned scratch: %v", err)
			case <-time.After(90 * time.Second):
				t.Fatal("descendant did not acknowledge shutdown")
			}
			select {
			case err := <-finished:
				joined = true
				t.Fatalf("cleanup did not wait for descendant exit: %v", err)
			default:
			}
			if _, err := commandWrite.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				joined = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(90 * time.Second):
				t.Fatal("cleanup did not finish after descendant exit")
			}
			stopped = true
			if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed attempt scratch remains: %v", err)
			}
			if _, err := os.Stat(resumed); err != nil {
				t.Fatalf("prior owner removed resumed scratch: %v", err)
			}
			if len(processStore.reaped) != 1 {
				t.Fatalf("recorded reaps = %d, want 1", len(processStore.reaped))
			}
			lock, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("descendant retained validation lock after cleanup: %v", err)
			}
		})
	}
}

func scratchHandshakePipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	return reader, writer
}

func TestScratchDescendantProcess(t *testing.T) {
	role := os.Getenv("DETENT_SCRATCH_HELPER")
	if role == "" {
		return
	}
	ready := os.NewFile(3, "ready")
	command := os.NewFile(4, "command")
	scratch := os.Getenv("DETENT_SCRATCH_PATH")
	if role == "parent" {
		if _, err := io.ReadFull(command, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		child := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestScratchDescendantProcess$")
		child.Dir = scratch
		if outside := os.Getenv("DETENT_SCRATCH_CHDIR"); outside != "" {
			child.Dir = outside
		}
		child.Env = append(os.Environ(), "DETENT_SCRATCH_HELPER=child", "GOCOVERDIR="+os.Getenv("DETENT_SCRATCH_CHILD_COVER"))
		child.ExtraFiles = []*os.File{ready, command}
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	lock, err := os.OpenFile(os.Getenv("DETENT_SCRATCH_LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(ready, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	<-signals
	output, err := exec.CommandContext(context.Background(), filepath.Join(scratch, "executable"), filepath.Join(scratch, "fixture")).CombinedOutput()
	if err != nil {
		output = fmt.Appendf(nil, "scratch failure: %v: %s\n", err, output)
	}
	if _, err := ready.Write(output); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(command, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
}
