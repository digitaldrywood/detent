package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

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
