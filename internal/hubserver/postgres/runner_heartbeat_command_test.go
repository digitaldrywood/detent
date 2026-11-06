package postgres_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5/pgxpool"
)

type heartbeatWire struct {
	sync.Mutex
	statements, exchanges int
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

type heartbeatCommand interface {
	WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
}

func TestRunnerHeartbeatCommandBoundsAndRollback(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	now := historyAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return seedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		seedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
	}
	wire := new(heartbeatWire)
	config := pool.Config()
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &heartbeatConn{Conn: conn, wire: wire, startup: true}, nil
	}
	countedPool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(countedPool.Close)
	counted, err := postgres.NewRepository(t.Context(), countedPool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	command, ok := any(counted).(heartbeatCommand)
	if !ok {
		t.Fatal("heartbeat has no bounded transaction command")
	}
	for _, size := range []int{2, 50} {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) SELECT $1,'project_'||i,'Example','native' FROM generate_series(1,$2) i ON CONFLICT DO NOTHING`, org, size); err != nil {
				t.Fatal(err)
			}
			for _, rollback := range []bool{false, true} {
				wire.Lock()
				wire.statements = 0
				wire.exchanges = 0
				wire.Unlock()
				sentinel := errors.New("rollback heartbeat")
				err := command.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
					identity, err := w.RunnerIdentityByCredential(ctx, "runner_shared")
					if err != nil {
						return err
					}
					if identity.OrganizationID != org {
						t.Fatalf("cross-tenant identity: %s", identity.OrganizationID)
					}
					if _, err := w.AccessCredential(ctx, "runner_shared"); err != nil {
						return err
					}
					identity.ReportedCapacity = 7
					if rollback {
						identity.ReportedCapacity = 9
					}
					identity.LastHeartbeatAt = now.Add(time.Minute)
					if err := w.UpdateRunnerIdentity(ctx, identity); err != nil {
						return err
					}
					if err := w.HeartbeatMachine(ctx, leasestore.Machine{OrganizationID: org, ID: "machine_shared", Capacity: identity.ReportedCapacity, Version: "example"}, now.Add(time.Minute)); err != nil {
						return err
					}
					if err := w.ReplaceRunnerBackendIsolation(ctx, "runner_shared", isolation.Report{"example": {isolation.NativeTrusted}}); err != nil {
						return err
					}
					problems, err := w.RunnerProblems(ctx, "runner_shared")
					if err != nil {
						return err
					}
					if len(problems) != 0 {
						t.Fatal(problems)
					}
					if _, err := w.RunnerProjects(ctx, "runner_shared", runnerstore.MaxRunnerProjectList); err != nil {
						return err
					}
					if _, err := w.RunnerRepositoryList(ctx, "runner_shared"); err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					if _, err := w.RunnerUpdateState(ctx, "runner_shared"); err != nil {
						return err
					}
					if _, err := w.RunnerUrgentUpdate(ctx); err != nil {
						return err
					}
					if _, err := w.RunnerHasActiveLease(ctx, "runner_shared", now.Add(time.Minute)); err != nil {
						return err
					}
					if rollback {
						return sentinel
					}
					return nil
				})
				if rollback && !errors.Is(err, sentinel) || !rollback && err != nil {
					t.Fatal(err)
				}
				wire.Lock()
				statements, exchanges := wire.statements, wire.exchanges
				wire.Unlock()
				t.Logf("tenant=%s projects=%d rollback=%t statements=%d exchanges=%d", org, size, rollback, statements, exchanges)
				if statements > 12 || exchanges > 8 {
					t.Errorf("heartbeat exceeds 12 statements / 8 exchanges: %d / %d", statements, exchanges)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					identity, err := r.RunnerIdentity(t.Context(), "runner_shared")
					if err != nil {
						return err
					}
					machine, err := r.Machine(t.Context(), "machine_shared")
					if err != nil {
						return err
					}
					if identity.ReportedCapacity != 7 || machine.Capacity != 7 {
						t.Errorf("rollback changed liveness: runner=%d machine=%d", identity.ReportedCapacity, machine.Capacity)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
