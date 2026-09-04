//go:build unix

package workspace

import (
	"context"
	"errors"
	"path/filepath"
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
