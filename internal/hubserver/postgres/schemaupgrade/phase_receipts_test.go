package schemaupgrade_test

import (
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
)

func TestPhaseReceiptMigrationRollsBackAndReapplies(t *testing.T) {
	t.Parallel()
	const baseline = 20261119224706
	const migration = 20261120022652
	db, fixture := pgstoretest.MigrationDatabaseAtVersion(t, baseline)
	pgstoretest.SeedCore(t, db)
	role := roleConnection(t, fixture, db, "detent_migration_v2")
	pool, err := role.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	migrationDB := stdlib.OpenDB(*pool.ConnConfig)
	t.Cleanup(func() {
		if err := migrationDB.Close(); err != nil {
			t.Error(err)
		}
	})
	provider, err := pgstoretest.MigrationProvider(migrationDB)
	if err != nil {
		t.Fatal(err)
	}
	check := func(present bool) {
		t.Helper()
		var table, index, column bool
		err := db.QueryRowContext(t.Context(), `SELECT
		to_regclass('native_phase_receipts') IS NOT NULL,
		EXISTS (SELECT FROM pg_index WHERE indexrelid=to_regclass('native_attempts_round_verdict_idx') AND indisvalid AND indisready),
		EXISTS (SELECT FROM pg_attribute WHERE attrelid='native_attempts'::regclass AND attname='verdict_round_id' AND NOT attisdropped)`).Scan(&table, &index, &column)
		if err != nil || table != present || index != present || column != present {
			t.Fatalf("migration present=%v, table=%v index=%v column=%v: %v", present, table, index, column, err)
		}
		for _, organization := range []string{"org_first", "org_second"} {
			var issues int
			err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1 AND native_id='wi_shared'`, organization).Scan(&issues)
			if err != nil || issues != 1 {
				t.Fatalf("tenant %s has %d preserved issues: %v", organization, issues, err)
			}
		}
	}
	if _, err := provider.UpTo(t.Context(), migration); err != nil {
		t.Fatal(err)
	}
	check(true)
	if _, err := provider.DownTo(t.Context(), baseline); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := provider.UpTo(t.Context(), migration); err != nil {
		t.Fatal(err)
	}
	check(true)
}
