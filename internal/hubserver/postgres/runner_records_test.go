package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerIdentityEnrollmentRevocationIsTenantScoped(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	now := historyAt()
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, organization := range organizations {
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_shared", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: organization, ID: 1, ProjectID: "project_shared", NativeID: "wi_shared", Number: 1, Title: "Example"}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: "runner_shared", Name: "runner_shared", Hash: apikey.HashToken("credential_" + string(organization)), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: timePointer(now.Add(24 * time.Hour)), ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: "runner_shared", ProjectID: "project_shared"}); err != nil {
				return err
			}
			return w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_shared", Hostname: "example", DisplayName: "Example", Capacity: 1, Version: "example"})
		}); err != nil {
			var postgresError *pq.Error
			if errors.As(err, &postgresError) {
				t.Fatalf("enroll runner: SQLSTATE %s: %s", postgresError.Code, postgresError.Message)
			}
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_shared", RunnerID: "runner_shared", MachineID: "machine_shared", TokenHash: apikey.HashToken("enrollment-token-" + string(organization)), Scope: "projects", Operations: []string{"read", "claim"}, ProjectIDs: []tracker.ProjectID{"project_shared"}, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), CreatedBy: "admin"})
		}); err != nil {
			var postgresError *pq.Error
			if errors.As(err, &postgresError) {
				t.Fatalf("create redeemed identity: SQLSTATE %s: %s", postgresError.Code, postgresError.Message)
			}
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			enrollment, err := w.RunnerEnrollmentByTokenHash(ctx, apikey.HashToken("enrollment-token-"+string(organization)))
			if err != nil {
				return fmt.Errorf("read enrollment: %w", err)
			}
			if enrollment.ID != "enrollment_shared" || len(enrollment.ProjectIDs) != 1 || enrollment.ProjectIDs[0] != "project_shared" {
				t.Fatalf("enrollment=%+v", enrollment)
			}
			if err := w.RedeemRunnerEnrollment(ctx, enrollment.ID, "runner_shared", "machine_shared", now); err != nil {
				return fmt.Errorf("redeem enrollment: %w", err)
			}
			if err := w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: organization, RunnerID: "runner_shared", MachineID: "machine_shared", TokenID: "runner_shared", EnrollmentID: enrollment.ID, Scope: enrollment.Scope, Operations: enrollment.Operations, DisplayName: "Example", CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "amd64", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), LastHeartbeatAt: now}); err != nil {
				return fmt.Errorf("create runner identity: %w", err)
			}
			if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: organization, RunnerID: "runner_shared", ActorID: "admin", Kind: "enrolled", OccurredAt: now}); err != nil {
				return fmt.Errorf("record identity event: %w", err)
			}
			return nil
		}); err != nil {
			var postgresError *pq.Error
			if errors.As(err, &postgresError) {
				t.Fatalf("redeem runner: SQLSTATE %s: %s", postgresError.Code, postgresError.Message)
			}
			t.Fatal(err)
		}
	}

	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: "org_first", RunnerID: "runner_shared", MachineID: "machine_shared", TokenID: "runner_shared", EnrollmentID: "enrollment_shared", Scope: "projects", Operations: []string{"read"}, DisplayName: "duplicate", CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now})
	}); !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("duplicate runner identity error=%v", err)
	}

	for _, organization := range organizations {
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			identity, err := r.RunnerIdentityByCredential(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if identity.OrganizationID != organization || identity.RunnerID != "runner_shared" {
				t.Fatalf("identity escaped organization: %+v", identity)
			}
			projects, err := r.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
			if err != nil {
				return err
			}
			if len(projects) != 1 || projects[0] != "project_shared" {
				t.Fatalf("projects=%v", projects)
			}
			allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, "project_shared", now.Add(time.Minute))
			if err != nil || !allowed {
				t.Fatalf("authorized=%t error=%v", allowed, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.RevokeAccessCredential(ctx, "runner_shared", now.Add(time.Minute)); err != nil {
			return err
		}
		return w.RemoveRunnerIdentity(ctx, "runner_shared", now.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		organization tracker.OrganizationID
		want         bool
	}{{"org_first", false}, {"org_second", true}} {
		if err := repository.ReadTenant(ctx, check.organization, corestore.Serving, func(r tenantstore.Reader) error {
			allowed, err := r.RunnerProjectAuthorized(ctx, "runner_shared", "project_shared", now.Add(2*time.Minute))
			if err != nil {
				return err
			}
			if allowed != check.want {
				t.Fatalf("organization %s authorization=%t want=%t", check.organization, allowed, check.want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestRunnerProjectObservationIsTenantScopedAndRejectsStaleSnapshots(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	ctx := t.Context()
	now := historyAt()
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_shared", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: "runner_shared", Name: "runner_shared", Hash: apikey.HashToken("credential_" + string(organization)), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: timePointer(now.Add(time.Hour)), ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_shared", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_shared", RunnerID: "runner_shared", MachineID: "machine_shared", TokenHash: apikey.HashToken("enrollment_" + string(organization)), Scope: "projects", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: organization, RunnerID: "runner_shared", MachineID: "machine_shared", TokenID: "runner_shared", EnrollmentID: "enrollment_shared", Scope: "projects", Operations: []string{"read"}, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			identity, err := w.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			identity.State = "draining"
			identity.Tags = []string{"linux", "example"}
			identity.MachineUse = "dedicated"
			identity.IsolationTier = "native-trusted"
			identity.HostServices = []string{"git"}
			identity.Availability = runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": 2}, HardDeadline: "30m"}
			identity.ProjectCapacityLimits = map[tracker.ProjectID]int{"project_shared": 3}
			identity.ProjectRanks = map[tracker.ProjectID]int{"project_shared": 7}
			if err := w.UpdateRunnerRouting(ctx, identity.Revision, identity); err != nil {
				return err
			}
			localAttempt := int64(5)
			if organization == "org_second" {
				localAttempt = 6
			}
			observedAt, diagnosticsAt, runtimeAt := now.Add(2*time.Minute), now.Add(time.Minute), now.Add(90*time.Second)
			return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: organization, RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: observedAt, DiagnosticsAt: &diagnosticsAt, RuntimeObservedAt: &runtimeAt, SnapshotDigest: strings.Repeat("a", 64)}, []runnerstore.ProjectDiagnosticAttempt{{Position: 0, RecordKey: fmt.Sprintf("local:%d", localAttempt), IssueID: "wi_shared", LocalAttemptID: localAttempt, NativeAttemptID: "attempt_shared", Generation: 3, Fence: 4, Stage: "implementing", Running: true, NativeClaim: true, HostOperation: "workspace.finalize_native_work", OperationPending: true, LeaseRenewedAt: &runtimeAt}})
		}); err != nil {
			t.Fatalf("seed observation for %s: %v", organization, err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_race", Name: "Race", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
			return err
		}); err != nil {
			t.Fatalf("seed race project for %s: %v", organization, err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			observation, attempts, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_shared", "wi_shared")
			if err != nil {
				return err
			}
			wantLocalAttempt := int64(5)
			if organization == "org_second" {
				wantLocalAttempt = 6
			}
			if observation.OrganizationID != organization || observation.SnapshotDigest != strings.Repeat("a", 64) || observation.RuntimeObservedAt == nil || !observation.RuntimeObservedAt.Equal(now.Add(90*time.Second)) || len(attempts) != 1 || attempts[0].LocalAttemptID != wantLocalAttempt || attempts[0].NativeAttemptID != "attempt_shared" || attempts[0].Fence != 4 || !attempts[0].Running || !attempts[0].OperationPending {
				t.Fatalf("organization %s observation=%+v attempts=%+v", organization, observation, attempts)
			}
			identity, err := r.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if identity.State != "draining" || identity.Revision != 2 || len(identity.Tags) != 2 || identity.MachineUse != "dedicated" || identity.IsolationTier != "native-trusted" || len(identity.HostServices) != 1 || identity.Availability.Timezone != "UTC" || len(identity.Availability.Windows) != 1 || identity.Availability.WindowSlots[identity.Availability.Windows[0]] != 2 || identity.ProjectCapacityLimits["project_shared"] != 3 || identity.ProjectRanks["project_shared"] != 7 {
				t.Fatalf("organization %s routing identity=%+v", organization, identity)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(ctx, "runner_shared")
		if err != nil {
			return err
		}
		identity.Revision--
		return w.UpdateRunnerRouting(ctx, identity.Revision, identity)
	}); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale runner routing revision error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(ctx, "runner_shared")
		if err != nil {
			return err
		}
		identity.ProjectRanks = make(map[tracker.ProjectID]int, runnerstore.MaxRunnerProjectRanks+1)
		for i := range runnerstore.MaxRunnerProjectRanks + 1 {
			identity.ProjectRanks[tracker.ProjectID(fmt.Sprintf("project_%03d", i))] = i
		}
		return w.UpdateRunnerRouting(ctx, identity.Revision, identity)
	}); !errors.Is(err, runnerstore.ErrInvalidRecord) {
		t.Fatalf("oversized runner project ranks error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(time.Minute), SnapshotDigest: strings.Repeat("b", 64)}, nil)
	}); !errors.Is(err, runnerstore.ErrStaleObservation) {
		t.Fatalf("stale project observation error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		staleDiagnosticsAt := now
		return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(3 * time.Minute), DiagnosticsAt: &staleDiagnosticsAt, SnapshotDigest: strings.Repeat("b", 64)}, nil)
	}); err != nil {
		t.Fatalf("later receipt with stale diagnostics error=%v", err)
	}
	if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		newerDiagnosticsAt, staleRuntimeAt := now.Add(2*time.Minute), now.Add(time.Minute)
		return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(4 * time.Minute), DiagnosticsAt: &newerDiagnosticsAt, RuntimeObservedAt: &staleRuntimeAt, SnapshotDigest: strings.Repeat("c", 64)}, nil)
	}); err != nil {
		t.Fatalf("later diagnostics with stale runtime error=%v", err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		observation, attempts, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_shared", "wi_shared")
		if err != nil {
			return err
		}
		if !observation.ObservedAt.Equal(now.Add(2*time.Minute)) || observation.DiagnosticsAt == nil || !observation.DiagnosticsAt.Equal(now.Add(time.Minute)) || observation.RuntimeObservedAt == nil || !observation.RuntimeObservedAt.Equal(now.Add(90*time.Second)) || observation.SnapshotDigest != strings.Repeat("a", 64) || len(attempts) != 1 || attempts[0].LocalAttemptID != 5 {
			t.Fatalf("stale write replaced current project observation: %+v attempts=%+v", observation, attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.ExecContext(ctx, `CREATE FUNCTION p12_runner_observation_write_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(EXTRACT(EPOCH FROM NEW.observed_at)::bigint); RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE TRIGGER p12_runner_observation_write_barrier BEFORE INSERT ON public.runner_project_observations FOR EACH ROW EXECUTE FUNCTION p12_runner_observation_write_barrier()`); err != nil {
		t.Fatal(err)
	}
	admin.SetMaxOpenConns(2)
	lockConnection, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockConnection.Close() })
	olderAt, newerAt := now.Add(4*time.Minute), now.Add(5*time.Minute)
	for _, at := range []time.Time{olderAt, newerAt} {
		if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, at.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	diagnosticsAt := now.Add(3 * time.Minute)
	writeObservation := func(at time.Time) func(context.Context) error {
		return func(writeCtx context.Context) error {
			return repository.WriteTenant(writeCtx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerProjectObservation(writeCtx, runnerstore.ProjectObservation{OrganizationID: "org_first", RunnerID: "runner_shared", ProjectID: "project_race", ObservedAt: at, DiagnosticsAt: &diagnosticsAt, SnapshotDigest: strings.Repeat("c", 64)}, nil)
			})
		}
	}
	olderTask := startRepositoryTask(t, writeObservation(olderAt))
	newerTask := startRepositoryTask(t, writeObservation(newerAt))
	testenv.WaitUntil(t, ctx, func() error {
		var waiting int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO runner_project_observations%'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting != 2 {
			return fmt.Errorf("observation insert waiters = %d, want 2", waiting)
		}
		return nil
	})
	if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, newerAt.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := newerTask.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, olderAt.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := olderTask.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		observation, _, err := r.RunnerProjectObservation(ctx, "runner_shared", "project_race", "")
		if err != nil {
			return err
		}
		if !observation.ObservedAt.Equal(newerAt) || observation.SnapshotDigest != strings.Repeat("c", 64) {
			t.Fatalf("concurrent first-insert result = %+v, want newer observation", observation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
