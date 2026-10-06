//go:build windows

package procgroup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

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
