//go:build windows

package procgroup

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWindowsInspectAndTerminate(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestWindowsProcessHelper$")
	cmd.Env = append(os.Environ(), "DETENT_WINDOWS_PROCESS_HELPER=1")
	Configure(cmd)
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

	outcome, err = Terminate(context.Background(), identity, time.Second)
	if err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}
	if outcome != TerminationOutcomeTerminated {
		t.Fatalf("Terminate() outcome = %q, want %q", outcome, TerminationOutcomeTerminated)
	}
	_ = cmd.Wait()
}

func TestWindowsProcessHelper(t *testing.T) {
	if os.Getenv("DETENT_WINDOWS_PROCESS_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}
