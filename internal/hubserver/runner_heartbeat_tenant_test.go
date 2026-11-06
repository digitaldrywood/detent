package hubserver

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type heartbeatWire struct {
	sync.Mutex
	statements, exchanges, transactions int
}
type heartbeatConn struct {
	net.Conn
	wire    *heartbeatWire
	pending []byte
	startup bool
}

func (c *heartbeatConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.pending = append(c.pending, value[:n]...)
	for {
		offset := 1
		if c.startup {
			offset = 0
		}
		if len(c.pending) < offset+4 {
			break
		}
		size := offset + int(binary.BigEndian.Uint32(c.pending[offset:offset+4]))
		if len(c.pending) < size {
			break
		}
		if c.startup {
			c.startup = false
		} else {
			c.wire.Lock()
			switch c.pending[0] {
			case 'P':
				if len(c.pending) > 6 && strings.HasPrefix(string(c.pending[6:]), "BEGIN ") {
					c.wire.transactions++
				}
			case 'E':
				c.wire.statements++
			case 'S':
				c.wire.exchanges++
			case 'Q':
				c.wire.statements++
				c.wire.exchanges++
			}
			c.wire.Unlock()
		}
		c.pending = c.pending[size:]
	}
	return n, err
}

func TestTenantHeartbeatHTTPCommandAndAuthority(t *testing.T) {
	f := newRunnerPGFixture(t)
	connection := startupConnection(t, f.admin, "detent_runtime_v2", "")
	config := postgresStartupTemplate.NativeConfig(t, f.admin, connection.User, connection.Password)
	config.MaxConns = 3
	wire := new(heartbeatWire)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &heartbeatConn{Conn: conn, wire: wire, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	original := f.service.database.postgres
	t.Cleanup(func() {
		f.service.config.TenantRepository = original
		f.service.database.postgres = original
	})
	f.service.config.TenantRepository = repository
	f.service.database.postgres = repository
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE runner_identities SET display_name='Example' WHERE organization_id='org_startup'`); err != nil {
		t.Fatal(err)
	}
	for index, binding := range f.bindings {
		path := "/api/v2/organizations/org_startup/projects/prj_runner/machines/" + string(binding.MachineID) + "/heartbeat"
		request := nativeMachineHeartbeatRequest{DisplayName: "Example", Capacity: 1, Version: "0.183.0", ProtocolMajor: 1, OS: "linux", Architecture: "amd64", BackendIsolation: isolation.Report{"example": {isolation.NativeTrusted}}}
		wire.Lock()
		wire.statements = 0
		wire.exchanges = 0
		wire.transactions = 0
		wire.Unlock()
		response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], request)
		requireNativeStatus(t, response, http.StatusOK)
		wire.Lock()
		statements, exchanges, transactions := wire.statements, wire.exchanges, wire.transactions
		wire.Unlock()
		t.Logf("full HTTP heartbeat statements=%d exchanges=%d transactions=%d", statements, exchanges, transactions)
		var snapshot runnerauth.RoutingSnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.RunnerID != binding.RunnerID || snapshot.Revision != 1 {
			t.Fatalf("wrong snapshot: %+v", snapshot)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=clock_timestamp() WHERE organization_id='org_startup' AND id=$1`, fmt.Sprintf("credential_%d", index+1)); err != nil {
			t.Fatal(err)
		}
		denied := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], request)
		requireNativeStatus(t, denied, http.StatusUnauthorized)
	}
}
