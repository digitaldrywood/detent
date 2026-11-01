package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestTenantServingSupervision(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		healthyFor     time.Duration
		unavailableFor time.Duration
		stalled        bool
		wait           time.Duration
		wantRestart    bool
		migrating      bool
	}{
		{name: "explicit migration progress beyond startup grace", unavailableFor: 2 * tenantStartupGrace, wait: tenantStartupGrace + time.Minute, migrating: true},
		{name: "migrating longer than the serving window", unavailableFor: 2 * time.Minute, wait: 3 * time.Minute},
		{name: "never serves within the startup grace", unavailableFor: time.Hour, wait: tenantStartupGrace + 10*time.Second, wantRestart: true},
		{name: "health request stalls during startup", unavailableFor: time.Hour, wait: tenantStartupGrace + 10*time.Second, stalled: true, wantRestart: true},
		{name: "listener lost after serving", healthyFor: time.Minute, unavailableFor: time.Minute, wantRestart: true},
		{name: "health request stalls after serving", healthyFor: time.Minute, unavailableFor: time.Minute, stalled: true, wantRestart: true},
		{name: "transient failure recovers", unavailableFor: 20 * time.Second},
		{name: "healthy child", healthyFor: 2 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started := time.Now()
				unreachable := errors.New("connection refused")
				wantErr := unreachable
				if test.stalled {
					wantErr = context.DeadlineExceeded
				}
				spec := TenantSpec{Organization: Organization{ID: "org_test"}, Check: func(ctx context.Context) error {
					elapsed := time.Since(started)
					if elapsed > test.healthyFor && elapsed < test.healthyFor+test.unavailableFor {
						if test.stalled {
							<-ctx.Done()
							return ctx.Err()
						}
						if test.migrating {
							return errTenantMigrating
						}
						return unreachable
					}
					return nil
				}}
				launcher := &ExecLauncher{Logger: slog.New(slog.DiscardHandler)}
				processResult := make(chan error, 1)
				result := make(chan error, 1)
				go func() { result <- launcher.waitTenant(ctx, spec, processResult, cancel) }()
				wait := test.wait
				if wait == 0 {
					wait = test.healthyFor + 40*time.Second
				}
				time.Sleep(wait)
				if restarted := ctx.Err() != nil; restarted != test.wantRestart {
					t.Fatalf("restart = %v, want %v", restarted, test.wantRestart)
				}
				select {
				case err := <-result:
					t.Fatalf("supervisor returned before child was reaped: %v", err)
				default:
				}
				processResult <- nil
				err := <-result
				if errors.Is(err, wantErr) != test.wantRestart {
					t.Fatalf("supervision result = %v", err)
				}
			})
		})
	}
}

func TestEntryHealthRequiresServingTenants(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	for _, test := range []struct {
		name         string
		body         string
		migrating    bool
		schema       int64
		tenantStatus int
		unreachable  bool
		want         int
	}{
		{name: "migration progress", tenantStatus: http.StatusServiceUnavailable, body: `{"schema_version":99999999999999,"migrations_running":true}`, schema: 99999999999999, migrating: true, want: http.StatusServiceUnavailable},
		{name: "migrated tenant ready", tenantStatus: http.StatusOK, body: `{"schema_version":99999999999999,"migrations_running":false}`, schema: 99999999999999, want: http.StatusOK},
		{name: "healthy", tenantStatus: http.StatusNoContent, want: http.StatusOK},
		{name: "connection refused", unreachable: true, want: http.StatusServiceUnavailable},
		{name: "tenant returns 502", tenantStatus: http.StatusBadGateway, want: http.StatusServiceUnavailable},
		{name: "tenant not ready", tenantStatus: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable},
		{name: "recovered", tenantStatus: http.StatusNoContent, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.service.config.transport = func(organization Organization) (http.RoundTripper, error) {
				if organization.ID == "org_beta" {
					if test.unreachable {
						return nil, errors.New("connection refused")
					}
					return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(test.tenantStatus)
						_, _ = io.WriteString(w, test.body)
					})}, nil
				}
				return handlerTransport{f.tenants[organization.Endpoint]}, nil
			}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
			var health struct {
				SchemaVersion     int64 `json:"schema_version"`
				MigrationsRunning bool  `json:"migrations_running"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if health.MigrationsRunning != test.migrating || (test.schema != 0 && health.SchemaVersion != test.schema) {
				t.Fatalf("migration health = %+v", health)
			}
			if response.Code != test.want || (strings.Contains(response.Body.String(), `"status":"ok"`) != (test.want == http.StatusOK)) {
				t.Fatalf("health = %d %s", response.Code, response.Body.String())
			}
		})
	}
}
