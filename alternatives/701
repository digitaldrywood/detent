package platformstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestHealthReservePreservesWorkloadBudgets(t *testing.T) {
	for _, test := range []struct {
		name    string
		pool    PoolConfig
		serving int
	}{
		{name: "minimum", pool: PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}, serving: 1},
		{name: "full serving", pool: PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1}, serving: 2},
		{name: "preserve background", pool: PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 2}, serving: 1},
		{name: "existing spare capacity", pool: PoolConfig{MaxConnections: 6, ServingConnections: 2, BackgroundConnections: 1}, serving: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := template.Database(t)
			var name string
			if err := db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "ALTER DATABASE "+pq.QuoteIdentifier(name)+" CONNECTION LIMIT 8"); err != nil {
				t.Fatal(err)
			}
			config := testConfig(t)
			config.Pool = test.pool
			r := repository(t, db, config)
			seedOrganizations(t, r)
			for _, budget := range []struct {
				count    int
				workload corestore.Workload
			}{{cap(r.serving), corestore.Serving}, {cap(r.background), corestore.Background}} {
				for range budget.count {
					tx, err := r.BeginEntry(t.Context(), budget.workload, true)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := tx.Rollback(); err != nil {
							t.Error(err)
						}
					}()
				}
			}
			if stats := r.pool.Stats(); stats.MaxOpenConnections != test.pool.MaxConnections || stats.InUse != cap(r.serving)+cap(r.background) {
				t.Fatalf("physical pool = %+v", stats)
			}
			stats := r.pool.Stats()
			t.Logf("held all admissions: serving=%d background=%d physical_in_use=%d max=%d", cap(r.serving), cap(r.background), stats.InUse, stats.MaxOpenConnections)
			for _, probe := range []struct {
				name string
				run  func(context.Context) error
			}{
				{name: "Ping", run: r.Ping},
				{name: "SchemaVersion", run: func(ctx context.Context) error {
					version, err := r.SchemaVersion(ctx)
					if err == nil && version <= 0 {
						return fmt.Errorf("schema version = %d", version)
					}
					return err
				}},
			} {
				ctx, cancel := context.WithCancel(t.Context())
				result := make(chan error, 1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					result <- probe.run(ctx)
				}()
				defer func() { cancel(); <-done }()
				var probeErr error
				waiting := false
				testenv.WaitUntil(t, t.Context(), func() error {
					select {
					case probeErr = <-result:
						return nil
					default:
						if r.pool.Stats().WaitCount > stats.WaitCount {
							waiting = true
							return nil
						}
						return fmt.Errorf("%s has neither completed nor entered physical connection wait", probe.name)
					}
				})
				cancel()
				<-done
				if waiting {
					t.Fatalf("%s entered physical connection wait with all actual workload admissions held: %v", probe.name, <-result)
				}
				if probeErr != nil {
					t.Fatalf("%s with all workload admissions held: %v", probe.name, probeErr)
				}
			}
			if cap(r.serving) != test.serving || cap(r.background) != test.pool.BackgroundConnections {
				t.Fatalf("workload budgets = %d/%d", cap(r.serving), cap(r.background))
			}
			tx, err := r.BeginHealthRead(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := tx.Rollback(); err != nil {
					t.Error(err)
				}
			}()
			var count int
			if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM organizations WHERE id IN ('org_first','org_second')").Scan(&count); err != nil || count != 2 {
				t.Fatalf("health registry read = %d, %v", count, err)
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE organizations SET name=name WHERE id='org_first'"); err == nil {
				t.Fatal("health read transaction accepted a write")
			} else {
				var state interface{ SQLState() string }
				if !errors.As(err, &state) || state.SQLState() != "25006" {
					t.Fatalf("health write refusal = %v", err)
				}
			}
		})
	}
}

func TestHealthReserveRejectsUnusableBudgets(t *testing.T) {
	for _, pool := range []PoolConfig{
		{MaxConnections: 2, ServingConnections: 1, BackgroundConnections: 1},
		{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 2},
		{MaxConnections: 4, ServingConnections: 0, BackgroundConnections: 1},
		{MaxConnections: 4, ServingConnections: 1, BackgroundConnections: 0},
		{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 2},
	} {
		t.Run(fmt.Sprintf("%d/%d/%d", pool.MaxConnections, pool.ServingConnections, pool.BackgroundConnections), func(t *testing.T) {
			config := testConfig(t)
			config.Pool = pool
			if _, err := New(t.Context(), &sql.DB{}, config); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid pool = %v", err)
			}
		})
	}
}
