package platformstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func TestNotificationQueuesPreserveTimestampAndScope(t *testing.T) {
	r := repository(t, template.Database(t), testConfig(t))
	at := time.Date(2026, 11, 1, 12, 0, 0, 123456000, time.FixedZone("example", -6*60*60))
	for _, account := range []bool{false, true} {
		tx, err := r.BeginEntry(t.Context(), corestore.Serving, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			if err := tx.EnqueueNotification(t.Context(), account, Notification{Event: "signup", ID: id, Email: id + "@example.test", OccurredAt: at.Format(time.RFC3339Nano)}); err != nil {
				tx.Rollback(t.Context())
				t.Fatal(err)
			}
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []bool{false, true} {
		found := map[string]bool{}
		for range 2 {
			item, err := r.ClaimNotification(t.Context(), corestore.Serving, account, at.Add(time.Hour).Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := time.Parse(time.RFC3339Nano, item.OccurredAt)
			if err != nil || !parsed.Equal(at) || parsed.Location() != time.UTC {
				t.Fatalf("notification timestamp=%s err=%v", item.OccurredAt, err)
			}
			if found[item.ID] {
				t.Fatal("notification claimed twice")
			}
			found[item.ID] = true
		}
		if !found["first"] || !found["second"] {
			t.Fatalf("notification recipients=%v", found)
		}
		if _, err := r.ClaimNotification(t.Context(), corestore.Serving, account, at.Format(time.RFC3339Nano)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("empty queue=%v", err)
		}
	}
}

func TestConstructorRejectsUnconfiguredPoolBeforeConnect(t *testing.T) {
	native, err := pgxpool.ParseConfig("postgres://example:fixture@127.0.0.1:1/example?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	native.MaxConns = 3
	calls := 0
	native.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { calls++; return errors.New("unexpected connection") }
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := New(t.Context(), pool, testConfig(t)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unconfigured pool=%v", err)
	}
	if calls != 0 {
		t.Fatal("constructor connected before validating native execution")
	}
}

func TestPlatformWriteReturningAfterColumnTypeChange(t *testing.T) {
	for _, path := range []string{"standalone", "transaction", "batch"} {
		t.Run(path, func(t *testing.T) {
			ctx := t.Context()
			admin := template.Database(t)
			r := repository(t, admin, testConfig(t))
			if _, err := admin.ExecContext(ctx, "CREATE TABLE platform.example_writes (id text); GRANT SELECT,INSERT ON platform.example_writes TO detent_platform_v2"); err != nil {
				t.Fatal(err)
			}
			const statement = "INSERT INTO platform.example_writes VALUES ($1) RETURNING id,pg_backend_pid()"
			var backend int
			for _, key := range []string{"before", "after"} {
				if key == "after" {
					if _, err := admin.ExecContext(ctx, "ALTER TABLE platform.example_writes ALTER COLUMN id TYPE varchar(20)"); err != nil {
						t.Fatal(err)
					}
				}
				var id string
				var pid int
				var err error
				if path == "standalone" {
					err = r.QueryRowEntry(ctx, corestore.Serving, statement, key).Scan(&id, &pid)
				} else {
					tx, beginErr := r.BeginEntry(ctx, corestore.Serving, false)
					if beginErr != nil {
						t.Fatal(beginErr)
					}
					defer tx.Rollback(ctx)
					if path == "batch" {
						batch := &pgx.Batch{}
						batch.Queue(statement, key)
						results := tx.SendBatch(ctx, batch)
						err = errors.Join(results.QueryRow().Scan(&id, &pid), results.Close())
					} else {
						err = tx.QueryRow(ctx, statement, key).Scan(&id, &pid)
					}
					if err == nil {
						err = tx.Commit(ctx)
					}
				}
				if err != nil || id != key || (key == "after" && pid != backend) {
					t.Fatalf("%s: returning %q backend=%d previous=%d error=%v", key, id, pid, backend, err)
				}
				backend = pid
			}
			var n int
			if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM platform.example_writes WHERE id='after'").Scan(&n); err != nil || n != 1 {
				t.Fatalf("committed rows=%d error=%v", n, err)
			}
		})
	}
}
