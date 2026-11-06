package platformstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runtimeWireCounts struct {
	mu                    sync.Mutex
	statements, exchanges int
}

type runtimeWireConn struct {
	net.Conn
	counts  *runtimeWireCounts
	pending []byte
	startup bool
}

func (c *runtimeWireConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	c.pending = append(c.pending, data[:n]...)
	for {
		if c.startup {
			if len(c.pending) < 4 || len(c.pending) < int(binary.BigEndian.Uint32(c.pending[:4])) {
				break
			}
			c.pending = c.pending[int(binary.BigEndian.Uint32(c.pending[:4])):]
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
		if c.pending[0] == 'Q' || c.pending[0] == 'E' {
			c.counts.statements++
		}
		if c.pending[0] == 'Q' || c.pending[0] == 'S' {
			c.counts.exchanges++
		}
		c.counts.mu.Unlock()
		c.pending = c.pending[size:]
	}
	return n, err
}

func (c *runtimeWireCounts) take() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	statements, exchanges := c.statements, c.exchanges
	c.statements, c.exchanges = 0, 0
	return statements, exchanges
}

func TestRuntimePublicationWorkDoesNotGrowWithTenants(t *testing.T) {
	admin := template.Database(t)
	base := rolePool(t, admin, postgres.PlatformRoleName, "")
	native := base.Config()
	base.Close()
	counts := &runtimeWireCounts{}
	dial := native.ConnConfig.DialFunc
	native.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &runtimeWireConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r, err := New(t.Context(), pool, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	seedOrganizations(t, r)
	for _, size := range []int{4, 100} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO platform.organizations(id,name,state,endpoint,generation,created_at,updated_at)
SELECT 'org_scale_'||n::text,'Example','ready','unix:/example/scale.sock',1,now(),now() FROM generate_series(1,$1::integer) n ON CONFLICT DO NOTHING`, size-2); err != nil {
			t.Fatal(err)
		}
		counts.take()
		revision := uuid.NewString()
		if err := r.PublishRuntime(t.Context(), revision, "entry", "example/payload", nil); err != nil {
			t.Fatal(err)
		}
		if statements, exchanges := counts.take(); statements != 5 || exchanges != 5 {
			t.Fatalf("%d tenants publication: statements=%d exchanges=%d", size, statements, exchanges)
		}
		page, err := r.RuntimeTenantPage(t.Context(), "", 2)
		if err != nil || len(page) != 2 || page[0].Organization != "org_first" || page[1].Organization != "org_scale_1" {
			t.Fatalf("%d tenants page=%+v error=%v", size, page, err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			target, err := r.RuntimeAdoption(t.Context(), org, "entry")
			if err != nil || target.Generation != 1 || target.Publication.Revision != revision || target.PreparedRevision != "" {
				t.Fatalf("%s adoption=%+v error=%v", org, target, err)
			}
		}
		if statements, exchanges := counts.take(); statements != 3 || exchanges != 3 {
			t.Fatalf("%d tenants page and two targets: statements=%d exchanges=%d", size, statements, exchanges)
		}
		t.Logf("tenants=%d publication: 5 statements/5 exchanges; page and two targets: 3 statements/3 exchanges", size)
	}
	if _, err := r.RuntimeTenantPage(t.Context(), "", 33); err == nil {
		t.Fatal("unbounded maintenance page accepted")
	}
}

type runtimeBudgetObjects struct {
	pool *pgxpool.Pool
	data map[string][]byte
}

func (s *runtimeBudgetObjects) StoreDetail(_ context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if s.pool.Stat().AcquiredConns() != 0 {
		return attachment.ArchiveReference{}, errors.New("runtime held a connection during object I/O")
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	s.data[ref.Key] = bytes.Clone(raw)
	return ref, err
}

func (s *runtimeBudgetObjects) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	return bytes.Clone(s.data[ref.Key]), nil
}

func TestRuntimeAdoptionWorkDoesNotGrowWithPlans(t *testing.T) {
	admin := template.Database(t)
	platform := repository(t, admin, testConfig(t))
	base := rolePool(t, admin, postgres.RuntimeRoleName, "")
	native := base.Config()
	base.Close()
	counts := &runtimeWireCounts{}
	dial := native.ConnConfig.DialFunc
	native.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &runtimeWireConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := hubserver.NewHostedRuntimeSnapshots(repo, &runtimeBudgetObjects{pool: pool, data: make(map[string][]byte)})
	if err != nil {
		t.Fatal(err)
	}
	var baselineStatements, baselineExchanges int
	for _, size := range []int{4, 100} {
		plans := hubserver.HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 1, ConnectedSeconds: 30, InvitationSeconds: 60}
		for i := range size {
			plans.Plans = append(plans.Plans, hubserver.HostedPlan{PlanReference: hubserver.PlanReference{ID: fmt.Sprintf("plan_%d", i), Version: 1}, Name: "Example", Features: []string{"collaboration"}, Allowances: map[string]int64{"api_mutations": 1000000, "ingested_events": 1000000}})
		}
		plans.Base = plans.Plans[0].PlanReference
		revision := uuid.NewString()
		if err := platform.PublishRuntime(t.Context(), revision, "entry", "example/payload", nil); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"first", "second"} {
			org := tracker.OrganizationID(fmt.Sprintf("org_plans_%d_%s", size, name))
			if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
			}); err != nil {
				t.Fatal(err)
			}
			counts.take()
			update := hubserver.HostedRuntimeUpdate{Plans: &plans, StageID: revision, Reason: "bounded catalog adoption"}
			if err := snapshots.Adopt(t.Context(), hubmaintenance.Tenant{Organization: string(org), Generation: 1}, "", update); err != nil {
				t.Fatal(err)
			}
			statements, exchanges := counts.take()
			if statements > 24 || exchanges > 24 {
				t.Fatalf("%d plans adoption: statements=%d exchanges=%d; budget=24", size, statements, exchanges)
			}
			if baselineStatements == 0 {
				baselineStatements, baselineExchanges = statements, exchanges
			}
			if statements != baselineStatements || exchanges != baselineExchanges {
				t.Fatalf("catalog work grew: statements=%d exchanges=%d baseline=%d/%d", statements, exchanges, baselineStatements, baselineExchanges)
			}
			t.Logf("tenant=%s plans=%d adoption: statements=%d exchanges=%d", org, size, statements, exchanges)
			update.StageID = uuid.NewString()
			if err := platform.PublishRuntime(t.Context(), update.StageID, "entry", "example/next-payload", nil); err != nil {
				t.Fatal(err)
			}
			if err := snapshots.Adopt(t.Context(), hubmaintenance.Tenant{Organization: string(org), Generation: 1}, revision, update); err != nil {
				t.Fatal(err)
			}
			statements, exchanges = counts.take()
			if statements != baselineStatements+1 || exchanges != baselineExchanges+1 {
				t.Fatalf("%d plans replacement grew: statements=%d exchanges=%d", size, statements, exchanges)
			}
			t.Logf("tenant=%s plans=%d replacement: statements=%d exchanges=%d", org, size, statements, exchanges)
		}
	}
}
