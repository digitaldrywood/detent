package hubmaintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var maintenanceTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE tenants(id text PRIMARY KEY,generation bigint NOT NULL);
 CREATE TABLE maintenance_items(organization_id text,id int,visits int NOT NULL DEFAULT 0,PRIMARY KEY(organization_id,id));
 ALTER TABLE maintenance_items ENABLE ROW LEVEL SECURITY;
 ALTER TABLE maintenance_items FORCE ROW LEVEL SECURITY;
 CREATE POLICY tenant ON maintenance_items USING(organization_id=current_setting('detent.organization_id',true));
 GRANT SELECT,UPDATE ON maintenance_items TO PUBLIC;
 INSERT INTO tenants VALUES('org_first',1),('org_second',2);
 INSERT INTO maintenance_items(organization_id,id) VALUES('org_first',1),('org_first',2),('org_second',1),('org_second',2)`)
	return err
})

func TestMain(m *testing.M) {
	testenv.ClearGitEnvironment()
	code := m.Run()
	if err := maintenanceTemplate.Close(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestPostgresVisitsOneItemPerTenant(t *testing.T) {
	admin := maintenanceTemplate.Database(t)
	role := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'fixture-only'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.WithoutCancel(t.Context()), `DROP ROLE `+role); err != nil {
			t.Error(err)
		}
	})
	config := maintenanceTemplate.NativeConfig(t, admin, role, "fixture-only")
	pools, err := pgtransport.Open(t.Context(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	registry := registryFunc(func(ctx context.Context, after string, limit int) ([]Tenant, error) {
		rows, err := admin.QueryContext(ctx, `SELECT id,generation FROM tenants WHERE id>$1 ORDER BY id LIMIT $2`, after, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var tenants []Tenant
		for rows.Next() {
			var tenant Tenant
			if err := rows.Scan(&tenant.Organization, &tenant.Generation); err != nil {
				return nil, err
			}
			tenants = append(tenants, tenant)
		}
		return tenants, rows.Err()
	})
	scheduler, err := New(registry, []Feature{{Name: "board", Bytes: 1024, Step: func(ctx context.Context, tenant Tenant, _ Budget) (Outcome, error) {
		work := &pgx.Batch{}
		work.Queue(`UPDATE maintenance_items SET visits=visits+1 WHERE id=(SELECT id FROM maintenance_items WHERE visits=0 ORDER BY id LIMIT 1) RETURNING organization_id,id`)
		tx, results, err := pools.BeginBatch(ctx, pgtransport.Scope{Organization: tracker.OrganizationID(tenant.Organization)}, pgtransport.Options{}, work)
		if err != nil {
			return Outcome{}, err
		}
		defer func() {
			if err := tx.Rollback(ctx); err != nil {
				t.Error(err)
			}
		}()
		var org string
		var id int
		if err := results.QueryRow().Scan(&org, &id); err != nil {
			return Outcome{}, errors.Join(err, results.Close())
		}
		if org != tenant.Organization || id < 1 || id > 2 {
			return Outcome{}, errors.New("cross-tenant maintenance result")
		}
		if err := results.Close(); err != nil {
			return Outcome{}, err
		}
		return Outcome{Progress: true, Items: 1}, tx.Commit(ctx)
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for tick := range 2 {
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		var touched, maximum int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE visits>0),max(visits) FROM maintenance_items`).Scan(&touched, &maximum); err != nil || touched != 2*(tick+1) || maximum != 1 {
			t.Fatalf("one-item bound failed: touched=%d max=%d err=%v", touched, maximum, err)
		}
	}
}
