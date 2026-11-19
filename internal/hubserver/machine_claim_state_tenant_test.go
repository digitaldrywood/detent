package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
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
	t.Parallel()
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

func TestRegistrationCleanupRejectsStalePayload(t *testing.T) {
	t.Parallel()
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

func newRunnerPGTenantFixtures(t *testing.T) []runnerPGFixture {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	seedHostedStartup(t, cfg)
	second := *cfg.Hosted
	second.OrganizationID, second.WorkOSOrganizationID = "org_second", "provider_org_second"
	secondConfig := cfg
	secondConfig.Hosted = &second
	seedHostedStartup(t, secondConfig)
	var fixtures []runnerPGFixture
	for _, config := range []Config{cfg, secondConfig} {
		service, err := Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
		fixtures = append(fixtures, seedRunnerPGFixture(t, admin, service, tracker.OrganizationID(config.Hosted.OrganizationID)))
	}
	return fixtures
}

type releasedCleanupShape struct {
	advanced     bool
	stalePayload bool
	payloadRunID string
	archived     bool
	corrupt      *memoryRunDetails
}

func prepareReleasedCleanup(t *testing.T, f runnerPGFixture, index int, shape releasedCleanupShape) (string, tracker.NativeRunEvent) {
	t.Helper()
	lease, data := f.leases[index], f.data[index]
	path := "/api/v2/organizations/" + string(f.organization) + "/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
	now := time.Now().UTC()
	data.Sequence = 2
	data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", WorkerLaunch: &tracker.NativeWorkerLaunch{PID: 123, ProcessStart: now, ExecutionVersion: "example", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), ControlVersion: "example", PolicyID: f.policy.PolicyID, Protocol: 1, SelectedAt: now}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "observe_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}), http.StatusOK)
	finished := data
	finished.Sequence, finished.Outcome = 3, "cancelled"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "finish_" + data.AttemptID}, Type: "run.finished", SchemaVersion: 1, Data: finished}), http.StatusOK)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, "example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sequence := int64(3)
	if shape.advanced {
		sequence = 4
		var payloadKey string
		if err := f.admin.QueryRowContext(t.Context(), "SELECT payload_object_key FROM native_attempts WHERE organization_id=$1 AND id=$2", f.organization, data.AttemptID).Scan(&payloadKey); err != nil {
			t.Fatal(err)
		}
		if !shape.stalePayload {
			terminated := finished
			terminated.Sequence = sequence
			terminated.MachineID, terminated.SessionID, terminated.RunnerID = lease.MachineID, lease.SessionID, f.bindings[index].RunnerID
			if shape.payloadRunID != "" {
				terminated.RunID = shape.payloadRunID
			}
			raw, err := json.Marshal(terminated)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: f.organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			payloadKey = ref.Key
		}
		if _, err := f.admin.ExecContext(t.Context(), "UPDATE native_attempts SET sequence=$3,payload_object_key=$4 WHERE organization_id=$1 AND id=$2", f.organization, data.AttemptID, sequence, payloadKey); err != nil {
			t.Fatal(err)
		}
		if store := shape.corrupt; store != nil {
			store.mu.Lock()
			corrupted := bytes.Clone(store.data[payloadKey])
			corrupted[0] = 'x'
			store.data[payloadKey] = corrupted
			store.mu.Unlock()
		}
	}
	if shape.archived {
		if _, err := f.admin.ExecContext(t.Context(), "UPDATE issues SET archived=true WHERE organization_id=$1 AND native_id=$2", f.organization, lease.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}
	termination := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":terminated:" + strconv.FormatInt(sequence+1, 10)}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: data.AttemptID, RunID: data.RunID, Sequence: sequence + 1, LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: f.policy.PolicyID, Identity: data.Identity, Outcome: "cancelled"}}
	return path, termination
}

