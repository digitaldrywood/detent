package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestAdmissionWorkspaceCleanupIsScopedToCreatedPath(t *testing.T) {
	t.Parallel()

	backend := &admissionWorkspace{}
	info, err := backend.Create(context.Background(), workspace.Issue{ID: "detent/admission"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(info.Path)
	})

	if err := backend.Cleanup(context.Background(), filepath.Join(info.Path, "other")); err == nil {
		t.Fatal("Cleanup(other) error = nil")
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("created workspace stat error = %v", err)
	}
	backend.AfterRun(context.Background(), info, workspace.Issue{})
	if _, err := os.Stat(info.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace stat error = %v, want os.ErrNotExist", err)
	}
}

func TestAdmissionWorkspaceCleanupRetriesENOTEMPTY(t *testing.T) {
	tests := []struct {
		name        string
		failures    int
		failure     error
		wantCalls   int
		wantWaits   []time.Duration
		wantRemoved bool
	}{
		{name: "first attempt succeeds", wantCalls: 1, wantRemoved: true},
		{
			name:        "ENOTEMPTY self heals",
			failures:    2,
			failure:     &os.PathError{Op: "unlinkat", Path: "cache", Err: syscall.ENOTEMPTY},
			wantCalls:   3,
			wantWaits:   []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
			wantRemoved: true,
		},
		{
			name:      "ENOTEMPTY exhausts bound",
			failures:  4,
			failure:   &os.PathError{Op: "unlinkat", Path: "cache", Err: syscall.ENOTEMPTY},
			wantCalls: 4,
			wantWaits: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
		},
		{
			name:      "permission failure is not retried",
			failures:  1,
			failure:   &os.PathError{Op: "unlinkat", Path: "cache", Err: syscall.EPERM},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &admissionWorkspace{}
			info, err := backend.Create(context.Background(), workspace.Issue{ID: "detent/admission"})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			t.Cleanup(func() {
				_ = os.RemoveAll(info.Path)
			})

			calls := 0
			backend.removeAll = func(path string) error {
				calls++
				if calls <= tt.failures {
					return tt.failure
				}
				return os.RemoveAll(path)
			}
			var waits []time.Duration
			backend.wait = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}

			err = backend.Cleanup(context.Background(), info.Path)
			if tt.wantRemoved && err != nil {
				t.Fatalf("Cleanup() error = %v", err)
			}
			if !tt.wantRemoved && !errors.Is(err, errors.Unwrap(tt.failure)) {
				t.Fatalf("Cleanup() error = %v, want %v", err, tt.failure)
			}
			if calls != tt.wantCalls {
				t.Fatalf("remove calls = %d, want %d", calls, tt.wantCalls)
			}
			if len(waits) != len(tt.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tt.wantWaits)
			}
			for i := range waits {
				if waits[i] != tt.wantWaits[i] {
					t.Fatalf("waits = %v, want %v", waits, tt.wantWaits)
				}
			}
			_, statErr := os.Stat(info.Path)
			if tt.wantRemoved && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("workspace stat error = %v, want os.ErrNotExist", statErr)
			}
			if !tt.wantRemoved && statErr != nil {
				t.Fatalf("workspace stat error = %v, want workspace retained", statErr)
			}
		})
	}
}

func TestAdmissionWorkspacePersistentFailuresReportAccumulation(t *testing.T) {
	var logs bytes.Buffer
	tracker := &admissionWorkspaceLeakTracker{}
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var leakedPaths []string
	t.Cleanup(func() {
		for _, path := range leakedPaths {
			_ = os.RemoveAll(path)
		}
	})
	tests := []struct {
		name      string
		contents  string
		wantCount string
		wantBytes string
	}{
		{name: "first leak", contents: "one", wantCount: "leaked_workspace_count=1", wantBytes: "leaked_workspace_bytes=3"},
		{name: "second leak", contents: "three", wantCount: "leaked_workspace_count=2", wantBytes: "leaked_workspace_bytes=8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &admissionWorkspace{logger: logger, leaks: tracker}
			info, err := backend.Create(context.Background(), workspace.Issue{ID: "detent/admission"})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			leakedPaths = append(leakedPaths, info.Path)
			cachePath := filepath.Join(info.Path, ".detent", "tmp", "node-compile-cache", "cache-entry")
			if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(cachePath, []byte(tt.contents), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			backend.removeAll = func(string) error {
				return &os.PathError{Op: "unlinkat", Path: cachePath, Err: syscall.ENOTEMPTY}
			}
			backend.wait = func(context.Context, time.Duration) error { return nil }

			logs.Reset()
			backend.AfterRun(context.Background(), info, workspace.Issue{})
			got := logs.String()
			for _, want := range []string{
				"remove backlog admission workspace failed after retries",
				tt.wantCount,
				tt.wantBytes,
				".detent/tmp/node-compile-cache/cache-entry",
				"directory not empty",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("log = %q, want %q", got, want)
				}
			}
		})
	}
}
