package hubserver

import (
	"database/sql"
	"net"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest/pgwire"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type boardWire struct {
	wire             pgwire.Wire
	delay            time.Duration
	pingEveryAcquire bool
}

func (w *boardWire) reset()           { w.wire.Reset() }
func (w *boardWire) stop() (int, int) { return w.wire.Stop() }
func boardWireRepository(t *testing.T, admin *sql.DB, wire *boardWire) *postgres.Repository {
	t.Helper()
	wire.wire.Delay = wire.delay
	wire.wire.PingEveryAcquire = wire.pingEveryAcquire
	return pgwire.Repository(t, admin, startupConnection(t, admin, postgres.RuntimeRoleName, ""), &wire.wire)
}

func boardWireConnection(conn net.Conn, wire *boardWire) net.Conn {
	return pgwire.Wrap(conn, &wire.wire)
}
