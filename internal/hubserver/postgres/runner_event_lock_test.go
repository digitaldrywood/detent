package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
