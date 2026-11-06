package hubserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryAuthBudgetContextKey struct{}

type entryAuthBudgetTrace struct{ statements atomic.Int64 }

func (t *entryAuthBudgetTrace) start(ctx context.Context, conn *pgx.Conn, statements int64) {
	active, _ := ctx.Value(entryAuthBudgetContextKey{}).(bool)
	conn.PgConn().Conn().(*entryAuthBudgetWireConn).active.Store(active)
	if active {
		t.statements.Add(statements)
	}
}
func (t *entryAuthBudgetTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.start(ctx, conn, 1)
	return ctx
}
func (*entryAuthBudgetTrace) TraceQueryEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	conn.PgConn().Conn().(*entryAuthBudgetWireConn).active.Store(false)
}
func (t *entryAuthBudgetTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	t.start(ctx, conn, int64(data.Batch.Len()))
	return ctx
}
func (*entryAuthBudgetTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (*entryAuthBudgetTrace) TraceBatchEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	conn.PgConn().Conn().(*entryAuthBudgetWireConn).active.Store(false)
}

type entryAuthBudgetWireConn struct {
	*nativeLeaseWireConn
	active atomic.Bool
	total  *atomic.Int64
}

func (c *entryAuthBudgetWireConn) Write(value []byte) (int, error) {
	before := c.exchanges.Load()
	n, err := c.nativeLeaseWireConn.Write(value)
	if c.active.Load() {
		c.total.Add(c.exchanges.Load() - before)
	}
	return n, err
}

func TestPostgresEntryAuthBudgetIndependentOfTenantSize(t *testing.T) {
	f := newSharedRouteFixture(t)
	f.requestContext = func(ctx context.Context) context.Context {
		return context.WithValue(ctx, entryAuthBudgetContextKey{}, true)
	}
	for org, service := range f.services {
		config, err := service.config.Postgres.Connection.PoolConfig()
		if err != nil {
			t.Fatal(err)
		}
		pgtransport.Configure(config)
		config.MaxConns = int32(service.config.Postgres.Pool.MaxConnections)
		trace := &entryAuthBudgetTrace{}
		config.ConnConfig.Tracer = trace
		var exchanges atomic.Int64
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &entryAuthBudgetWireConn{nativeLeaseWireConn: &nativeLeaseWireConn{Conn: conn, exchanges: &atomic.Int64{}, startup: true}, total: &exchanges}, nil
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		measured, err := postgres.NewRepository(t.Context(), pool, service.config.Postgres.Pool)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := measured.Close(); err != nil {
				t.Error(err)
			}
		})
		originalAccess, originalTenant := service.config.AccessRepository, service.config.TenantRepository
		service.config.AccessRepository, service.config.TenantRepository = measured, measured
		provider := &browserHostedProvider{members: map[string]auth.Membership{}, invitations: map[string]auth.Invitation{}, inviteRoles: map[string]string{}}
		service.config.Hosted.Provider = provider
		if _, err := provider.CreateMembership(t.Context(), org+"_owner", "provider_"+org, "owner"); err != nil {
			t.Fatal(err)
		}
		baseline := map[string][2]int64{}
		seeded := 0
		for _, size := range []int{2, 50} {
			for seeded < size {
				id := fmt.Sprintf("unrelated_%s_%d", org, seeded)
				if err := measured.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(org), ID: id, Name: id, Hash: id, Scope: "operator", Kind: "personal", ProjectAccess: "selected", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
						return err
					}
					return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: tracker.OrganizationID(org), UserID: id, Email: id + "@example.test", MembershipID: id, Role: "member", Active: true, PrincipalID: id, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				}); err != nil {
					t.Fatal(err)
				}
				seeded++
			}
			for _, route := range []struct {
				path, body string
				want       int
			}{
				{"/internal/v1/keys/admin", `{}`, http.StatusOK},
				{"/internal/v1/keys/context", `{}`, http.StatusOK},
				{"/internal/v1/owner/bootstrap", `{"organization_name":"Example"}`, http.StatusConflict},
				{"/internal/v1/invitations/accept", ``, http.StatusNoContent},
			} {
				body := route.body
				subject, email := org+"_owner", org+"_owner@example.test"
				if body == "" {
					id := fmt.Sprintf("budget_%s_%d", org, size)
					subject, email = id, id+"@example.test"
					invitation := auth.Invitation{ID: id, Email: email, OrganizationID: "provider_" + org, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
					provider.invitations[id], provider.inviteRoles[id] = invitation, "member"
					if err := measured.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
						return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: tracker.OrganizationID(org), ID: id, Email: invitation.Email, Role: "member", CreatedAt: time.Now(), ExpiresAt: invitation.ExpiresAt})
					}); err != nil {
						t.Fatal(err)
					}
					body = `{"invitation_id":"` + id + `"}`
				}
				trace.statements.Store(0)
				exchanges.Store(0)
				response := f.request(t, org, route.path, body, func(c *cloudassert.Claims) { c.Subject, c.Email = subject, email })
				requireEntryAuthResponse(t, response.Code, response.Body.String(), route.want)
				counts := [2]int64{trace.statements.Load(), exchanges.Load()}
				if counts[0] == 0 || counts[0] > 50 || counts[1] == 0 || counts[1] > 40 {
					t.Fatalf("%s size=%d statements/exchanges=%v", route.path, size, counts)
				}
				if size == 2 {
					baseline[route.path] = counts
				} else if counts != baseline[route.path] {
					t.Fatalf("%s counts grew:2x=%v50x=%v", route.path, baseline[route.path], counts)
				}
				t.Logf("%s %s size=%d statements=%d exchanges=%d", org, route.path, size, counts[0], counts[1])
			}
		}
		service.config.AccessRepository, service.config.TenantRepository = originalAccess, originalTenant
		if err := measured.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
