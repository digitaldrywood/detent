package schemaupgrade_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptWorkerIdentityMigration(t *testing.T) {
	t.Parallel()
	db := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, db, pgstoretest.SmallPool())
	const org = tracker.OrganizationID("org_first")
	tenants := []tracker.OrganizationID{org, "org_second"}
	var attempts [2]string
	for _, tenant := range tenants {
		pgstoretest.SeedLeaseFamily(t, repository, tenant)
		for i := range attempts {
			event := pgstoretest.AttemptEvent(pgstoretest.ClaimLease(t, repository, tenant, pgstoretest.LeaseClaim(int64(i+1))))
			pgstoretest.RecordLeaseAttempt(t, repository, tenant, event)
			attempts[i] = event.ID
		}
	}
	first, second := attempts[0], attempts[1]
	ctx := t.Context()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	migration := roleConnection(t, pgstoretest.Template.Template, db, postgres.MigrationRoleName)
	runtime := roleConnection(t, pgstoretest.Template.Template, db, postgres.RuntimeRoleName)
	cfg, err := migration.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["role"] = postgres.MigrationRoleName
	cfg.ConnConfig.RuntimeParams["search_path"] = "public, pg_catalog"
	restricted := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() {
		if err := restricted.Close(); err != nil {
			t.Error(err)
		}
	})
	restricted.SetMaxOpenConns(1)
	requireTenantIsolation := func() {
		t.Helper()
		var secured bool
		err := restricted.QueryRowContext(ctx, `SELECT current_user='detent_migration_v2' AND session_user<>current_user
			AND row_security_active('native_attempts') AND row_security_active('organizations')
			AND (SELECT bool_and(relforcerowsecurity) FROM pg_class WHERE oid IN ('native_attempts'::regclass,'organizations'::regclass))
			AND NULLIF(current_setting('detent.organization_id', true),'') IS NULL AND NOT EXISTS(SELECT FROM native_attempts)
			AND NOT EXISTS(SELECT FROM pg_policies WHERE policyname='worker_receipt_organizations')`).Scan(&secured)
		if err != nil || !secured {
			t.Fatalf("migration role has unexpected tenant visibility: %t, %v", secured, err)
		}
	}
	requireTenantIsolation()
	provider, err := pgstoretest.MigrationProvider(restricted)
	if err != nil {
		t.Fatal(err)
	}
	compatibility := fstest.MapFS{}
	feature := fstest.MapFS{}
	for name, file := range hubMigrationFiles(t) {
		version, _, _ := strings.Cut(name, "_")
		if version <= "20261118085855" {
			compatibility[name] = file
		}
		if version <= "20261119062927" {
			feature[name] = file
		}
	}
	coldStart := func(files fs.FS, want error) {
		t.Helper()
		hub, err := postgres.Open(ctx, postgres.Config{Connection: runtime, Pool: pgstoretest.SmallPool()}, files)
		if hub != nil {
			if closeErr := hub.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if !errors.Is(err, want) || (hub == nil) != (want != nil) {
			t.Fatalf("cold start = %v, want %v", err, want)
		}
	}
	const previous = 20261118085855
	const version = 20261119025159
	if _, err := provider.DownTo(ctx, previous); err != nil {
		t.Fatal(err)
	}
	coldStart(compatibility, nil)
	coldStart(feature, postgres.ErrSchema)
	if _, err := provider.UpTo(ctx, version); err != nil {
		t.Fatal(err)
	}
	var allNull bool
	if err := db.QueryRowContext(ctx, `SELECT num_nonnulls(worker_pid,worker_pgid,worker_started_at,worker_start_key,worker_runner_id)=0 FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, first).Scan(&allNull); err != nil || !allNull {
		t.Fatalf("legacy authority was backfilled or row disappeared: %t, %v", allNull, err)
	}
	for _, test := range []struct {
		name, assignments string
	}{
		{"partial tuple", "worker_pid=1"},
		{"nonpositive PID", "worker_pid=0,worker_pgid=2,worker_started_at=now(),worker_start_key='key',worker_runner_id='runner'"},
		{"nonpositive PGID", "worker_pid=1,worker_pgid=-1,worker_started_at=now(),worker_start_key='key',worker_runner_id='runner'"},
		{"blank start key", "worker_pid=1,worker_pgid=2,worker_started_at=now(),worker_start_key='  ',worker_runner_id='runner'"},
		{"infinite start", "worker_pid=1,worker_pgid=2,worker_started_at='infinity',worker_start_key='key',worker_runner_id='runner'"},
		{"missing indexed runner", "worker_pid=1,worker_pgid=2,worker_started_at=now(),worker_start_key='key',worker_runner_id=NULL"},
		{"oversized UTF-8 key", "worker_pid=1,worker_pgid=2,worker_started_at=now(),worker_start_key=repeat('é',65),worker_runner_id='runner'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), "UPDATE native_attempts SET "+test.assignments+" WHERE organization_id=$1 AND id=$2", org, first)
			var rejected *pgconn.PgError
			if !errors.As(err, &rejected) || rejected.ConstraintName != "native_attempts_worker_identity_check" || rejected.Code != "23514" {
				t.Fatalf("invalid worker tuple = %v", err)
			}
		})
	}
	for _, tenant := range tenants {
		if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET worker_pid=1,worker_pgid=2,worker_started_at=now(),worker_start_key=repeat('é',64),worker_runner_id='runner',cleanup_acknowledged_at=now() WHERE organization_id=$1 AND id=$2`, tenant, first); err != nil {
			t.Fatal(err)
		}
	}
	if err := postgres.Migrate(ctx, migration, feature); err != nil {
		t.Fatal(err)
	}
	coldStart(feature, nil)
	requireTenantIsolation()
	coldStart(compatibility, postgres.ErrSchema)
	var pending, legacySeek, typedSeek bool
	for _, org := range tenants {
		err = db.QueryRowContext(ctx, `SELECT worker_retired_at IS NULL AND cleanup_acknowledged_at IS NULL AND worker_pid=1,
		(SELECT indexdef LIKE '%worker_pid IS NULL%' FROM pg_indexes WHERE indexname='native_attempts_pending_cleanup_idx'),
		(SELECT indexdef LIKE '%worker_retired_at IS NULL%' FROM pg_indexes WHERE indexname='native_attempts_worker_inventory_idx')
		FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, first).Scan(&pending, &legacySeek, &typedSeek)
		if err != nil || !pending || !legacySeek || !typedSeek {
			t.Errorf("%s upgraded owner pending=%t legacy seek=%t worker seek=%t: %v", org, pending, legacySeek, typedSeek, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET cleanup_acknowledged_at=now() WHERE organization_id=$1 AND id=$2`, org, first); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET worker_pid=2,worker_pgid=2,worker_started_at=now(),worker_start_key='second-worker',worker_runner_id='runner',worker_retired_at=now(),cleanup_acknowledged_at=now() WHERE organization_id=$1 AND id=$2`, org, second); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		t.Fatal(err)
	}
	requireTenantIsolation()
	for _, org := range tenants {
		err = db.QueryRowContext(ctx, `SELECT cleanup_acknowledged_at IS NULL AND worker_pid=1,
		(SELECT indexdef NOT LIKE '%worker_pid%' FROM pg_indexes WHERE indexname='native_attempts_pending_cleanup_idx'),
		(SELECT indexdef LIKE '%cleanup_acknowledged_at IS NULL%' FROM pg_indexes WHERE indexname='native_attempts_worker_inventory_idx')
		FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, first).Scan(&pending, &legacySeek, &typedSeek)
		if err != nil || !pending || !legacySeek || !typedSeek {
			t.Errorf("%s rollback owner pending=%t legacy seek=%t worker seek=%t: %v", org, pending, legacySeek, typedSeek, err)
		}
		var acknowledged bool
		if err := db.QueryRowContext(ctx, `SELECT cleanup_acknowledged_at IS NOT NULL FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, second).Scan(&acknowledged); err != nil || !acknowledged {
			t.Errorf("%s rollback lost acknowledged attempt with retired worker: %t, %v", org, acknowledged, err)
		}
	}
	if err := postgres.Migrate(ctx, migration, feature); err != nil {
		t.Fatal(err)
	}
	const drainCheck = `DO $$
		DECLARE tenant text;
		BEGIN
			CREATE POLICY worker_drain_organizations ON organizations FOR SELECT TO detent_migration_v2 USING (true);
			FOR tenant IN SELECT organization_id FROM organizations ORDER BY organization_id LOOP
				PERFORM set_config('detent.organization_id', tenant, true);
				IF EXISTS (SELECT FROM native_attempts WHERE organization_id=tenant AND worker_pid IS NOT NULL
					AND (worker_retired_at IS NULL OR cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled'))) THEN
					RAISE EXCEPTION 'worker or attempt cleanup is outstanding' USING ERRCODE='55000';
				END IF;
			END LOOP;
			DROP POLICY worker_drain_organizations ON organizations;
		END $$`
	if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET cleanup_acknowledged_at=now() WHERE worker_pid IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	_, err = restricted.ExecContext(ctx, drainCheck)
	pgstoretest.RequireSQLState(t, err, "55000")
	for _, org := range tenants {
		if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET worker_retired_at=now(),status='cancelled',cleanup_acknowledged_at=NULL WHERE organization_id=$1 AND worker_pid IS NOT NULL`, org); err != nil {
			t.Fatal(err)
		}
		_, err = restricted.ExecContext(ctx, drainCheck)
		pgstoretest.RequireSQLState(t, err, "55000")
		if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET cleanup_acknowledged_at=now() WHERE organization_id=$1 AND worker_pid IS NOT NULL`, org); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restricted.ExecContext(ctx, drainCheck); err != nil {
		t.Fatalf("downgrade after drain: %v", err)
	}
	if _, err := provider.DownTo(ctx, previous); err != nil {
		t.Fatal(err)
	}
	coldStart(compatibility, nil)
	coldStart(feature, postgres.ErrSchema)
	var attempt, columns, indexes int
	err = db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND id=$2 AND sequence=1),
		(SELECT count(*) FROM pg_attribute WHERE attrelid='native_attempts'::regclass AND NOT attisdropped AND attname IN ('worker_pid','worker_pgid','worker_started_at','worker_start_key','worker_runner_id','worker_retired_at')),
		(SELECT count(*) FROM pg_indexes WHERE indexname='native_attempts_pending_cleanup_idx' AND indexdef NOT LIKE '%worker_pid%') + (SELECT count(*) FROM pg_indexes WHERE indexname='native_attempts_worker_inventory_idx')`, org, first).Scan(&attempt, &columns, &indexes)
	if err != nil || attempt != 1 || columns != 0 || indexes != 1 {
		t.Fatalf("rollback attempt=%d columns=%d indexes=%d: %v", attempt, columns, indexes, err)
	}
}
