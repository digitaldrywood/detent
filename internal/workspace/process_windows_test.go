package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"
)

func TestWindowsScratchProcessAlive(t *testing.T) {
	for _, tt := range []struct {
		name      string
		alive     bool
		err       error
		wantAlive bool
		wantErr   bool
	}{
		{name: "live", alive: true, wantAlive: true},
		{name: "exited"},
		{name: "not running", err: process.ErrorProcessNotRunning},
		{name: "vanished pid", err: windows.ERROR_INVALID_PARAMETER},
		{name: "wrapped vanished pid", err: fmt.Errorf("creation time: %w", windows.ERROR_INVALID_PARAMETER)},
		{name: "access denied", err: windows.ERROR_ACCESS_DENIED, wantErr: true},
		{name: "observation failed", err: errors.New("observation failed"), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			alive, err := windowsScratchProcessAlive(t.Context(), func(context.Context) (bool, error) {
				return tt.alive, tt.err
			})
			if alive != tt.wantAlive || (err != nil) != tt.wantErr {
				t.Fatalf("alive = %t, error = %v, want %t, error %t", alive, err, tt.wantAlive, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, tt.err) {
				t.Fatalf("observation error lost: %v", err)
			}
		})
	}
}

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
		err := scratchProcessInspectionError(ctx, cmd.Process.Pid, "directory", inspectionErr, func(ctx context.Context) (bool, error) {
			return windowsScratchProcessAlive(ctx, p.IsRunningWithContext)
		})
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