func releasedCleanupAttempt(t *testing.T, f runnerPGFixture, id string) leasestore.Attempt {
	t.Helper()
	var attempt leasestore.Attempt
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		attempt, err = r.Attempt(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func TestReleasedCleanupAcknowledgesPreUpgradeAttempts(t *testing.T) {
	t.Parallel()
	fixtures := newRunnerPGTenantFixtures(t)
	for _, f := range fixtures {
		for index, tc := range []struct {
			name  string
			shape releasedCleanupShape
		}{{"terminal history behind attempt", releasedCleanupShape{advanced: true}}, {"archived work item", releasedCleanupShape{archived: true}}} {
			t.Run(string(f.organization)+"/"+tc.name, func(t *testing.T) {
				path, termination := prepareReleasedCleanup(t, f, index, tc.shape)
				credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.organization, apikey.HashToken(f.tokens[index]), "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := hubscope.NativeScope{Organization: f.organization, Project: "prj_runner", Credential: credential}
				snapshot := runnerauth.RoutingSnapshot{RunnerID: f.bindings[index].RunnerID}
				for range 2 {
					if err := f.service.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err != nil {
						t.Fatal(err)
					}
				}
				if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 1 || snapshot.ClaimState.ReleasedExecutions[0].AttemptID != termination.Data.AttemptID {
					t.Fatalf("%s runner %d cleanup offer = %+v", f.organization, index, snapshot.ClaimState)
				}
				cleanupPath := "/api/v2/organizations/" + string(f.organization) + "/projects/prj_runner/work-items/" + string(f.leases[index].WorkItemID) + "/attempts/" + termination.Data.AttemptID + "?view=cleanup"
				view := performHubAPIRequest(t, f.service, http.MethodGet, cleanupPath, f.tokens[index], nil)
				if view.Code != http.StatusOK {
					t.Fatalf("%s runner %d cleanup view status = %d: %s", f.organization, index, view.Code, view.Body.String())
				}
				var cleanup tracker.NativeAttempt
				decodeHubResponse(t, view, &cleanup)
				if cleanup.Sequence+1 != termination.Data.Sequence || cleanup.PolicyID != termination.Data.PolicyID || *cleanup.Identity != *termination.Data.Identity {
					t.Fatalf("%s runner %d cleanup view = %+v", f.organization, index, cleanup.NativeRunData)
				}
				other := fixtures[0]
				if other.organization == f.organization {
					other = fixtures[1]
				}
				if response := performHubAPIRequest(t, f.service, http.MethodPost, path, other.tokens[index], termination); response.Code == http.StatusOK {
					t.Fatalf("%s accepted another tenant's termination", f.organization)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], termination), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], termination), http.StatusOK)
				attempt := releasedCleanupAttempt(t, f, termination.Data.AttemptID)
				if attempt.CleanupAcknowledgedAt == nil || attempt.Sequence != termination.Data.Sequence || attempt.Status != "cancelled" {
					t.Fatalf("%s runner %d acknowledged attempt = %+v", f.organization, index, attempt)
				}
				if err := f.service.readRegistrationClaimStateTenant(t.Context(), scope, &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
					t.Fatalf("%s runner %d acknowledged cleanup is still offered: %+v", f.organization, index, snapshot.ClaimState)
				}
			})
		}
	}
}

func TestReleasedCleanupRejectsUnprovenTermination(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		shape  releasedCleanupShape
		modify func(*tracker.NativeRunEvent)
		want   int
	}{
		{name: "different fencing token", shape: releasedCleanupShape{advanced: true}, modify: func(e *tracker.NativeRunEvent) { e.Data.FencingToken++ }, want: http.StatusConflict},
		{name: "different policy", shape: releasedCleanupShape{advanced: true}, modify: func(e *tracker.NativeRunEvent) { e.Data.PolicyID = "policy_" + strings.Repeat("c", 64) }, want: http.StatusConflict},
		{name: "payload from another run", shape: releasedCleanupShape{advanced: true, payloadRunID: "run_other"}, want: http.StatusInternalServerError},
		{name: "stale terminal payload", shape: releasedCleanupShape{advanced: true, stalePayload: true}, want: http.StatusInternalServerError},
		{name: "corrupted stored payload", shape: releasedCleanupShape{advanced: true, corrupt: &memoryRunDetails{}}, want: http.StatusServiceUnavailable},
		{name: "archived item with different fencing token", shape: releasedCleanupShape{archived: true}, modify: func(e *tracker.NativeRunEvent) { e.Data.FencingToken++ }, want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			details := tc.shape.corrupt
			f := newRunnerPGFixtureWithDetails(t, func(store attachment.DetailStore) attachment.DetailStore {
				if details != nil {
					return details
				}
				return store
			})
			path, termination := prepareReleasedCleanup(t, f, 0, tc.shape)
			before := releasedCleanupAttempt(t, f, termination.Data.AttemptID)
			if tc.modify != nil {
				tc.modify(&termination)
			}
			if response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[0], termination); response.Code == http.StatusOK {
				t.Fatalf("unproven termination was accepted: %s", response.Body.String())
			} else if response.Code != tc.want {
				t.Fatalf("rejection status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			after := releasedCleanupAttempt(t, f, termination.Data.AttemptID)
			if after.CleanupAcknowledgedAt != nil || after.Sequence != before.Sequence || after.PayloadObjectKey != before.PayloadObjectKey {
				t.Fatalf("rejected termination changed the attempt: before=%+v after=%+v", before, after)
			}
		})
	}
}
