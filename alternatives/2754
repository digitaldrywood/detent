//go:build windows

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsInspectAndTerminate(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	for _, normalExit := range []bool{false, true} {
		name := "terminated"
		if normalExit {
			name = "normal exit"
		}
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestWindowsProcessHelper$")
			cmd.Env = append(os.Environ(), "DETENT_WINDOWS_PROCESS_HELPER=1")
			Configure(t.Context(), cmd)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = input.Close() })
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})

			identity, err := Inspect(cmd)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if identity.PID != cmd.Process.Pid || identity.StartedAt.IsZero() {
				t.Fatalf("Inspect() = %#v", identity)
			}

			stale := identity
			stale.StartedAt = stale.StartedAt.Add(time.Second)
			outcome, err := Terminate(context.Background(), stale, time.Second)
			if err != nil {
				t.Fatalf("Terminate(stale) error = %v", err)
			}
			if outcome != TerminationOutcomeStaleIdentity {
				t.Fatalf("Terminate(stale) outcome = %q, want %q", outcome, TerminationOutcomeStaleIdentity)
			}
			if _, err := inspectProcess(identity.PID); err != nil {
				t.Fatalf("process after stale termination error = %v", err)
			}

			handle, err := openProcess(identity.PID, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(handle)
			if normalExit {
				if err := input.Close(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatalf("normal exit: %v", err)
				}
			} else {
				outcome, err = Terminate(context.Background(), identity, time.Second)
				if err != nil {
					t.Fatalf("Terminate() error = %v", err)
				}
				if outcome != TerminationOutcomeTerminated {
					t.Fatalf("Terminate() outcome = %q, want %q", outcome, TerminationOutcomeTerminated)
				}
				_ = cmd.Wait()
			}
			if _, err := inspectProcess(identity.PID); !errors.Is(err, ErrProcessNotRunning) {
				t.Fatalf("inspect exited process with retained handle: %v", err)
			}
			if alive, err := Alive(identity); err != nil || alive {
				t.Fatalf("Alive(exited) = %t, %v", alive, err)
			}
			observations, err := Observe([]Identity{identity})
			if err != nil || len(observations) != 1 || observations[0].Alive || observations[0].ProcessCount != 0 {
				t.Fatalf("Observe(exited) = %#v, %v", observations, err)
			}
		})
	}
}

func TestWindowsProcessHelper(t *testing.T) {
	if os.Getenv("DETENT_WINDOWS_PROCESS_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestWindowsJobTree(t *testing.T) {
	for _, mode := range []string{"terminate", "cleanup", "cleanup after parent exit", "wait", "cancel", "identity"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsJobHelper$")
			cmd.Env = append(os.Environ(), "DETENT_WINDOWS_JOB_HELPER=parent", "GOCOVERDIR="+t.TempDir())
			Configure(ctx, cmd)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := Start(cmd); err != nil {
				t.Fatal(err)
			}
			groupID := GroupID(cmd)
			t.Cleanup(func() {
				_ = Cleanup(groupID)
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			if groupID <= 0 {
				t.Fatal("started process has no job group")
			}
			var pid int
			if _, err := fmt.Fscanln(output, &pid); err != nil {
				t.Fatalf("grandchild readiness: %v", err)
			}
			grandchild, err := openProcess(pid, windows.SYNCHRONIZE|windows.PROCESS_TERMINATE)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = windows.TerminateProcess(grandchild, 1); _ = windows.CloseHandle(grandchild) })
			if event, err := windows.WaitForSingleObject(grandchild, 0); err != nil || event != uint32(windows.WAIT_TIMEOUT) {
				t.Fatalf("grandchild is not running: event=%d, error=%v", event, err)
			}
			switch mode {
			case "terminate":
				err = TerminateTree(cmd, groupID)
			case "cleanup":
				err = Cleanup(groupID)
			case "cleanup after parent exit":
				if err := input.Close(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if event, err := windows.WaitForSingleObject(grandchild, 0); err != nil || event != uint32(windows.WAIT_TIMEOUT) {
					t.Fatalf("grandchild exited with parent: event=%d, error=%v", event, err)
				}
				err = Cleanup(groupID)
			case "wait":
				if err := input.Close(); err != nil {
					t.Fatal(err)
				}
				err = Wait(cmd)
			case "cancel":
				cancel()
			case "identity":
				identity, inspectErr := Inspect(cmd)
				if inspectErr != nil {
					t.Fatal(inspectErr)
				}
				_, err = Terminate(ctx, identity, time.Second)
			}
			if err != nil {
				t.Fatal(err)
			}
			if event, err := windows.WaitForSingleObject(grandchild, 5000); err != nil || event != windows.WAIT_OBJECT_0 {
				t.Fatalf("grandchild survived %s: event=%d, error=%v", mode, event, err)
			}
			if cmd.ProcessState == nil {
				_ = cmd.Wait()
			}
			if err := Cleanup(groupID); err != nil {
				t.Fatalf("repeated cleanup: %v", err)
			}
		})
	}
}

func TestWindowsJobStart(t *testing.T) {
	for _, mode := range []string{"assigned before resume", "assignment failure", "resume failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			marker := filepath.Join(t.TempDir(), "started")
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsJobHelper$")
			cmd.Env = append(os.Environ(), "DETENT_WINDOWS_JOB_HELPER=marker", "DETENT_WINDOWS_JOB_MARKER="+marker, "GOCOVERDIR="+t.TempDir())
			Configure(ctx, cmd)
			t.Cleanup(func() {
				_ = Cleanup(GroupID(cmd))
				if cmd.Process != nil && cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			failure := errors.New("injected start failure")
			var assignedJob windows.Handle
			resumed := false
			err := startWindows(cmd, func(job, process windows.Handle) error {
				if cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
					return errors.New("worker was not created suspended")
				}
				if mode == "assignment failure" {
					return failure
				}
				if err := windows.AssignProcessToJobObject(job, process); err != nil {
					return err
				}
				assignedJob = job
				return nil
			}, func(pid int) error {
				var members struct {
					Assigned uint32
					Count    uint32
					PIDs     [16]uintptr
				}
				if err := windows.QueryInformationJobObject(assignedJob, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&members)), uint32(unsafe.Sizeof(members)), nil); err != nil {
					return err
				}
				if members.Count != 1 || members.PIDs[0] != uintptr(pid) {
					return fmt.Errorf("worker not assigned before resume: %+v", members)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("worker ran before resume: %v", err)
				}
				if mode == "resume failure" {
					return failure
				}
				resumed = true
				return resumeWindowsProcess(pid)
			})
			if mode != "assigned before resume" {
				if !errors.Is(err, failure) {
					t.Fatalf("Start() = %v, want injected failure", err)
				}
				if resumed || cmd.ProcessState == nil {
					t.Fatal("failed start resumed or did not reap worker")
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed start executed worker: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !resumed {
				t.Fatal("worker was not resumed")
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("resumed worker did not run: %v", err)
			}
		})
	}
}

func TestWindowsJobHelper(t *testing.T) {
	switch os.Getenv("DETENT_WINDOWS_JOB_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsJobHelper$")
		child.Env = append(os.Environ(), "DETENT_WINDOWS_JOB_HELPER=grandchild")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "grandchild":
		event, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, os.Getpid()); err != nil {
			t.Fatal(err)
		}
		_, _ = windows.WaitForSingleObject(event, 30000)
		os.Exit(2)
	case "marker":
		if err := os.WriteFile(os.Getenv("DETENT_WINDOWS_JOB_MARKER"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
}
