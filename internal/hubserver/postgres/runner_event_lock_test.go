package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
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
