package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMissingRuntimeDatabaseDoesNotInventProcessOwnership(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		headless bool
	}{
		{name: "local board"},
		{name: "headless runner", headless: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "detent.db")
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime database already exists: %v", err)
			}
			backend, err := openRuntimeStore(t.Context(), BootConfig{RuntimeDBPath: path, Headless: tc.headless})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			})
			now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			if _, err := startWorkerGeneration(t.Context(), backend, "example", now); err != nil {
				t.Fatal(err)
			}
			if err := reapWorkerProcessesWithCleanup(t.Context(), backend, nil, "startup", 0, func() time.Time { return now },
				func(context.Context, procgroup.Identity, time.Duration) (procgroup.TerminationOutcome, error) {
					t.Error("empty runtime database authorized a process signal")
					return procgroup.TerminationOutcomeStaleIdentity, nil
				}, func(store.WorkerProcess) error {
					t.Error("empty runtime database authorized artifact cleanup")
					return nil
				}); err != nil {
				t.Fatal(err)
			}
			attempt := tracker.NativeAttempt{StartedAt: now, NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{
				LocalAttemptID: 1,
				WorkerLaunch:   &tracker.NativeWorkerLaunch{PID: os.Getpid(), GroupID: os.Getpid(), ProcessStart: now},
			}}}
			if stopped, err := releasedExecutionWorkersStopped(t.Context(), backend, "wi_example", attempt); err != nil || stopped {
				t.Fatalf("missing local identity acknowledged worker exit: stopped=%t error=%v", stopped, err)
			}
		})
	}
}

func TestStartupReapSkipsLiveSiblingGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tc := range []struct {
		name          string
		siblingExited bool
		wantReaped    []int
	}{
		{name: "live sibling generation", wantReaped: []int{4343}},
		{name: "exited sibling generation", siblingExited: true, wantReaped: []int{4242, 4343}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "attempts.db")
			open := func() store.Store {
				backend, err := store.Open(t.Context(), store.Config{Path: path})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { backend.Close() })
				return backend
			}
			now := time.Now().UTC()
			seed := func(backend store.Store, issueID string, pid int) int64 {
				id, err := backend.StartWorkAttempt(t.Context(), store.WorkAttemptStart{
					ProjectID: "detent", IssueID: issueID, WorkerType: "agent",
					StartedAt: now.Add(-time.Hour), LeaseExpiresAt: now.Add(time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
				sessionID, err := backend.StartSession(t.Context(), store.SessionStart{ProjectID: "detent", IssueID: issueID, WorkAttemptID: id, StartedAt: now.Add(-time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				if err := backend.UpdateSessionWorkerProcess(t.Context(), sessionID, store.WorkerProcessRegistration{
					WorkerProcessIdentity: store.WorkerProcessIdentity{PID: pid, GroupID: pid, StartedAt: now},
				}); err != nil {
					t.Fatal(err)
				}
				return id
			}
			sibling := open()
			if _, err := startWorkerGeneration(t.Context(), sibling, "old", now); err != nil {
				t.Fatal(err)
			}
			siblingAttempt := seed(sibling, "sibling", 4242)
			legacyAttempt := seed(open(), "legacy", 4343)
			if tc.siblingExited {
				if err := sibling.ExitWorkerGeneration(t.Context(), now); err != nil {
					t.Fatal(err)
				}
			}
			current := open()
			if _, err := startWorkerGeneration(t.Context(), current, "new", now); err != nil {
				t.Fatal(err)
			}

			nativeAttempt := tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: legacyAttempt}}, StartedAt: now}
			if stopped, err := releasedExecutionWorkersStopped(t.Context(), current, "legacy", nativeAttempt); err != nil || stopped {
				t.Fatalf("live provider acknowledged: stopped=%t error=%v", stopped, err)
			}
			var reaped []int
			err := reapWorkerProcessesWithCleanup(t.Context(), current, nil, "startup", time.Second, func() time.Time { return now },
				func(_ context.Context, identity procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
					reaped = append(reaped, identity.PID)
					return procgroup.TerminationOutcomeAlreadyExited, nil
				}, func(store.WorkerProcess) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(reaped)
			if !slices.Equal(reaped, tc.wantReaped) {
				t.Fatalf("reaped pids = %v, want %v", reaped, tc.wantReaped)
			}
			for _, check := range []struct {
				id         int64
				wantActive bool
			}{{siblingAttempt, !tc.siblingExited}, {legacyAttempt, false}} {
				attempt, err := current.WorkAttempt(t.Context(), check.id)
				if err != nil {
					t.Fatal(err)
				}

				nativeAttempt.Runtime.LocalAttemptID = check.id
				stopped, err := releasedExecutionWorkersStopped(t.Context(), current, tracker.NativeWorkItemID(attempt.IssueID), nativeAttempt)
				if err != nil || stopped == check.wantActive {
					t.Fatalf("reaped native execution stopped=%t active=%t error=%v", stopped, check.wantActive, err)
				}
				if stopped, err := releasedExecutionWorkersStopped(t.Context(), current, "unrelated", nativeAttempt); err != nil || stopped {
					t.Fatalf("foreign local attempt acknowledged: stopped=%t error=%v", stopped, err)
				}
				if active := attempt.Status == store.WorkAttemptStatusActive; active != check.wantActive {
					t.Fatalf("attempt %s active = %t, want %t (%+v)", attempt.IssueID, active, check.wantActive, attempt)
				}
			}
		})
	}
}
