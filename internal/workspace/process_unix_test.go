//go:build unix

package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReapProcesses(t *testing.T) {
	const processID = 2081001

	scanErr := errors.New("scan failed")
	waitErr := errors.New("wait failed")
	tests := []struct {
		name          string
		initial       []int
		scanErr       error
		waits         [][]int
		waitErr       error
		signalErr     error
		wantSignals   []syscall.Signal
		wantReaped    int
		wantErr       error
		wantSubstring string
	}{
		{name: "empty workspace"},
		{
			name:        "term clears workspace",
			initial:     []int{processID},
			waits:       [][]int{nil},
			wantSignals: []syscall.Signal{syscall.SIGTERM},
			wantReaped:  1,
		},
		{
			name:        "kill clears term survivor",
			initial:     []int{processID},
			waits:       [][]int{{processID}, nil},
			wantSignals: []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL},
			wantReaped:  1,
		},
		{
			name:       "initial scan fails",
			scanErr:    scanErr,
			wantErr:    scanErr,
			wantReaped: 0,
		},
		{
			name:        "wait fails",
			initial:     []int{processID},
			waitErr:     waitErr,
			wantSignals: []syscall.Signal{syscall.SIGTERM},
			wantReaped:  1,
			wantErr:     waitErr,
		},
		{
			name:          "kill leaves survivor",
			initial:       []int{processID},
			waits:         [][]int{{processID}, {processID}},
			wantSignals:   []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL},
			wantReaped:    1,
			wantSubstring: "remained after SIGKILL",
		},
		{
			name:        "signal failure is preserved",
			initial:     []int{processID},
			waits:       [][]int{nil},
			signalErr:   syscall.EPERM,
			wantSignals: []syscall.Signal{syscall.SIGTERM},
			wantErr:     syscall.EPERM,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var signals []syscall.Signal
			scan := func(context.Context, string) ([]int, error) {
				return append([]int(nil), tt.initial...), tt.scanErr
			}
			signal := func(pid int, sig syscall.Signal) error {
				if pid != processID {
					t.Fatalf("signal pid = %d, want %d", pid, processID)
				}
				signals = append(signals, sig)
				return tt.signalErr
			}
			waitCall := 0
			wait := func(context.Context, string, time.Duration, workspaceProcessScanner) ([]int, error) {
				if tt.waitErr != nil {
					return nil, tt.waitErr
				}
				if waitCall >= len(tt.waits) {
					t.Fatalf("wait call %d exceeds configured results", waitCall+1)
				}
				result := append([]int(nil), tt.waits[waitCall]...)
				waitCall++
				return result, nil
			}

			reaped, err := reapProcessesWithWait(t.Context(), "/workspace", time.Second, scan, signal, wait)
			if reaped != tt.wantReaped {
				t.Fatalf("reaped = %d, want %d", reaped, tt.wantReaped)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && tt.wantSubstring == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tt.wantSubstring != "" && (err == nil || !strings.Contains(err.Error(), tt.wantSubstring)) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantSubstring)
			}
			if len(signals) != len(tt.wantSignals) {
				t.Fatalf("signals = %v, want %v", signals, tt.wantSignals)
			}
			for index := range signals {
				if signals[index] != tt.wantSignals[index] {
					t.Fatalf("signals = %v, want %v", signals, tt.wantSignals)
				}
			}
		})
	}
}

func TestReapProcessesRejectsUnsafePaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "whitespace", path: "  "},
		{name: "relative", path: "workspace"},
		{name: "filesystem root", path: string(filepath.Separator)},
		{name: "cleaned filesystem root", path: filepath.Join(string(filepath.Separator), "tmp", "..")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReapProcesses(t.Context(), tt.path, time.Second)
			if err == nil {
				t.Fatal("ReapProcesses() error = nil, want non-nil")
			}
		})
	}
}

func TestWorkspaceScanOutput(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		missing      bool
		cancelBefore bool
		expired      bool
		cancelReady  bool
		wantStage    string
		wantContext  string
		wantOutput   string
		wantExit     bool
	}{
		{name: "success", command: "printf 123", wantOutput: "123"},
		{name: "missing executable", missing: true, wantStage: "start", wantContext: "<nil>"},
		{name: "canceled before start", cancelBefore: true, wantStage: "start", wantContext: "context canceled"},
		{name: "deadline before start", expired: true, wantStage: "start", wantContext: "context deadline exceeded"},
		{name: "no matches exit", command: "exit 1", wantStage: "wait", wantContext: "<nil>", wantExit: true},
		{name: "partial output failure", command: "printf 123; exit 2", wantOutput: "123", wantStage: "wait", wantContext: "<nil>", wantExit: true},
		{name: "exit failure", command: "exit 2", wantStage: "wait", wantContext: "<nil>", wantExit: true},
		{name: "signal without cancellation", command: "kill -KILL $$", wantStage: "wait", wantContext: "<nil>", wantExit: true},
		{name: "canceled after readiness", command: "printf ready >&3; read value", cancelReady: true, wantStage: "wait", wantContext: "context canceled", wantExit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if tt.expired {
				var expire context.CancelFunc
				ctx, expire = context.WithDeadline(ctx, time.Time{})
				defer expire()
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", tt.command)
			if tt.missing {
				cmd = exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing"))
			}
			if tt.cancelBefore {
				cancel()
			}
			if tt.cancelReady {
				readyReader, readyWriter, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer readyReader.Close()
				defer readyWriter.Close()
				inputReader, inputWriter, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer inputReader.Close()
				defer inputWriter.Close()
				cmd.Stdin = inputReader
				cmd.ExtraFiles = []*os.File{readyWriter}
				if err := readyReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				ready := make(chan error, 1)
				go func() {
					_, err := io.ReadFull(readyReader, make([]byte, 5))
					cancel()
					ready <- err
				}()
				defer func() {
					if err := <-ready; err != nil {
						t.Errorf("readiness: %v", err)
					}
				}()
			}
			output, err := workspaceScanOutput(ctx, cmd)
			if string(output) != tt.wantOutput {
				t.Errorf("output = %q, want %q", output, tt.wantOutput)
			}
			if tt.wantStage == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected command error")
			}
			for _, field := range []string{"stage=" + tt.wantStage, "start_elapsed=", "deadline_set=true", "deadline_remaining=", "context_error=" + tt.wantContext} {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("error %q missing %q", err, field)
				}
			}
			if tt.wantStage == "wait" {
				for _, field := range []string{"wait_elapsed=", "pid=", "output_bytes=" + strconv.Itoa(len(tt.wantOutput)), "context_at_start=<nil>"} {
					if !strings.Contains(err.Error(), field) {
						t.Errorf("error %q missing %q", err, field)
					}
				}
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) != tt.wantExit {
				t.Errorf("exit error = %v, want %t", err, tt.wantExit)
			}
			if tt.cancelBefore && !errors.Is(err, context.Canceled) {
				t.Errorf("cancellation identity lost: %v", err)
			}
			if tt.expired && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("deadline identity lost: %v", err)
			}
			t.Log(err)
		})
	}
}
