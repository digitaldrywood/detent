package hubserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type genericRunnerEventRepository struct{ tenantstore.Repository }

func (r genericRunnerEventRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, _ corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, corestore.Background, write)
}

func TestGenericRunnerEventCommandDoesNotDeadlockMachineHeartbeat(t *testing.T) {
	first, second, repository, _, _ := newRunnerEventWireFixtures(t)
	service := &Service{config: Config{TenantRepository: genericRunnerEventRepository{repository}, NativeRunEvents: &nativeRunEventParticipantFixture{}}}
	for index, fixture := range []runnerPGFixture{first, second} {
		org := []tracker.OrganizationID{"org_startup", "org_second"}[index]
		t.Run(string(org), func(t *testing.T) {
			ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
			locked, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 2)
			var workers sync.WaitGroup
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); cancel(); workers.Wait() })
			now := time.Now().UTC()
			binding := fixture.bindings[0]
			workers.Go(func() {
				done <- repository.WriteRunnerHeartbeat(ctx, org, "credential_1", binding.RunnerID, func(ctx context.Context, w tenantstore.Writer) error {
					if err := w.HeartbeatMachine(ctx, leasestore.Machine{OrganizationID: org, ID: string(binding.MachineID), DisplayName: "updated", Capacity: 1, Version: "test"}, now); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: org, RunnerID: binding.RunnerID, ProjectID: "prj_runner", ObservedAt: now, SnapshotDigest: strings.Repeat("a", 64)}, nil)
				})
			})
			select {
			case <-locked:
			case err := <-done:
				t.Fatalf("heartbeat ended before locking the machine: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var heartbeatPID int
			if err := fixture.admin.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction' AND query LIKE 'UPDATE machines SET%'`).Scan(&heartbeatPID); err != nil {
				t.Fatal(err)
			}
			data := fixture.data[0]
			command := eventstore.RunCommand{Project: "prj_runner", WorkItem: fixture.leases[0].WorkItemID, LeaseID: string(data.LeaseID), Operation: "append-event"}
			scope := nativeScope{organization: org, project: command.Project, credential: apiCredential{ID: "credential_1", Runner: runnerauth.Identity{Binding: binding}}}
			workers.Go(func() {
				done <- service.writeNativeRunCommand(ctx, scope, command, func(ctx context.Context, w tenantstore.Writer) error {
					if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
						return err
					}
					if _, err := w.LockWorkItem(ctx, scope.project, command.WorkItem); err != nil {
						return err
					}
					_, err := w.RunnerForLease(ctx, command.LeaseID)
					return err
				})
			})
			testenv.WaitUntil(t, ctx, func() error {
				select {
				case err := <-done:
					t.Fatalf("command ended before lock observation: %v", err)
				default:
				}
				var waiting bool
				if err := fixture.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, heartbeatPID).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("event has not reached the heartbeat runner lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			for range 2 {
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("concurrent runner command (SQLSTATE=%s): %v", pgtransport.SQLState(err), err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		})
	}
}
