package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

func TestWindowsScratchInspectionConfirmsProcessExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsScratchInspectionHelper$")
	cmd.Env = append(os.Environ(), "DETENT_WINDOWS_INSPECTION_HELPER=1", "GOCOVERDIR="+t.TempDir())
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if _, err := io.ReadFull(output, make([]byte, 1)); err != nil {
		t.Fatalf("wait for readiness: %v", err)
	}
	p, err := process.NewProcessWithContext(ctx, int32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	inspectionErr := errors.New("cannot read process PEB")
	for _, exited := range []bool{false, true} {
		if exited {
			if err := input.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		}
		err := scratchProcessInspectionError(ctx, cmd.Process.Pid, "directory", inspectionErr, p.IsRunningWithContext)
		if exited && err != nil {
			t.Fatalf("exited process inspection: %v", err)
		}
		if !exited && !errors.Is(err, inspectionErr) {
			t.Fatalf("live process lost inspection error: %v", err)
		}
	}
}

func TestWindowsScratchInspectionHelper(t *testing.T) {
	if os.Getenv("DETENT_WINDOWS_INSPECTION_HELPER") != "1" {
		return
	}
	if _, err := os.Stdout.Write([]byte("r")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}
