package pgtransport_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestInitialBatchReadsOnlyTenantRows(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	for _, tenant := range []tracker.OrganizationID{"first", "second"} {
		work := &pgx.Batch{}
		work.Queue(`SELECT tenant FROM protected WHERE id=$1`, "same")
		work.Queue(`SELECT count(*) FROM protected`)
		tx, results, err := pools.BeginBatch(t.Context(), pgtransport.Scope{Organization: tenant}, pgtransport.Options{ReadOnly: true}, work)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		var got string
		var rows int
		if err := results.QueryRow().Scan(&got); err != nil {
			t.Fatal(err)
		}
		if err := results.QueryRow().Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := results.Close(); err != nil {
			t.Fatal(err)
		}
		if got != string(tenant) || rows != 1 {
			t.Fatalf("tenant=%s rows=%d, want %s and one row", got, rows, tenant)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCancellationDuringProtectedRead(t *testing.T) {
	admin, config := fixture(t)
	admin.SetMaxOpenConns(2)
	pools := openPools(t, config, nil)
	tx := begin(t, pools, pgtransport.Options{ReadOnly: true})
	var pid int
	if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	lock, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(t.Context(), `LOCK TABLE protected IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { var count int; done <- tx.QueryRow(ctx, `SELECT count(*) FROM protected`).Scan(&count) }()
	t.Cleanup(func() { cancel(); lock.Rollback(); <-done })
	testenv.WaitUntil(t, t.Context(), func() error {
		var blocked bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&blocked); err != nil {
			return err
		}
		if !blocked {
			return errors.New("protected read has not reached its lock wait")
		}
		return nil
	})
	cancel()
	err = <-done
	done <- err
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked read cancellation: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	reuse(t, pools)
}

func TestResultsHideConnections(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	for _, name := range []string{"direct", "batch", "initial batch", "callback", "initial callback"} {
		t.Run(name, func(t *testing.T) {
			var retained pgx.Rows
			work := &pgx.Batch{}
			queued := work.Queue(`SELECT * FROM protected`)
			callback := strings.Contains(name, "callback")
			if callback {
				queued.Query(func(rows pgx.Rows) error { retained = rows; return nil })
			}
			var tx *pgtransport.Transaction
			var results pgx.BatchResults
			var err error
			if strings.HasPrefix(name, "initial") {
				tx, results, err = pools.BeginBatch(t.Context(), pgtransport.Scope{Organization: "first"}, pgtransport.Options{ReadOnly: true}, work)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				tx = begin(t, pools, pgtransport.Options{ReadOnly: true})
				if name != "direct" {
					results = tx.SendBatch(t.Context(), work)
				}
			}
			defer tx.Rollback(t.Context())
			if name == "direct" {
				retained, err = tx.Query(t.Context(), `SELECT * FROM protected`)
			} else if !callback {
				retained, err = results.Query()
			}
			if err != nil {
				t.Fatal(err)
			}
			if retained != nil {
				retained.Close()
			}
			if results != nil {
				if err := results.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if retained == nil {
				t.Fatal("missing result")
			}
			if retained.Conn() != nil {
				t.Error("result exposes its connection")
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			reuse(t, pools)
			if retained.Conn() != nil {
				t.Error("retained result exposes a released connection")
			}
		})
	}
}
