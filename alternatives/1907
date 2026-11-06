package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRegistrationClaimStateVerifiesArchivedWorkerLaunch(t *testing.T) {
	f := newRunnerPGFixture(t)
	reader, ok := any(f.service).(interface {
		readRegistrationClaimStateTenant(context.Context, nativeScope, *runnerauth.RoutingSnapshot) error
	})
	if !ok {
		t.Fatal("tenant registration claim-state hydration is unavailable")
	}
	for index, lease := range f.leases {
		data := f.data[index]
		data.Sequence = 2
		data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation"}
		if index == 0 {
			now := time.Now().UTC()
			data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", WorkerLaunch: &tracker.NativeWorkerLaunch{PID: 123, ProcessStart: now, ExecutionVersion: "example", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), ControlVersion: "example", PolicyID: f.policy.PolicyID, Protocol: 1, SelectedAt: now}}
		}
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "observe_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}
		path := "/api/v2/organizations/org_startup/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], event), http.StatusOK)
		if index == 0 {
			reconnect := event
			reconnect.IdempotencyKey = "cleanup_reconnect_" + data.AttemptID
			reconnect.Data.Sequence = 3
			reconnect.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], reconnect), http.StatusOK)
		}

		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, "example")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), "org_startup", apikey.HashToken(f.tokens[index]), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org_startup", project: "prj_runner", credential: credential}
		snapshot := runnerauth.RoutingSnapshot{RunnerID: f.bindings[index].RunnerID}
		if err := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err != nil {
			t.Fatal(err)
		}
		want := 1 - index
		if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != want {
			t.Fatalf("runner %d cleanup = %+v", index, snapshot.ClaimState)
		}
		cleanupPath := "/api/v2/organizations/org_startup/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/attempts/" + data.AttemptID + "?view=cleanup"
		response := performHubAPIRequest(t, f.service, http.MethodGet, cleanupPath, f.tokens[index], nil)
		if index == 0 {
			requireNativeStatus(t, response, http.StatusOK)
			var cleanup tracker.NativeAttempt
			decodeHubResponse(t, response, &cleanup)
			if cleanup.AttemptID != data.AttemptID || cleanup.LeaseID != lease.ID || cleanup.FencingToken != lease.FencingToken || cleanup.ClaimReleasedAt == nil || cleanup.CleanupAcknowledgedAt != nil || cleanup.Runtime == nil || cleanup.Runtime.WorkerLaunch == nil || cleanup.Runtime.WorkerLaunch.PID != 123 {
				t.Fatalf("cleanup identity differs: %+v", cleanup)
			}
		} else {
			requireNativeStatus(t, response, http.StatusNotFound)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, cleanupPath, f.tokens[1-index], nil), http.StatusNotFound)
		if index == 0 {
			if snapshot.ClaimState.ReleasedExecutions[0].AttemptID != data.AttemptID {
				t.Fatalf("cleanup attempt = %+v", snapshot.ClaimState)
			}
			original := f.service.config.RunDetails
			f.service.config.RunDetails = nil
			missingErr := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot)
			_, cleanupErr := f.service.readNativeCleanupAttempt(t.Context(), scope, string(lease.WorkItemID), data.AttemptID)
			f.service.config.RunDetails = original
			if missingErr == nil || cleanupErr == nil {
				t.Fatal("missing archive store was accepted")
			}
			f.service.config.RunDetails = invalidRegistrationArchive{DetailStore: original}
			err := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot)
			_, cleanupErr = f.service.readNativeCleanupAttempt(t.Context(), scope, string(lease.WorkItemID), data.AttemptID)
			f.service.config.RunDetails = original
			if !errors.Is(err, artifact.ErrIntegrity) || !errors.Is(cleanupErr, artifact.ErrIntegrity) {
				t.Fatalf("corrupt archive errors: registration=%v cleanup=%v", err, cleanupErr)
			}
		}
	}
}

type invalidRegistrationArchive struct{ attachment.DetailStore }

func (invalidRegistrationArchive) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return []byte("{}"), nil
}

func TestRegistrationCleanupDoesNotInferAcknowledgment(t *testing.T) {
	released := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		receipt time.Duration
		role    string
		pid     int
		corrupt bool
		want    bool
	}{
		{"before release", -time.Microsecond, "code", 123, false, true},
		{"at release", 0, "code", 123, false, true},
		{"after release", time.Microsecond, "code", 123, false, true},
		{"no execution role", -time.Microsecond, "", 123, false, false},
		{"no worker process", -time.Microsecond, "code", 0, false, false},
		{"wrong archived fence", -time.Microsecond, "code", 123, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := tenantAttemptSnapshot{attempt: leasestore.Attempt{ID: "attempt", RunID: "run", Sequence: 2, Status: "cancelled"}, lease: leasestore.Lease{LeaseID: "lease", FencingToken: 1, ReleasedAt: &released}, event: eventstore.Event{Type: "run.finished", RecordedAt: released.Add(tc.receipt)}}
			run := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", Sequence: 2, LeaseID: "lease", FencingToken: 1, RunnerID: "runner", Outcome: "cancelled", Identity: &tracker.NativeExecutionIdentity{Role: tc.role}, Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{PID: tc.pid, ProcessStart: released.Add(-time.Hour)}}}
			if tc.corrupt {
				run.FencingToken = 2
			}
			got, err := registrationCleanupRequired(snapshot, run, "runner")
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

func TestRegistrationCleanupRejectsStalePayload(t *testing.T) {
	released := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		gap     int64
		delta   time.Duration
		outcome string
		invalid bool
	}{
		{"synthetic cancellation", 1, 0, "", false},
		{"sequence gap", 2, 0, "", true},
		{"unrelated update", 1, time.Microsecond, "", true},
		{"terminal payload", 1, 0, "cancelled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := tenantAttemptSnapshot{attempt: leasestore.Attempt{ID: "attempt", RunID: "run", Sequence: 2 + tc.gap, Status: "cancelled", UpdatedAt: released.Add(tc.delta)}, lease: leasestore.Lease{LeaseID: "lease", FencingToken: 1, ReleasedAt: &released}}
			run := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", Sequence: 2, LeaseID: "lease", FencingToken: 1, RunnerID: "runner", Outcome: tc.outcome}
			_, err := registrationCleanupRequired(snapshot, run, "runner")
			if errors.Is(err, artifact.ErrIntegrity) != tc.invalid {
				t.Fatalf("archive identity error = %v, invalid=%t", err, tc.invalid)
			}
		})
	}
}
