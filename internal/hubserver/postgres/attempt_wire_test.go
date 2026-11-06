package postgres_test

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type attemptWireCounts struct {
	exchanges  atomic.Int64
	statements atomic.Int64
}

type attemptWireConn struct {
	net.Conn
	counts  *attemptWireCounts
	pending []byte
	startup bool
}

func (c *attemptWireConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.pending = append(c.pending, value[:n]...)
	for len(c.pending) >= 5 {
		if c.startup {
			size := int(binary.BigEndian.Uint32(c.pending[:4]))
			if len(c.pending) < size {
				break
			}
			c.pending = c.pending[size:]
			c.startup = false
			continue
		}
		size := 1 + int(binary.BigEndian.Uint32(c.pending[1:5]))
		if len(c.pending) < size {
			break
		}
		switch c.pending[0] {
		case 'S':
			c.counts.exchanges.Add(1)
		case 'Q':
			c.counts.exchanges.Add(1)
			c.counts.statements.Add(1)
		case 'E':
			c.counts.statements.Add(1)
		}
		c.pending = c.pending[size:]
	}
	return n, err
}

func attemptCountedRepository(t *testing.T, repository *postgres.Repository, pool *pgxpool.Pool) (*postgres.Repository, *attemptWireCounts) {
	t.Helper()
	config := pool.Config()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	counts := &attemptWireCounts{}
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &attemptWireConn{Conn: conn, counts: counts, startup: true}, nil
	}
	counted, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = postgres.NewRepository(t.Context(), counted, smallPool())
	if err != nil {
		counted.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, counts
}
