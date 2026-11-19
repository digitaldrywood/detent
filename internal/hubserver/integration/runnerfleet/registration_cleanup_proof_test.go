package runnerfleet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/executionapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRegistrationCleanupDoesNotInferAcknowledgment(t *testing.T) {
	t.Parallel()
	released := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		receipt      time.Duration
		role         string
		pid          int
		corrupt      bool
		acknowledged bool
		want         bool
	}{
		{"after release", time.Microsecond, "code", 123, false, false, true},
		{"explicit cleanup acknowledgment", time.Microsecond, "code", 123, false, true, false},
		{"no execution role", -time.Microsecond, "", 123, false, false, false},
		{"no worker process", -time.Microsecond, "code", 0, false, false, false},
		{"wrong archived fence", -time.Microsecond, "code", 123, true, false, false},
		{"acknowledged wrong archived fence", time.Microsecond, "code", 123, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: leasestore.Attempt{ID: "attempt", RunID: "run", Sequence: 2, Status: "cancelled"}, Lease: leasestore.Lease{LeaseID: "lease", FencingToken: 1, ReleasedAt: &released}, Event: eventstore.Event{Type: "run.finished", RecordedAt: released.Add(tc.receipt)}}
			run := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", Sequence: 2, LeaseID: "lease", FencingToken: 1, RunnerID: "runner", Outcome: "cancelled", Identity: &tracker.NativeExecutionIdentity{Role: tc.role}, Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{PID: tc.pid, ProcessStart: released.Add(-time.Hour)}}}
			if tc.acknowledged {
				at := released.Add(time.Microsecond)
				snapshot.Attempt.CleanupAcknowledgedAt = &at
			}
			if tc.corrupt {
				run.FencingToken = 2
			}
			got, err := (runnerfleet.HeartbeatDeps{AttemptRunMatches: executionapi.NativeAttemptRunMatches}).RegistrationCleanupRequired(snapshot, run, "runner")
			if tc.corrupt {
				if !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("identity error = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("cleanup required = %t, %v; want %t", got, err, tc.want)
			}
		})
	}
}
