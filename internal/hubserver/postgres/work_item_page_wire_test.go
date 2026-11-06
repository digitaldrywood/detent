package postgres_test

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type pageWireCounts struct {
	mu                                    sync.Mutex
	statements, exchanges                 int
	checkoutStatements, checkoutExchanges int
}

func (c *pageWireCounts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements, c.exchanges, c.checkoutStatements, c.checkoutExchanges = 0, 0, 0, 0
}
func (c *pageWireCounts) read() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statements, c.exchanges
}

func (c *pageWireCounts) beginServing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkoutStatements, c.checkoutExchanges = c.statements, c.exchanges
	c.statements, c.exchanges = 0, 0
}
func (c *pageWireCounts) checkout() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkoutStatements, c.checkoutExchanges
}

type pageWireConn struct {
	net.Conn
	counts  *pageWireCounts
	pending []byte
	startup bool
}

func (c *pageWireConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	c.pending = append(c.pending, data[:n]...)
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
		case 'P':
			c.counts.statements++
		case 'Q':
			c.counts.statements++
			c.counts.exchanges++
		case 'S':
			c.counts.exchanges++
		}
		c.counts.mu.Unlock()
		c.pending = c.pending[size:]
	}
	return n, err
}
func pageMeasuredRepository(t *testing.T, source *pgxpool.Pool, counts *pageWireCounts) *postgres.Repository {
	t.Helper()
	config := source.Config()
	prepare := config.PrepareConn
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		if prepare != nil {
			ok, err := prepare(ctx, conn)
			if !ok || err != nil {
				return ok, err
			}
		}
		counts.beginServing()
		return true, nil
	}
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &pageWireConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	return repository
}
