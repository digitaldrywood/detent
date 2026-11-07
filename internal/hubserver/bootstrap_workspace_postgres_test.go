package hubserver

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresBootstrapWorkspaceCostAndTenantIsolation(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
		cfg.Workspace = &WorkspaceConfig{Enabled: true, Terminal: WorkspaceTerminalConfig{Enabled: true}}
	})
	for _, org := range []string{"org_startup", "org_other"} {
		service := f.services[org]
		config, err := service.config.Postgres.Connection.PoolConfig()
		if err != nil {
			t.Fatal(err)
		}
		pgtransport.Configure(config)
		config.MaxConns = int32(service.config.Postgres.Pool.MaxConnections)
		trace := &entryAuthBudgetTrace{}
		config.ConnConfig.Tracer = trace
		var exchanges atomic.Int64
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &entryAuthBudgetWireConn{nativeLeaseWireConn: &nativeLeaseWireConn{Conn: conn, exchanges: &atomic.Int64{}, startup: true}, total: &exchanges}, nil
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		measured, err := postgres.NewRepository(t.Context(), pool, service.config.Postgres.Pool)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := measured.Close(); err != nil {
				t.Error(err)
			}
		})
		originalAccess, originalTenant := service.config.AccessRepository, service.config.TenantRepository
		service.config.AccessRepository, service.config.TenantRepository = measured, measured
		organization := tracker.OrganizationID(org)
		now := time.Now().UTC().Truncate(time.Microsecond)
		if err := measured.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_shared", Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: "project_shared", CanWrite: true, ManageRunner: true})
		}); err != nil {
			t.Fatal(err)
		}
		seeded := 0
		var baseline [2]int64
		for _, size := range []int{2, 50} {
			for seeded < size {
				id := fmt.Sprintf("runner_%d", seeded)
				if err := measured.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: id, Hostname: "example", Capacity: 1}); err != nil {
						return err
					}
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: apikey.HashToken(org + id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: new(now.Add(time.Hour)), ProjectAccess: "selected", Kind: "service"}); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: "project_shared"}); err != nil {
						return err
					}
					enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken("enrollment_" + org + id), Scope: "projects", Operations: []string{"claim"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "example"}
					if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
						return err
					}
					if err := w.RedeemRunnerEnrollment(t.Context(), id, id, tracker.MachineID(id), now); err != nil {
						return err
					}
					if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: id, Scope: "projects", Operations: []string{"claim"}, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
						return err
					}
					return w.ReplaceRunnerWorkspaceReport(t.Context(), id, workspacesession.Capabilities{Files: true, Exec: true, Terminal: org == "org_startup"}, "container")
				}); err != nil {
					t.Fatal(err)
				}
				seeded++
			}
			trace.statements.Store(0)
			exchanges.Store(0)
			ctx := context.WithValue(t.Context(), entryAuthBudgetContextKey{}, true)
			payload, err := service.readPostgresAppBootstrap(ctx, apiCredential{Hosted: &auth.HostedIdentity{OrganizationID: "provider_" + org, Subject: org + "_owner"}, HostedRole: "owner"}, auth.Session{})
			if err != nil {
				t.Fatal(err)
			}
			counts := [2]int64{trace.statements.Load(), exchanges.Load()}
			if !payload.Feature.Workspaces || payload.Plan == nil || len(payload.Projects) != 1 || !payload.Projects[0].Capabilities.Files || payload.Projects[0].Capabilities.Terminal != (org == "org_startup") {
				t.Fatalf("tenant %s bootstrap capabilities or plan incorrect: %+v", org, payload)
			}
			if size == 2 {
				baseline = counts
			} else if counts != baseline {
				t.Fatalf("bootstrap cost grew: 2x=%v 50x=%v", baseline, counts)
			}
			if counts[0] == 0 || counts[1] == 0 {
				t.Fatal("bootstrap trace is empty")
			}
			t.Logf("tenant=%s runners=%d statements=%d exchanges=%d", org, size, counts[0], counts[1])
		}
		service.config.AccessRepository, service.config.TenantRepository = originalAccess, originalTenant
		if err := measured.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
