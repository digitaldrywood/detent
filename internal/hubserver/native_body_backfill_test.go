package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBodyBackfillDrainsWithinBoundedTicks(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	poolConfig, err := cfg.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(poolConfig)
	poolConfig.MaxConns = 3
	var exchanges atomic.Int64
	dial := poolConfig.ConnConfig.DialFunc
	poolConfig.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &nativeLeaseWireConn{Conn: conn, exchanges: &exchanges, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, cfg.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	service.config.TenantRepository = repository
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	service.config.now = func() time.Time { return now }
	for _, test := range []struct{ size, ticks int }{{2, 48}, {50, 48}, {100, 48}, {1200, 360}} {
		t.Run(fmt.Sprint(test.size), func(t *testing.T) {
			size := test.size
			for _, name := range []string{"first", "second"} {
				org := tracker.OrganizationID(fmt.Sprintf("org_%s_%d", name, size))
				seedStartupOrganization(t, repository, org)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for index := range size {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", Body: string(org), State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
					raw, err := json.Marshal(issue)
					if err != nil {
						t.Fatal(err)
					}
					version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
							return err
						}
						_, err := w.CreateCollaborationVersion(t.Context(), version)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM native_work_item_page_dirty WHERE organization_id=$1`, org); err != nil {
					t.Fatal(err)
				}
				tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
				maxTransactions, maxStatements := 0, 0
				var maxExchanges int64
				for range test.ticks {
					now = now.Add(time.Second)
					before := exchanges.Load()
					transactions, statements := 0, 0
					ctx := postgres.WithTransactionTiming(t.Context(), func(name string, _ time.Time) {
						if name == "pg_commit" {
							transactions++
						}
					}, func(time.Time, string) { statements++ })
					budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
					if err := service.WorkItemPageMaintenanceFeature().Step(budget.Context(ctx, tenant), tenant, budget); err != nil {
						t.Fatal(err)
					}
					wireExchanges := exchanges.Load() - before
					if transactions > 256 || statements > 4096 || wireExchanges > 4096 {
						t.Fatalf("tick exceeded fixed database budget: transactions=%d statements=%d exchanges=%d", transactions, statements, wireExchanges)
					}
					maxTransactions = max(maxTransactions, transactions)
					maxStatements = max(maxStatements, statements)
					maxExchanges = max(maxExchanges, wireExchanges)
				}
				var pages, pending int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_item_pages WHERE organization_id=$1 AND body=$1`, org).Scan(&pages); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_item_page_dirty WHERE organization_id=$1`, org).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if pages != size || pending != 0 {
					t.Errorf("tenant %s after %d ticks: body pages=%d/%d pending=%d", org, test.ticks, pages, size, pending)
				}
				t.Logf("tenant=%s backlog=%d ticks=%d max_transactions=%d max_statements=%d max_exchanges=%d", org, size, test.ticks, maxTransactions, maxStatements, maxExchanges)
			}
		})
	}
}
