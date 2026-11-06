package tenantauth_test

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type wireCounts struct {
	mu                    sync.Mutex
	exchanges, statements int
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
			c.counts.exchanges++
		case 'Q':
			c.counts.exchanges++
			c.counts.statements++
		case 'E':
			c.counts.statements++
		}
		c.counts.mu.Unlock()
		c.pending = c.pending[size:]
	}
	return n, err
}

func TestAuthorityUsesTwoWireExchangesAndFourStatements(t *testing.T) {
	counts := new(wireCounts)
	_, pools := fixture(t, func(config *pgxpool.Config) {
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := new(net.Dialer).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &countedConn{Conn: conn, counts: counts, startup: true}, nil
		}
	})
	authority := tenantauth.New(pools, nil)
	for _, org := range []string{"first", "second", "first"} {
		counts.mu.Lock()
		counts.exchanges = 0
		counts.statements = 0
		counts.mu.Unlock()
		input := tenantauth.RequestIdentity{Organization: tracker.OrganizationID(org), CredentialHash: "same-proof"}
		if _, err := authority.Resolve(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		counts.mu.Lock()
		exchanges, statements := counts.exchanges, counts.statements
		counts.mu.Unlock()
		if exchanges != 2 || statements != 4 {
			t.Fatalf("organization=%s exchanges=%d statements=%d", org, exchanges, statements)
		}
		t.Logf("organization=%s exchanges=%d statements=%d", org, exchanges, statements)
	}
}
