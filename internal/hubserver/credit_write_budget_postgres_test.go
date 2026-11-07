package hubserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func TestPostgresCreditWritesStayBoundedWithHistory(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	f.requestContext = func(ctx context.Context) context.Context {
		return context.WithValue(ctx, entryAuthBudgetContextKey{}, true)
	}
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			service := f.services[org]
			service.stopHostedBilling()
			done := make(chan struct{})
			close(done)
			service.billing = &hostedBillingWorker{service: service, cancel: func() {}, done: done}
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
			oldAccess, oldTenant := service.config.AccessRepository, service.config.TenantRepository
			oldBilling, oldPostgres := service.database.billingRepository, service.database.postgres
			service.config.AccessRepository, service.config.TenantRepository = measured, measured
			service.database.billingRepository, service.database.postgres = measured, measured
			t.Cleanup(func() {
				service.config.AccessRepository, service.config.TenantRepository = oldAccess, oldTenant
				service.database.billingRepository, service.database.postgres = oldBilling, oldPostgres
				if err := measured.Close(); err != nil {
					t.Error(err)
				}
			})
			provider := service.config.Hosted.Billing.Provider.(*hostedCreditProvider)
			provider.savedMethod = "pm_example"
			provider.duringCheckout = func(context.Context, billing.CreditRequest) error {
				if got := pool.Stat().AcquiredConns(); got != 0 {
					return fmt.Errorf("provider I/O held %d database connections", got)
				}
				return nil
			}
			warm := f.request(t, org, "/api/v2/organizations/"+org+"/billing/credits/checkout", `{"price":"price_credit","idempotency_key":"warmup"}`, sharedRouteBrowserClaims, map[string]string{"X-CSRF-Token": strings.Repeat("b", 64)})
			requireNativeStatus(t, warm, http.StatusOK)
			baseline := map[string][2]int64{}
			seeded := 0
			for _, size := range []int{2, 50} {
				for seeded < size {
					if err := service.database.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
						_, err := w.PostBillingCredit(t.Context(), billingstore.CreditTransaction{OrganizationID: service.database.hostedOrganization, Mode: billing.ModeTest, Source: fmt.Sprintf("history_%d", seeded), Kind: "purchase", AmountMicros: 1, RecordedAt: service.config.now()})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					seeded++
				}
				for _, route := range []struct {
					name, method, body string
					status             int
				}{
					{"checkout", http.MethodPost, fmt.Sprintf(`{"price":"price_credit","idempotency_key":"size_%d"}`, size), http.StatusOK},
					{"auto-fund", http.MethodPut, `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`, http.StatusNoContent},
					{"events", http.MethodPost, fmt.Sprintf(`{"event_id":"evt_size_%d","event_type":"checkout.session.completed","customer_id":%q}`, size, "cus_"+org), http.StatusNoContent},
				} {
					trace.statements.Store(0)
					exchanges.Store(0)
					path := "/api/v2/organizations/" + org + "/billing/credits/" + route.name
					var claims func(*cloudassert.Claims)
					if route.name == "events" {
						path = "/internal/v1/billing/events"
					} else {
						claims = func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Method = route.method }
					}
					response := f.request(t, org, path, route.body, claims, map[string]string{"X-CSRF-Token": strings.Repeat("b", 64)})
					requireNativeStatus(t, response, route.status)
					counts := [2]int64{trace.statements.Load(), exchanges.Load()}
					if counts[0] == 0 || counts[1] == 0 {
						t.Fatal("request database traffic was not measured")
					}
					if size == 2 {
						baseline[route.name] = counts
					} else if counts != baseline[route.name] {
						t.Errorf("%s statement/exchange counts grew: 2x=%v 50x=%v", route.name, baseline[route.name], counts)
					}
					t.Logf("%s history=%d statements=%d exchanges=%d", route.name, size, counts[0], counts[1])
				}
			}
		})
	}
}
