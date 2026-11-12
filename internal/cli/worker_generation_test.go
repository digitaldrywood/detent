package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type missingWorkerProcessStore struct {
	shutdownWorkerProcessStore
}

func (*missingWorkerProcessStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return store.WorkAttempt{}, store.ErrNotFound
}

func TestMissingProcessRegistrationDoesNotInventOwnership(t *testing.T) {
	t.Parallel()
	backend := &missingWorkerProcessStore{}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if err := reapWorkerProcessesWithCleanup(t.Context(), backend, nil, "startup", 0, func() time.Time { return now },
		func(context.Context, procgroup.Identity, time.Duration) (procgroup.TerminationOutcome, error) {
			t.Error("missing registration authorized a process signal")
			return procgroup.TerminationOutcomeStaleIdentity, nil
		}, func(store.WorkerProcess) error {
			t.Error("missing registration authorized artifact cleanup")
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	attempt := tracker.NativeAttempt{StartedAt: now, NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{
		LocalAttemptID: 1,
		WorkerLaunch:   &tracker.NativeWorkerLaunch{PID: os.Getpid(), GroupID: os.Getpid(), ProcessStart: now},
	}}}
	if stopped, err := releasedExecutionWorkersStopped(t.Context(), backend, "wi_example", attempt); err != nil || stopped {
		t.Fatalf("missing process registration acknowledged worker exit: stopped=%t error=%v", stopped, err)
	}
}
