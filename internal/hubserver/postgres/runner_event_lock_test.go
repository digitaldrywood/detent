package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerEventDoesNotDeadlockMachineHeartbeat(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			seedLeaseFamily(t, repository, org)
			now := time.Now().UTC()
			seedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", now, true)
			lease := claimLease(t, repository, org, leaseClaim(1))
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: "runner_shared", LeaseID: lease.LeaseID, AssignedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			event := attemptEvent(lease)
			recordLeaseAttempt(t, repository, org, event)
			event.Sequence, event.PayloadObjectKey = 2, "example/next"
			locked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			heartbeat := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteRunnerHeartbeat(ctx, org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
					if err := w.HeartbeatMachine(ctx, leasestore.Machine{OrganizationID: org, ID: "machine_1", DisplayName: "updated", Capacity: 1, Version: "test"}, now); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: org, RunnerID: "runner_shared", ProjectID: "prj_1", ObservedAt: now, SnapshotDigest: strings.Repeat("a", 64)}, nil)
				})
			})
			defer releaseOnce.Do(func() { close(release) })
			waitRepositorySignal(t, locked)
			var heartbeatPID int
			if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE 'UPDATE machines SET%'`, login).Scan(&heartbeatPID); err != nil {
				t.Fatal(err)
			}
			appendEvent := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					facts := w.(interface {
						WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
					})
					return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, AttemptID: event.ID, Operation: "append-event"}, func(w tenantstore.Writer) error {
						if _, err := w.RunnerForLease(ctx, lease.LeaseID); err != nil {
							return err
						}
						_, _, err := w.RecordAttemptEvent(ctx, event)
						return err
					})
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-appendEvent.done:
					return fmt.Errorf("event ended before lock observation: %v", appendEvent.err)
				default:
				}
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)))`, login, heartbeatPID).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("event has not reached the heartbeat runner lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			if err := heartbeat.wait(t); err != nil {
				t.Errorf("heartbeat (SQLSTATE=%s): %v", pgtransport.SQLState(err), err)
			}
			if err := appendEvent.wait(t); err != nil {
				t.Errorf("append event (SQLSTATE=%s): %v", pgtransport.SQLState(err), err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				observation, _, err := r.RunnerProjectObservation(t.Context(), "runner_shared", "prj_1", "")
				if err != nil {
					return err
				}
				if observation.SnapshotDigest != strings.Repeat("a", 64) {
					return errors.New("heartbeat observation did not commit")
				}
				attempt, err := r.Attempt(t.Context(), event.ID)
				if err == nil && attempt.Sequence != 2 {
					return fmt.Errorf("attempt sequence=%d, want 2", attempt.Sequence)
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRunnerCompletionAcquiresProjectLockBeforePublication(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		seedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", historyAt(), true)
		lease := claimLease(t, repository, org, leaseClaim(1))
		for _, completion := range []bool{false, true} {
			err := repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				facts, ok := w.(interface {
					WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
				})
				if !ok {
					return fmt.Errorf("runner command facts unavailable")
				}
				return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, Completion: completion}, func(tenantstore.Writer) error {
					var project string
					err := admin.QueryRowContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id='prj_1' FOR SHARE NOWAIT`, org).Scan(&project)
					if completion {
						if pgtransport.SQLState(err) != pgtransport.LockTimeout {
							return fmt.Errorf("completion project lock permits concurrent shared lock: %w", err)
						}
					} else if err != nil {
						return err
					}
					return nil
				})
			})
			if err != nil {
				t.Errorf("tenant=%s completion=%t: %v", org, completion, err)
			}
		}
	}
}

func TestRunnerEventQueuedFenceDiagnostics(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		seedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", historyAt(), true)
		lease := claimLease(t, repository, org, leaseClaim(1))
		for _, test := range []struct {
			name                          string
			wrongToken, released, expired bool
		}{
			{name: "wrong token", wrongToken: true},
			{name: "released", released: true},
			{name: "expired", expired: true},
		} {
			if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '3 hours',renewed_at=clock_timestamp()-interval '2 hours',released_at=CASE WHEN $3 THEN clock_timestamp() END,expires_at=clock_timestamp()+CASE WHEN $4 THEN interval '-1 hour' ELSE interval '1 hour' END WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID, test.released, test.expired); err != nil {
				t.Fatal(err)
			}
			err := repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				facts := w.(interface {
					WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
				})
				return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID}, func(w tenantstore.Writer) error {
					token := lease.FencingToken
					if test.wrongToken {
						token++
					}
					_, _, err := w.RecordAttemptEvent(ctx, leasestore.AttemptEvent{ID: "example-attempt", RunID: "example-run", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: token}, Sequence: 1, Status: "running", PayloadObjectKey: "example-object"})
					return err
				})
			})
			var fence *leasestore.FenceError
			if !errors.Is(err, tracker.ErrStaleFencingToken) || !errors.As(err, &fence) || fence.Check != "attempt_locked_identity" {
				t.Errorf("tenant=%s %s diagnostic=%v", org, test.name, err)
			}
		}
	}
}
