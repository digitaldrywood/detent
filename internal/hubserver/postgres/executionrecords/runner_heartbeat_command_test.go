package executionrecords_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type heartbeatCommand interface {
	WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
}

func TestRunnerHeartbeatCommandTenantScopeAndRollback(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
	}
	command, ok := any(repository).(heartbeatCommand)
	if !ok {
		t.Fatal("heartbeat has no bounded transaction command")
	}
	for _, size := range []int{2, 3} {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) SELECT $1,'project_'||i,'Example '||i,'native' FROM generate_series(1,$2) i ON CONFLICT (organization_id,id) DO NOTHING`, org, size); err != nil {
				t.Fatal(err)
			}
			for _, rollback := range []bool{false, true} {
				sentinel := errors.New("rollback heartbeat")
				err := command.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
					identity, err := w.RunnerIdentityByCredential(ctx, "runner_shared")
					if err != nil {
						return err
					}
					if identity.OrganizationID != org {
						t.Fatalf("cross-tenant identity: %s", identity.OrganizationID)
					}
					if locker, ok := w.(runnerstore.UpdateLocker); ok {
						if err := locker.LockRunnerUpdate(ctx, identity.RunnerID); err != nil {
							return err
						}
					}
					if _, err := w.AccessCredential(ctx, "runner_shared"); err != nil {
						return err
					}
					identity.ReportedCapacity = 7
					if rollback {
						identity.ReportedCapacity = 9
					}
					identity.LastHeartbeatAt = now.Add(time.Minute)
					if err := w.UpdateRunnerIdentity(ctx, identity); err != nil {
						return err
					}
					version := "committed"
					if rollback {
						version = "rolled-back"
					}
					if err := w.HeartbeatMachine(ctx, leasestore.Machine{OrganizationID: org, ID: "machine_shared", Capacity: identity.ReportedCapacity, Version: version}, now.Add(time.Minute)); err != nil {
						return err
					}
					if err := w.ReplaceRunnerBackendIsolation(ctx, "runner_shared", isolation.Report{"example": {isolation.NativeTrusted}}); err != nil {
						return err
					}
					problems, err := w.RunnerProblems(ctx, "runner_shared")
					if err != nil {
						return err
					}
					if len(problems) != 0 {
						t.Fatal(problems)
					}
					if _, err := w.RunnerProjects(ctx, "runner_shared", runnerstore.MaxRunnerProjectList); err != nil {
						return err
					}
					if _, err := w.RunnerRepositoryList(ctx, "runner_shared"); err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					if _, err := w.RunnerUpdateState(ctx, "runner_shared"); err != nil {
						return err
					}
					if _, err := w.RunnerUrgentUpdate(ctx); err != nil {
						return err
					}
					if _, err := w.RunnerHasActiveLease(ctx, "runner_shared", now.Add(time.Minute)); err != nil {
						return err
					}
					if rollback {
						return sentinel
					}
					return nil
				})
				if rollback && !errors.Is(err, sentinel) || !rollback && err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					identity, err := r.RunnerIdentity(t.Context(), "runner_shared")
					if err != nil {
						return err
					}
					machine, err := r.Machine(t.Context(), "machine_shared")
					if err != nil {
						return err
					}
					if identity.ReportedCapacity != 7 || machine.Capacity != 1 || machine.Version != "committed" {
						t.Errorf("rollback changed liveness or configured capacity: runner=%d machine=%d version=%s", identity.ReportedCapacity, machine.Capacity, machine.Version)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestRunnerHeartbeatReturnsCommittedUpdateRevision(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		if err := repository.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			identity, err := w.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			identity.ReportedCapacity = 3
			if err := w.UpdateRunnerIdentity(ctx, identity); err != nil {
				return err
			}
			request := runnerauth.UpdateRequest{ID: "update_example", RequestedAt: now, Service: "detent", Version: "0.184.0", Release: true, FromRelease: true, ExpectedBuildRevision: strings.Repeat("a", 64)}
			if err := w.CreateRunnerUpdateRequest(ctx, runnerstore.UpdateRequestRecord{OrganizationID: org, RunnerID: identity.RunnerID, Request: request}); err != nil {
				return err
			}
			if err := w.SetRunnerUpdateDesired(ctx, identity.RunnerID, identity.Revision, request.ID); err != nil {
				return err
			}
			identity.ReportedCapacity = 4
			if err := w.UpdateRunnerIdentity(ctx, identity); err != nil {
				return err
			}
			current, err := w.RunnerIdentity(ctx, identity.RunnerID)
			if err != nil {
				return err
			}
			if current.Revision != identity.Revision+1 || current.ReportedCapacity != 4 {
				t.Errorf("heartbeat returns stale identity: revision=%d capacity=%d", current.Revision, current.ReportedCapacity)
			}
			state, err := w.RunnerUpdateState(ctx, identity.RunnerID)
			if err != nil {
				return err
			}
			if state.Desired == nil || state.Desired.ID != request.ID {
				t.Error("heartbeat omitted its committed update request")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerHeartbeatCapacityConfigIsVisibleToLaterWritersAndResponse(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		previous := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, ObservedAt: now}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.UpdateRunnerCapacityConfig(t.Context(), "runner_shared", previous)
		}); err != nil {
			t.Fatal(err)
		}
		config := runnerauth.CapacityConfig{Revision: strings.Repeat("b", 64), LocalLimit: 4 + index, ClientLimit: 3 + index, RuntimeLimit: 2 + index, Manageable: true, ObservedAt: now.Add(time.Second + 789*time.Nanosecond)}
		expected := config
		expected.ObservedAt = expected.ObservedAt.UTC().Truncate(time.Microsecond)
		var response runnerstore.Identity
		check := func(stage string, identity runnerstore.Identity) {
			t.Helper()
			if identity.CapacityConfig == nil || *identity.CapacityConfig != expected {
				t.Errorf("%s/%s stale capacity config: got=%+v want=%+v", org, stage, identity.CapacityConfig, expected)
			}
		}
		if err := repository.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			if err := w.UpdateRunnerCapacityConfig(ctx, "runner_shared", config); err != nil {
				return err
			}
			byCredential, err := w.RunnerIdentityByCredential(ctx, "runner_shared")
			if err != nil {
				return err
			}
			check("later writer", byCredential)
			if byCredential.CapacityConfig != nil {
				byCredential.ReportedCapacity = int64(byCredential.CapacityConfig.RuntimeLimit)
			}
			if err := w.UpdateRunnerIdentity(ctx, byCredential); err != nil {
				return err
			}
			invalid := config
			invalid.LocalLimit = 0
			if err := w.UpdateRunnerCapacityConfig(ctx, "runner_shared", invalid); !errors.Is(err, runnerstore.ErrInvalidRecord) {
				t.Errorf("invalid capacity write: %v", err)
			}
			config.RuntimeLimit = 999
			response, err = w.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			check("response snapshot", response)
			if response.ReportedCapacity != int64(expected.RuntimeLimit) {
				t.Errorf("later writer used stale capacity: %d", response.ReportedCapacity)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		check("returned response", response)
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			committed, err := r.RunnerIdentity(t.Context(), "runner_shared")
			if err != nil {
				return err
			}
			check("committed", committed)
			if committed.ReportedCapacity != response.ReportedCapacity {
				t.Error("response differs from committed later write")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerHeartbeatOtherCachedWritesStayCoherent(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		if err := repository.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			problem := runnerauth.Problem{Code: "backend_missing", Message: "Example", FixHint: "Retry", ReportedAt: now, FirstSeen: now}
			if err := w.ReplaceRunnerProblems(ctx, "runner_shared", []runnerauth.Problem{problem}); err != nil {
				return err
			}
			problems, err := w.RunnerProblems(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if len(problems) != 1 || problems[0] != problem {
				t.Errorf("%s stale problem snapshot: %+v", org, problems)
			}
			identity, err := w.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			identity.State = "draining"
			if err := w.UpdateRunnerRouting(ctx, identity.Revision, identity); err != nil {
				return err
			}
			current, err := w.RunnerIdentityByCredential(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if current.State != "draining" || current.Revision != identity.Revision+1 {
				t.Errorf("%s stale routing identity: %+v", org, current)
			}
			at := now.Add(time.Second + 789*time.Nanosecond)
			if err := w.RemoveRunnerIdentity(ctx, "runner_shared", at); err != nil {
				return err
			}
			removed, err := w.RunnerIdentity(ctx, "runner_shared")
			if err != nil {
				return err
			}
			if removed.RemovedAt == nil || !removed.RemovedAt.Equal(at.Truncate(time.Microsecond)) || removed.Revision != identity.Revision+2 {
				t.Errorf("%s stale removed identity: %+v", org, removed)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
