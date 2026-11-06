package hubserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type leaseTransactionKey struct{}

type leaseTransactionCounter struct {
	tenantstore.Repository
	reads, writes int
}

func (r *leaseTransactionCounter) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if ctx.Value(leaseTransactionKey{}) == true {
		r.reads++
	}
	return r.Repository.ReadTenant(ctx, org, workload, read)
}
func (r *leaseTransactionCounter) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if ctx.Value(leaseTransactionKey{}) == true {
		r.writes++
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

func TestLeaseRenewalReturnsItsCommittedSnapshot(t *testing.T) {
	f := newRunnerPGFixture(t)
	credential, status, err := f.service.authenticateAccessHash(t.Context(), apikey.HashToken(f.tokens[0]), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authentication: status=%d err=%v", status, err)
	}
	service := &Service{config: f.service.config}
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	config, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	var exchanges atomic.Int64
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &nativeLeaseWireConn{Conn: conn, exchanges: &exchanges, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	measured, err := postgres.NewRepository(t.Context(), pool, f.service.config.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := measured.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, rich := range []bool{false, true} {
		t.Run(fmt.Sprintf("rich_%t", rich), func(t *testing.T) {
			var reservation providercapacity.Reservation
			if rich {
				report := providercapacity.Report{Provider: "example-provider", Backend: "example-backend", AccountAlias: "example-account", SharedAccountAlias: "example-shared", Billing: "subscription", MaxConcurrent: 1, Availability: "available", ObservedAt: time.Now().UTC().Truncate(time.Microsecond)}
				for index := 0; index < 128; index++ {
					id := fmt.Sprintf("model_%03d", 127-index)
					report.Models = append(report.Models, id)
					detail := providercapacity.ModelDetail{ID: id, Label: "Example", Provider: report.Provider, Default: index == 0, DefaultReasoningEffort: "effort_00", Legacy: index%2 == 0}
					for effort := 0; effort < 16; effort++ {
						detail.ReasoningEfforts = append(detail.ReasoningEfforts, fmt.Sprintf("effort_%02d", effort))
					}
					report.ModelDetails = append(report.ModelDetails, detail)
				}
				for index, kind := range []string{"primary", "secondary", "five_hour", "weekly", "weekly_opus", "weekly_sonnet", "seven_day", "seven_day_opus"} {
					window := providercapacity.UsageWindow{Kind: kind}
					if index > 0 {
						percent := float64(index)
						window.PercentUsed = &percent
						window.ObservedAt = report.ObservedAt
						window.ResetAt = report.ObservedAt.Add(time.Hour)
					}
					report.UsageWindows = append(report.UsageWindows, window)
				}
				reservation = providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: report.Backend, Model: report.Models[0]}, Report: report, Reason: "available"}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: "org_startup", LeaseID: f.leases[0].ID, Reservation: reservation})
				}); err != nil {
					t.Fatal(err)
				}
			}
			counter := &leaseTransactionCounter{Repository: measured}
			service.config.TenantRepository = counter
			response := httptest.NewRecorder()
			request := newHubAPIRequest(t, http.MethodPost, "/renew", f.tokens[0], tracker.NativeLeaseMutation{FencingToken: f.leases[0].FencingToken, TTLSeconds: 300})
			var statements []string
			ctx := postgres.WithTransactionTiming(context.WithValue(t.Context(), leaseTransactionKey{}, true), nil, func(_ time.Time, query string) { statements = append(statements, query) })
			request = request.WithContext(ctx)
			c := echo.New().NewContext(request, response)
			c.SetParamNames("lease")
			c.SetParamValues(string(f.leases[0].ID))
			c.Set("native_scope", nativeScope{organization: "org_startup", project: "prj_runner", credential: credential})
			exchanges.Store(0)
			if err := service.renewNativeLeaseTenant(c); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, response, http.StatusOK)
			var renewed tracker.NativeLease
			decodeHubResponse(t, response, &renewed)
			if renewed.ID != f.leases[0].ID || renewed.FencingToken != f.leases[0].FencingToken || renewed.PolicyID != f.policy.PolicyID || renewed.IsolationPolicy == nil {
				t.Fatalf("renewal lost authority or response details: %+v", renewed)
			}
			if got := exchanges.Load(); got <= 0 || got > 8 {
				t.Errorf("renew command wire exchanges=%d, want 1..8 including BEGIN/binding/COMMIT", got)
			}
			t.Logf("renew command statements excluding BEGIN/binding=%d wire exchanges=%d", len(statements), exchanges.Load())

			if counter.reads != 0 || counter.writes != 1 {
				t.Fatalf("renewal transactions read=%d write=%d, want 0 and 1", counter.reads, counter.writes)
			}
			if rich && !reflect.DeepEqual(renewed.ProviderReservation, &reservation) {
				t.Fatalf("renewal changed provider snapshot: got=%+v", renewed.ProviderReservation)
			}
		})
	}

}

func TestLeaseCommandLocksRunnerBeforeFence(t *testing.T) {
	for _, operation := range []string{"renew", "release"} {
		t.Run(operation, func(t *testing.T) {
			f := newRunnerPGFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			credential, status, err := f.service.authenticateAccessHash(ctx, apikey.HashToken(f.tokens[0]), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatal(status, err)
			}
			blocker, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id='org_startup' AND runner_id=$1 FOR UPDATE`, f.bindings[0].RunnerID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				response := httptest.NewRecorder()
				request := newHubAPIRequest(t, http.MethodPost, "/"+operation, f.tokens[0], tracker.NativeLeaseMutation{FencingToken: f.leases[0].FencingToken, TTLSeconds: 300, Reason: "released"}).WithContext(ctx)
				c := echo.New().NewContext(request, response)
				c.SetParamNames("lease")
				c.SetParamValues(string(f.leases[0].ID))
				c.Set("native_scope", nativeScope{organization: "org_startup", project: "prj_runner", credential: credential})
				var err error
				wantStatus := http.StatusOK
				if operation == "renew" {
					err = f.service.renewNativeLeaseTenant(c)
				} else {
					err = f.service.releaseNativeLeaseTenant(c)
					wantStatus = http.StatusNoContent
				}
				if err == nil && response.Code != wantStatus {
					err = fmt.Errorf("lease command status=%d, want %d", response.Code, wantStatus)
				}
				result <- err
			}()
			t.Cleanup(func() { cancel(); _ = blocker.Rollback(); <-done })
			testenv.WaitUntil(t, ctx, func() error {
				select {
				case err := <-result:
					t.Fatalf("command finished before runner lock: %v", err)
				default:
				}
				var waiting bool
				if err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%LockLeaseMutationRunner%')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("lease command has not reached runner lock")
				}
				return nil
			})
			if _, err := blocker.ExecContext(ctx, `SELECT lease_id FROM leases WHERE organization_id='org_startup' AND lease_id=$1 FOR UPDATE NOWAIT`, string(f.leases[0].ID)); err != nil {
				t.Fatalf("lease was locked before its runner: %v", err)
			}
			if err := blocker.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

type nativeLeaseWireConn struct {
	net.Conn
	exchanges *atomic.Int64
	pending   []byte
	startup   bool
}

func (c *nativeLeaseWireConn) Write(value []byte) (int, error) {
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
		if c.pending[0] == 'S' || c.pending[0] == 'Q' {
			c.exchanges.Add(1)
		}
		c.pending = c.pending[size:]
	}
	return n, err
}
