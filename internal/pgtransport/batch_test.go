package pgtransport_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type wireCounts struct {
	mu                               sync.Mutex
	syncs, queries, parses, executes int
}

type countedConn struct {
	net.Conn
	counts  *wireCounts
	pending []byte
	startup bool
}

func (c *countedConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.pending = append(c.pending, value[:n]...)
	for {
		if c.startup {
			if len(c.pending) < 4 {
				break
			}
			size := int(binary.BigEndian.Uint32(c.pending[:4]))
			if len(c.pending) < size {
				break
			}
			c.pending = c.pending[size:]
			c.startup = false
			continue
		}
		if len(c.pending) < 5 {
			break
		}
		size := 1 + int(binary.BigEndian.Uint32(c.pending[1:5]))
		if len(c.pending) < size {
			break
		}
		c.counts.mu.Lock()
		switch c.pending[0] {
		case 'S':
			c.counts.syncs++
		case 'Q':
			c.counts.queries++
		case 'P':
			c.counts.parses++
		case 'E':
			c.counts.executes++
		}
		c.counts.mu.Unlock()
		c.pending = c.pending[size:]
	}
	return n, err
}
func (c *wireCounts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncs = 0
	c.queries = 0
	c.parses = 0
	c.executes = 0
}
func (c *wireCounts) counts() (int, int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncs, c.queries, c.parses, c.executes
}

func TestInitialWorkOneExchange(t *testing.T) {
	_, config := fixture(t)
	counts := new(wireCounts)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &countedConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pools := openPools(t, config, nil)
	for _, tenant := range []tracker.OrganizationID{"first", "second"} {
		counts.reset()
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
		syncs, queries, parses, executes := counts.counts()
		if got != string(tenant) || rows != 1 || syncs != 1 || queries != 0 || parses != 4 || executes != 4 {
			t.Fatalf("tenant=%s rows=%d Sync=%d Q=%d Parse=%d Execute=%d", got, rows, syncs, queries, parses, executes)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		syncs, queries, _, _ = counts.counts()
		if syncs+queries != 2 {
			t.Fatalf("full exchanges=%d", syncs+queries)
		}
		t.Logf("tenant=%s BEGIN+bind+two protected reads: statements=4 exchanges=1; including COMMIT: statements=5 exchanges=2", tenant)
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

func TestReadRowOneExchange(t *testing.T) {
	_, config := fixture(t)
	config.MaxConns = 1
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	counts := new(wireCounts)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &countedConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pools, err := pgtransport.Adopt(pool, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	var previousPID int
	for _, org := range []tracker.OrganizationID{"first", "second", "first"} {
		scope := pgtransport.Scope{Organization: org}
		var visible string
		var pid int
		const sql = "SELECT string_agg(tenant, ',' ORDER BY tenant), pg_backend_pid() FROM protected"
		counts.reset()
		if err := pools.ReadRow(t.Context(), scope, sql, nil, &visible, &pid); err != nil {
			t.Fatal(err)
		}
		syncs, queries, _, executes := counts.counts()
		if visible != string(org) || previousPID != 0 && previousPID != pid {
			t.Fatalf("scope=%s visible=%s pid=%d previous=%d", org, visible, pid, previousPID)
		}
		previousPID = pid
		if exchanges, statements := syncs+queries, executes+queries; exchanges != 1 || statements != 2 {
			t.Errorf("scope=%s statements=%d exchanges=%d, want 2 and 1", org, statements, exchanges)
		}
	}
	if err := pools.ReadRow(t.Context(), pgtransport.Scope{}, "SELECT 1", nil, new(int)); !errors.Is(err, pgtransport.ErrTenantRequired) {
		t.Errorf("empty scope returned %v", err)
	}
	var state *pgconn.PgError
	err = pools.ReadRow(t.Context(), pgtransport.Scope{Organization: "first"}, "INSERT INTO protected VALUES ('first','another') RETURNING id", nil, new(string))
	if !errors.As(err, &state) || state.Code != "25006" {
		t.Fatalf("read-only lookup allowed a write or failed for a different reason: %v", err)
	}
	var visible, pid int
	var scope string
	if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), "SELECT count(*), pg_backend_pid(), coalesce(current_setting('detent.organization_id', true), '') FROM protected").Scan(&visible, &pid, &scope); err != nil {
		t.Fatal(err)
	}
	if visible != 0 || scope != "" || pid != previousPID {
		t.Fatalf("released connection pid=%d scope=%q visible=%d, expected pid=%d with no visible rows", pid, scope, visible, previousPID)
	}
}
