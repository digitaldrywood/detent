package hubserver

import (
	"context"
	"encoding/json"
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
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
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
		t.Run("invalid cleanup cursor", func(t *testing.T) {
			key := releasedExecutionCursorKey{organization: scope.organization, project: scope.project, runner: snapshot.RunnerID}
			previous, present := f.service.cleanupPollCursors.Load(key)
			t.Cleanup(func() {
				if present {
					f.service.cleanupPollCursors.Store(key, previous)
				} else {
					f.service.cleanupPollCursors.Delete(key)
				}
			})
			f.service.cleanupPollCursors.Store(key, 17)
			if err := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err == nil {
				t.Fatal("invalid cleanup cursor was accepted")
			}
		})
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
			var cleanup tracker.NativeAttempt
			decodeHubResponse(t, response, &cleanup)
			finished := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "cleanup_finished_" + data.AttemptID}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: data.AttemptID, RunID: data.RunID, Sequence: cleanup.Sequence + 1, LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: cleanup.PolicyID, Identity: cleanup.Identity, Outcome: "cancelled"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], finished), http.StatusOK)
			if err := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
				t.Fatalf("acknowledged cleanup remained eligible: %+v", snapshot.ClaimState)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				claims, err := r.(runnerstore.ClaimStateReader).RunnerClaimState(t.Context(), snapshot.RunnerID, scope.project, "", 100, time.Now().UTC())
				if err == nil && len(claims.Candidates) != 0 {
					t.Fatalf("heartbeat reoffered acknowledged cleanup: %+v", claims.Candidates)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, cleanupPath, f.tokens[index], nil), http.StatusNotFound)
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
		name         string
		receipt      time.Duration
		role         string
		pid          int
		corrupt      bool
		acknowledged bool
		want         bool
	}{
		{"before release", -time.Microsecond, "code", 123, false, false, true},
		{"at release", 0, "code", 123, false, false, true},
		{"after release", time.Microsecond, "code", 123, false, false, true},
		{"explicit cleanup acknowledgment", time.Microsecond, "code", 123, false, true, false},
		{"no execution role", -time.Microsecond, "", 123, false, false, false},
		{"no worker process", -time.Microsecond, "code", 0, false, false, false},
		{"wrong archived fence", -time.Microsecond, "code", 123, true, false, false},
		{"acknowledged wrong archived fence", time.Microsecond, "code", 123, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := tenantAttemptSnapshot{attempt: leasestore.Attempt{ID: "attempt", RunID: "run", Sequence: 2, Status: "cancelled"}, lease: leasestore.Lease{LeaseID: "lease", FencingToken: 1, ReleasedAt: &released}, event: eventstore.Event{Type: "run.finished", RecordedAt: released.Add(tc.receipt)}}
			run := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", Sequence: 2, LeaseID: "lease", FencingToken: 1, RunnerID: "runner", Outcome: "cancelled", Identity: &tracker.NativeExecutionIdentity{Role: tc.role}, Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{PID: tc.pid, ProcessStart: released.Add(-time.Hour)}}}
			if tc.acknowledged {
				at := released.Add(time.Microsecond)
				snapshot.attempt.CleanupAcknowledgedAt = &at
			}
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
			content := eventstore.NewContent(&memoryRunDetails{})
			raw, err := json.Marshal(tracker.CollaborationEvent{ID: "event", OrganizationID: "org_example", ProjectID: "prj_example", AggregateID: "wi_example", Type: "run.observed", SchemaVersion: 1, RecordedAt: released.Add(-time.Hour), Data: tracker.CollaborationData{Run: &run}})
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), "org_example", raw)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.attempt.LeaseID, snapshot.attempt.FencingToken = "lease", 1
			_, _, err = readTenantRunCleanupData(t.Context(), content, &tenantRunCleanupPredecessor{attempt: snapshot.attempt, lease: snapshot.lease, event: event})
			if errors.Is(err, artifact.ErrIntegrity) != tc.invalid {
				t.Fatalf("cleanup archive error = %v, invalid=%t", err, tc.invalid)
			}
		})
	}
}
