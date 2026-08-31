package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestReapWorkerProcessesCleansArtifactsOnlyAfterVerifiedExit(t *testing.T) {
	t.Parallel()

	reapErr := errors.New("process group remained alive")
	tests := []struct {
		name       string
		reapErr    error
		wantExists bool
		wantReaped bool
	}{
		{name: "verified exit", wantReaped: true},
		{name: "unverified exit", reapErr: reapErr, wantExists: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cleanupRoot := t.TempDir()
			cleanupPath := filepath.Join(cleanupRoot, "run-2011")
			if err := os.MkdirAll(filepath.Join(cleanupPath, ".detent", "tmp"), 0o700); err != nil {
				t.Fatalf("create cleanup path: %v", err)
			}
			processStore := &shutdownWorkerProcessStore{processes: []store.WorkerProcess{{
				SessionID: 2011,
				WorkerProcessIdentity: store.WorkerProcessIdentity{
					PID:       4242,
					GroupID:   4242,
					StartedAt: time.Date(2026, 8, 27, 12, 40, 0, 0, time.UTC),
				},
				CleanupRoot: cleanupRoot,
				CleanupPath: cleanupPath,
			}}}

			err := reapWorkerProcesses(
				context.Background(),
				processStore,
				slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
				"startup",
				time.Millisecond,
				time.Now,
				func(context.Context, procgroup.Identity, time.Duration) (procgroup.TerminationOutcome, error) {
					return procgroup.TerminationOutcomeTerminated, tt.reapErr
				},
			)
			if got := errors.Is(err, reapErr); got != (tt.reapErr != nil) {
				t.Fatalf("reapWorkerProcesses() error = %v, want reap error %v", err, tt.reapErr != nil)
			}
			_, statErr := os.Stat(cleanupPath)
			if got := statErr == nil; got != tt.wantExists {
				t.Fatalf("cleanup path exists = %v, want %v, stat error = %v", got, tt.wantExists, statErr)
			}
			if got := len(processStore.reaped) == 1; got != tt.wantReaped {
				t.Fatalf("process marked reaped = %v, want %v", got, tt.wantReaped)
			}
		})
	}
}

func TestReapWorkerProcessesAtStartup(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 12, 16, 0, 0, 0, time.UTC)
	reapedAt := startedAt.Add(time.Minute)
	processStore := &shutdownWorkerProcessStore{processes: []store.WorkerProcess{
		{
			SessionID:  1214,
			Identifier: "digitaldrywood/detent#1214",
			WorkerProcessIdentity: store.WorkerProcessIdentity{
				PID:       4242,
				GroupID:   4242,
				StartedAt: startedAt,
			},
		},
		{
			SessionID:  1215,
			Identifier: "digitaldrywood/detent#1215",
			WorkerProcessIdentity: store.WorkerProcessIdentity{
				PID:       4343,
				GroupID:   4343,
				StartedAt: startedAt,
			},
		},
	}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	err := reapWorkerProcesses(
		context.Background(),
		processStore,
		logger,
		"startup",
		time.Millisecond,
		func() time.Time { return reapedAt },
		func(_ context.Context, identity procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
			if identity.PID == 4242 {
				return procgroup.TerminationOutcomeTerminated, nil
			}
			return procgroup.TerminationOutcomeAlreadyExited, nil
		},
	)
	if err != nil {
		t.Fatalf("reapWorkerProcesses() error = %v", err)
	}
	if len(processStore.reaped) != 2 {
		t.Fatalf("reaped processes = %#v", processStore.reaped)
	}
	if processStore.reaped[0].reap.Outcome != store.WorkerProcessOutcomeTerminated || processStore.reaped[1].reap.Outcome != store.WorkerProcessOutcomeAlreadyExited {
		t.Fatalf("reap outcomes = %#v", processStore.reaped)
	}
	for _, reaped := range processStore.reaped {
		if !reaped.reap.ReapedAt.Equal(reapedAt) {
			t.Fatalf("reaped at = %s, want %s", reaped.reap.ReapedAt, reapedAt)
		}
		if reaped.reap.Reason != "startup" {
			t.Fatalf("reap reason = %q, want startup", reaped.reap.Reason)
		}
	}
	for _, want := range []string{
		"reason=startup",
		"decision=terminated",
		"decision=already_exited",
		"issue_identifier=digitaldrywood/detent#1214",
		"issue_identifier=digitaldrywood/detent#1215",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs missing %q:\n%s", want, logs.String())
		}
	}
}
