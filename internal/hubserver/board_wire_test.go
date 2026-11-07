package hubserver

import (
	"context"
	"database/sql"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

type boardWire struct {
	mu                    sync.Mutex
	enabled               bool
	exchanges, statements int
	delay                 time.Duration
	pingEveryAcquire      bool
}

type boardWireConn struct {
	net.Conn
	wire              *boardWire
	frontend, backend []byte
	startup           bool
}

func (c *boardWireConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.frontend = append(c.frontend, value[:n]...)
	for {
		if c.startup {
			if len(c.frontend) < 4 {
				break
			}
			size := int(binary.BigEndian.Uint32(c.frontend[:4]))
			if len(c.frontend) < size {
				break
			}
			c.frontend = c.frontend[size:]
			c.startup = false
			continue
		}
		if len(c.frontend) < 5 {
			break
		}
		size := 1 + int(binary.BigEndian.Uint32(c.frontend[1:5]))
		if len(c.frontend) < size {
			break
		}
		c.wire.mu.Lock()
		if c.wire.enabled {
			switch c.frontend[0] {
			case 'S':
				c.wire.exchanges++
			case 'Q':
				c.wire.exchanges++
				c.wire.statements++
			case 'E':
				c.wire.statements++
			}
		}
		c.wire.mu.Unlock()
		c.frontend = c.frontend[size:]
	}
	return n, err
}

func (c *boardWireConn) Read(value []byte) (int, error) {
	n, err := c.Conn.Read(value)
	c.backend = append(c.backend, value[:n]...)
	for len(c.backend) >= 5 {
		size := 1 + int(binary.BigEndian.Uint32(c.backend[1:5]))
		if len(c.backend) < size {
			break
		}
		c.wire.mu.Lock()
		delay := time.Duration(0)
		if c.wire.enabled && c.backend[0] == 'Z' {
			delay = c.wire.delay
		}
		c.wire.mu.Unlock()
		c.backend = c.backend[size:]
		if delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}
	}
	return n, err
}

func (w *boardWire) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.exchanges, w.statements = 0, 0
	w.enabled = true
}
func (w *boardWire) stop() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled = false
	return w.exchanges, w.statements
}

func boardWireRepository(t *testing.T, admin *sql.DB, wire *boardWire) *postgres.Repository {
	t.Helper()
	connection := startupConnection(t, admin, postgres.RuntimeRoleName, "")
	config, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 10"); err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return wire.pingEveryAcquire }
	pgtransport.Configure(config)
	tables := []string{}
	rows, err := admin.QueryContext(t.Context(), "SELECT relname FROM pg_class WHERE relnamespace='public'::regnamespace AND relrowsecurity")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Tracer = pgtest.NewTenantGuard(tables, func(message string) { t.Error(message) })
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &boardWireConn{Conn: conn, wire: wire, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository
}
