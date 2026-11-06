package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerClaimSharesLockedHeartbeatAuthority(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	counter := &reportCountingTracer{}
	counted, err := postgres.NewRepository(t.Context(), tracedFeaturePool(t, pool, counter), smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	command, ok := any(counted).(interface {
		WriteRunnerCommand(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	})
	if !ok {
		t.Fatal("claim has no shared runner command authority")
	}
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return seedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		seedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", historyAt(), true)
		capacity := index + 2
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.UpdateRunnerCapacityConfig(t.Context(), "runner_shared", runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, ObservedAt: historyAt()})
		}); err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{2, 50} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) SELECT $1,'project_'||i,'Example '||i,'native' FROM generate_series(1,$2) i ON CONFLICT (organization_id,id) DO NOTHING`, org, size); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO runner_project_capacity_limits(organization_id,runner_id,project_id,capacity_limit) VALUES($1,'runner_shared','project_1',$2) ON CONFLICT DO NOTHING`, org, capacity); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO runner_availability_windows(organization_id,runner_id,position,window_spec) VALUES($1,'runner_shared',0,'Mon-Sun 00:00-24:00') ON CONFLICT DO NOTHING`, org); err != nil {
				t.Fatal(err)
			}
			counter.count.Store(0)
			err := command.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				before := counter.count.Load()
				identity, err := w.RunnerIdentityByCredential(ctx, "runner_shared")
				if err != nil {
					return err
				}
				again, err := w.RunnerIdentity(ctx, identity.RunnerID)
				if err != nil {
					return err
				}
				projects, err := w.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
				if err != nil {
					return err
				}
				if _, err := w.AccessCredential(ctx, identity.TokenID); err != nil {
					return err
				}
				if identity.OrganizationID != org || again.CapacityConfig == nil || again.CapacityConfig.RuntimeLimit != capacity || identity.ProjectCapacityLimits["project_1"] != capacity || len(identity.Availability.Windows) != 1 || len(projects) != size {
					return fmt.Errorf("incomplete authority for tenant %s: org=%s config=%+v limit=%d windows=%d projects=%d expected=%d", org, identity.OrganizationID, again.CapacityConfig, identity.ProjectCapacityLimits["project_1"], len(identity.Availability.Windows), len(projects), size)
				}
				if got := counter.count.Load() - before; got != 0 {
					t.Errorf("authority reload statements=%d, want 0", got)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := counter.count.Load(); got != 4 {
				t.Errorf("tenant=%s projects=%d statements excluding control=%d, want 4", org, size, got)
			}
		}
	}
}
