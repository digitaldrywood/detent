package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRegistrationClaimStateVerifiesArchivedWorkerLaunch(t *testing.T) {
	f := newRunnerPGFixture(t)
	reader, ok := any(f.service).(interface {
		readRegistrationClaimStateTenant(context.Context, hubscope.NativeScope, *runnerauth.RoutingSnapshot) error
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
		scope := hubscope.NativeScope{Organization: "org_startup", Project: "prj_runner", Credential: credential}
		snapshot := runnerauth.RoutingSnapshot{RunnerID: f.bindings[index].RunnerID}
		t.Run("invalid cleanup cursor", func(t *testing.T) {
			key := releasedExecutionCursorKey{organization: scope.Organization, project: scope.Project, runner: snapshot.RunnerID}
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
		admin := f.admin
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			current, err := r.Attempt(t.Context(), data.AttemptID)
			if err == nil && (current.WorkerCleanupRequired == nil || *current.WorkerCleanupRequired != (index == 0) || current.RuntimeSummarySequence != current.Sequence) {
				t.Fatalf("producer cleanup fact = %+v", current)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET worker_cleanup_required=NULL,runtime_summary_sequence=NULL WHERE organization_id=$1 AND id=$2", scope.Organization, data.AttemptID); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			for i := range 100 {
				id := fmt.Sprintf("a_invalid_%03d", i)
				if id >= data.AttemptID {
					t.Fatalf("invalid-prefix fixture sorts after worker: %q >= %q", id, data.AttemptID)
				}
				var fence int64
				if err := admin.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at,released_at,project_id,runner_id) SELECT organization_id,$3,issue_id,machine_id,$3,work_item_revision,expires_at,acquired_at,renewed_at,released_at,project_id,runner_id FROM leases WHERE organization_id=$1 AND lease_id=$2 RETURNING fencing_token`, scope.Organization, lease.ID, id).Scan(&fence); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) SELECT organization_id,$3,runner_id,assigned_at FROM runner_lease_assignments WHERE organization_id=$1 AND lease_id=$2", scope.Organization, lease.ID, id); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) SELECT organization_id,$3,issue_id FROM native_attempt_runs WHERE organization_id=$1 AND run_id=$2", scope.Organization, data.RunID, id); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at) SELECT organization_id,project_id,$3,issue_id,$3,$4,$3,1,'cancelled',work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at FROM native_attempts WHERE organization_id=$1 AND id=$2`, scope.Organization, data.AttemptID, id, fence); err != nil {
					t.Fatal(err)
				}
			}
			if err := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
				t.Fatalf("unverified prefix offered cleanup: %+v", snapshot.ClaimState)
			}
			key := releasedExecutionCursorKey{organization: scope.Organization, project: scope.Project, runner: snapshot.RunnerID}
			if cursor, ok := f.service.cleanupPollCursors.Load(key); !ok || cursor != "a_invalid_099" {
				t.Fatalf("unknown prefix did not advance bounded cursor: %v", cursor)
			}
		}
		readSnapshot := func() error {
			for range 2 {
				if err := reader.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err != nil {
					return err
				}
				if snapshot.ClaimState != nil && len(snapshot.ClaimState.ReleasedExecutions) != 0 {
					break
				}
			}
			return nil
		}
		if err := readSnapshot(); err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			current, err := r.Attempt(t.Context(), data.AttemptID)
			if err == nil && (current.WorkerCleanupRequired == nil || *current.WorkerCleanupRequired != (index == 0) || current.RuntimeSummarySequence != current.Sequence) {
				t.Fatalf("fallback cleanup fact = %+v", current)
			}
			return err
		}); err != nil {
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
			missingErr := readSnapshot()
			_, cleanupErr := f.service.readNativeCleanupAttempt(t.Context(), scope, string(lease.WorkItemID), data.AttemptID)
			f.service.config.RunDetails = original
			if missingErr != nil || cleanupErr == nil || snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 1 {
				t.Fatalf("known cleanup depended on archive: registration=%v cleanup=%v claims=%+v", missingErr, cleanupErr, snapshot.ClaimState)
			}
			f.service.config.RunDetails = invalidRegistrationArchive{DetailStore: original}
			err := readSnapshot()
			_, cleanupErr = f.service.readNativeCleanupAttempt(t.Context(), scope, string(lease.WorkItemID), data.AttemptID)
			f.service.config.RunDetails = original
			if err != nil || !errors.Is(cleanupErr, artifact.ErrIntegrity) || snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 1 {
				t.Fatalf("corrupt archive errors: registration=%v cleanup=%v", err, cleanupErr)
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET worker_cleanup_required=NULL WHERE organization_id=$1 AND id=$2", scope.Organization, data.AttemptID); err != nil {
				t.Fatal(err)
			}
			f.service.config.RunDetails = invalidRegistrationArchive{DetailStore: original}
			err = readSnapshot()
			f.service.config.RunDetails = original
			if err != nil || snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
				t.Fatalf("unverified unknown cleanup was offered: error=%v claims=%+v", err, snapshot.ClaimState)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				current, err := r.Attempt(t.Context(), data.AttemptID)
				if err == nil && current.WorkerCleanupRequired != nil {
					t.Fatalf("invalid proof became known: %+v", current)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := readSnapshot(); err != nil {
				t.Fatal(err)
			}
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 1 {
				t.Fatalf("verified unknown cleanup was not recovered: %+v", snapshot.ClaimState)
			}
			var cleanup tracker.NativeAttempt
			decodeHubResponse(t, response, &cleanup)
			finished := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "cleanup_finished_" + data.AttemptID}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: data.AttemptID, RunID: data.RunID, Sequence: cleanup.Sequence + 1, LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: cleanup.PolicyID, Identity: cleanup.Identity, Outcome: "cancelled"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], finished), http.StatusOK)
			if err := readSnapshot(); err != nil {
				t.Fatal(err)
			}
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
				t.Fatalf("acknowledged cleanup remained eligible: %+v", snapshot.ClaimState)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				claims, err := r.(runnerstore.ClaimStateReader).RunnerClaimState(t.Context(), snapshot.RunnerID, scope.Project, "", 100, time.Now().UTC())
				if err == nil && len(claims.Candidates) != 0 {
					t.Fatalf("heartbeat reoffered acknowledged cleanup: %+v", claims.Candidates)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			acknowledged := performHubAPIRequest(t, f.service, http.MethodGet, cleanupPath, f.tokens[index], nil)
			requireNativeStatus(t, acknowledged, http.StatusOK)
			var receipt tracker.NativeAttempt
			decodeHubResponse(t, acknowledged, &receipt)
			if receipt.CleanupAcknowledgedAt == nil || receipt.CleanupAcknowledgedAt.IsZero() || receipt.AttemptID != cleanup.AttemptID || receipt.LeaseID != cleanup.LeaseID || receipt.FencingToken != cleanup.FencingToken || receipt.Runtime == nil || receipt.Runtime.WorkerLaunch == nil || *receipt.Runtime.WorkerLaunch != *cleanup.Runtime.WorkerLaunch {
				t.Fatalf("acknowledged cleanup receipt = %+v", receipt)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, cleanupPath, f.tokens[1-index], nil), http.StatusNotFound)
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
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: leasestore.Attempt{ID: "attempt", RunID: "run", Sequence: 2, Status: "cancelled"}, Lease: leasestore.Lease{LeaseID: "lease", FencingToken: 1, ReleasedAt: &released}, Event: eventstore.Event{Type: "run.finished", RecordedAt: released.Add(tc.receipt)}}
			run := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", Sequence: 2, LeaseID: "lease", FencingToken: 1, RunnerID: "runner", Outcome: "cancelled", Identity: &tracker.NativeExecutionIdentity{Role: tc.role}, Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{PID: tc.pid, ProcessStart: released.Add(-time.Hour)}}}
			if tc.acknowledged {
				at := released.Add(time.Microsecond)
				snapshot.Attempt.CleanupAcknowledgedAt = &at
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
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: leasestore.Attempt{ID: "attempt", RunID: "run", Sequence: 2 + tc.gap, Status: "cancelled", UpdatedAt: released.Add(tc.delta)}, Lease: leasestore.Lease{LeaseID: "lease", FencingToken: 1, ReleasedAt: &released}}
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
			snapshot.Attempt.LeaseID, snapshot.Attempt.FencingToken = "lease", 1
			_, _, err = readTenantRunCleanupData(t.Context(), content, &tenantRunCleanupPredecessor{attempt: snapshot.Attempt, lease: snapshot.Lease, event: event})
			if errors.Is(err, artifact.ErrIntegrity) != tc.invalid {
				t.Fatalf("cleanup archive error = %v, invalid=%t", err, tc.invalid)
			}
		})
	}
}
