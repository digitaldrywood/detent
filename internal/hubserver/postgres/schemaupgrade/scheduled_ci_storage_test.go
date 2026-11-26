package schemaupgrade_test

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
)

func TestScheduledCIStorageUpgradeChargesRetainedDecisionsOnce(t *testing.T) {
	t.Parallel()
	db, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261120205733)
	pgstoretest.SeedCore(t, db)
	tenants := []struct {
		org           string
		decisions     int
		before, after int64
	}{
		{"org_first", 129, 123, 14700},
		{"org_second", 2, 456, 684},
	}
	for _, tenant := range tenants {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value)
 VALUES($1,'collaboration_bytes',$2)`, tenant.org, tenant.before); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO scheduled_ci_decisions
 (organization_id,project_id,repository,head_sha,run_id,run_attempt,job_id,outcome,target,reason,created_at)
 SELECT $1,'prj_shared','owner/repository',repeat('a',40),'1','1',lpad(n::text,3,'0'),'source','TestExample','résumé',CURRENT_TIMESTAMP
 FROM generate_series(1,$2::integer) n`, tenant.org, tenant.decisions); err != nil {
			t.Fatal(err)
		}
	}
	store := roleConnection(t, fixture, db, postgres.MigrationRoleName)
	migrations := hubMigrationFiles(t)
	if err := postgres.Migrate(t.Context(), store, migrations); err != nil {
		t.Fatal(err)
	}
	assertCounters := func() {
		t.Helper()
		for _, tenant := range tenants {
			var count int
			var used, project, other int64
			err := db.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM scheduled_ci_decisions WHERE organization_id=$1), value,
 COALESCE((SELECT collaboration_bytes FROM scheduled_ci_project_storage WHERE organization_id=$1 AND project_id='prj_shared'),0),
 COALESCE((SELECT collaboration_bytes FROM scheduled_ci_project_storage WHERE organization_id=$1 AND project_id='prj_other'),0)
 FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes'`, tenant.org).Scan(&count, &used, &project, &other)
			wantProject := tenant.after - tenant.before
			if err != nil || count != tenant.decisions || used != tenant.after || project != wantProject || other != 0 {
				t.Errorf("%s decisions=%d tenant bytes=%d project bytes=%d other project bytes=%d, want %d, %d, %d, 0: %v", tenant.org, count, used, project, other, tenant.decisions, tenant.after, wantProject, err)
			}
		}
	}
	assertCounters()
	if t.Failed() {
		return
	}
	backfills := fstest.MapFS{}
	for _, pattern := range []string{"*_scheduled_ci_decision_storage.sql", "*_backfill_scheduled_ci_project_storage.sql"} {
		names, err := fs.Glob(migrations, pattern)
		if err != nil || len(names) != 1 {
			t.Fatalf("backfill migration=%v error=%v", names, err)
		}
		backfills[names[0]] = migrations[names[0]]
	}
	pool, err := store.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	migrationDB := stdlib.OpenDB(*pool.ConnConfig)
	t.Cleanup(func() {
		if err := migrationDB.Close(); err != nil {
			t.Error(err)
		}
	})
	replay, err := goose.NewProvider(goose.DialectPostgres, migrationDB, postgres.RoleMigrations(backfills),
		goose.WithDisableVersioning(true), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := replay.Up(t.Context()); err != nil || len(applied) != 2 {
		t.Fatalf("replay applied %d migrations: %v", len(applied), err)
	}
	assertCounters()
}
