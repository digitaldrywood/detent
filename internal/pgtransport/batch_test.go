package pgtransport_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
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
