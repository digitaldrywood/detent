package cli

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

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
				if active := attempt.Status == store.WorkAttemptStatusActive; active != check.wantActive {
					t.Fatalf("attempt %s active = %t, want %t (%+v)", attempt.IssueID, active, check.wantActive, attempt)
				}
			}
		})
	}
}
