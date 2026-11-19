package schemaupgrade_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptWorkerIdentityMigration(t *testing.T) {
	t.Parallel()
	db := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, db, pgstoretest.SmallPool())
	const org = tracker.OrganizationID("org_first")
	pgstoretest.SeedLeaseFamily(t, repository, org)
	lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
	event := pgstoretest.AttemptEvent(lease)
	pgstoretest.RecordLeaseAttempt(t, repository, org, event)

	ctx := t.Context()
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	const previous = 20261118085855
	const version = 20261119025159
	if _, err := provider.DownTo(ctx, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, version); err != nil {
		t.Fatal(err)
	}
	var allNull bool
	if err := db.QueryRowContext(ctx, `SELECT num_nonnulls(worker_pid,worker_pgid,worker_started_at,worker_start_key,worker_runner_id)=0 FROM native_attempts WHERE organization_id=$1 AND id=$2`, org, event.ID).Scan(&allNull); err != nil || !allNull {
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
			_, err := db.ExecContext(t.Context(), "UPDATE native_attempts SET "+test.assignments+" WHERE organization_id=$1 AND id=$2", org, event.ID)
			var rejected *pgconn.PgError
			if !errors.As(err, &rejected) || rejected.ConstraintName != "native_attempts_worker_identity_check" || rejected.Code != "23514" {
				t.Fatalf("invalid worker tuple = %v", err)
			}
		})
	}
	if _, err := db.ExecContext(ctx, `UPDATE native_attempts SET worker_pid=1,worker_pgid=2,worker_started_at=now(),worker_start_key=repeat('é',64),worker_runner_id='runner' WHERE organization_id=$1 AND id=$2`, org, event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, previous); err != nil {
		t.Fatal(err)
	}
	var attempt, columns, indexes int
	err = db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND id=$2 AND sequence=1),
		(SELECT count(*) FROM pg_attribute WHERE attrelid='native_attempts'::regclass AND NOT attisdropped AND attname IN ('worker_pid','worker_pgid','worker_started_at','worker_start_key','worker_runner_id')),
		(SELECT count(*) FROM pg_indexes WHERE indexname='native_attempts_pending_cleanup_idx' AND indexdef NOT LIKE '%worker_pid%') + (SELECT count(*) FROM pg_indexes WHERE indexname='native_attempts_worker_inventory_idx')`, org, event.ID).Scan(&attempt, &columns, &indexes)
	if err != nil || attempt != 1 || columns != 0 || indexes != 1 {
		t.Fatalf("rollback attempt=%d columns=%d indexes=%d: %v", attempt, columns, indexes, err)
	}
}
