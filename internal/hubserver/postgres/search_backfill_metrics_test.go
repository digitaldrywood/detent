package postgres_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type searchWireCounts struct {
	sync.Mutex
	statements, exchanges int
}

type searchCountedConn struct {
	net.Conn
	counts  *searchWireCounts
	pending []byte
	startup bool
}

func (c *searchCountedConn) Write(value []byte) (int, error) {
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
		c.counts.Lock()
		switch c.pending[0] {
		case 'S':
			c.counts.exchanges++
		case 'Q':
			c.counts.exchanges++
			c.counts.statements++
		case 'E':
			c.counts.statements++
		}
		c.counts.Unlock()
		c.pending = c.pending[size:]
	}
	return n, err
}

func TestSearchBackfillWorkDoesNotGrowWithHistory(t *testing.T) {
	for _, size := range []int{4, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newSearchFixture(t)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				_, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents
     (organization_id,id,kind,source_id,revision,indexed_revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
     SELECT $1,'thread/'||lpad(n::text,4,'0'),'thread','source/'||n,1,1,'sentinel','general',now(),'example',repeat('a',64),1,1
     FROM generate_series(1,$2::integer) n`, org, size)
				if err != nil {
					t.Fatal(err)
				}
			}
			counts := new(searchWireCounts)
			config := f.pool.Config()
			dial := config.ConnConfig.DialFunc
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &searchCountedConn{Conn: conn, counts: counts, startup: true}, nil
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
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				counts.Lock()
				counts.statements = 0
				counts.exchanges = 0
				counts.Unlock()
				n, err := repository.QueueSearchBackfill(t.Context(), org, 2)
				counts.Lock()
				statements, exchanges := counts.statements, counts.exchanges
				counts.Unlock()
				if err != nil || n != 2 {
					t.Fatalf("n=%d err=%v", n, err)
				}
				if statements != 12 || exchanges != 9 {
					t.Fatalf("history=%d statements=%d exchanges=%d want=12/9", size, statements, exchanges)
				}
				t.Logf("tenant=%s history=%d page=2 statements=%d wire exchanges=%d", org, size, statements, exchanges)
			}
		})
	}
}
