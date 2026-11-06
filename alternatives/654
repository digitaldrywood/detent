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
		name          string
		body          string
		migrating     bool
		schema        int64
		tenantStatus  int
		unreachable   bool
		want          int
		factsComplete bool
	}{
		{name: "migration progress", tenantStatus: http.StatusServiceUnavailable, body: `{"schema_version":99999999999999,"migrations_running":true}`, schema: 99999999999999, migrating: true, want: http.StatusServiceUnavailable},
		{name: "migrated tenant ready", tenantStatus: http.StatusOK, body: `{"schema_version":99999999999999,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","migrations_running":false,"schema_facts_complete":true}`, schema: 99999999999999, want: http.StatusOK, factsComplete: true},
		{name: "unobserved tenant migration keeps schema facts incomplete", tenantStatus: http.StatusServiceUnavailable, body: `{"schema_version":100,"migrations_running":false,"schema_facts_complete":false}`, want: http.StatusServiceUnavailable},
		{name: "healthy", tenantStatus: http.StatusNoContent, want: http.StatusOK},
		{name: "connection refused", unreachable: true, want: http.StatusServiceUnavailable},
		{name: "tenant returns 502", tenantStatus: http.StatusBadGateway, want: http.StatusServiceUnavailable},
		{name: "tenant not ready", tenantStatus: http.StatusServiceUnavailable, want: http.StatusServiceUnavailable},
		{name: "recovered", tenantStatus: http.StatusNoContent, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.service.config.transport = func(organization Organization) (http.RoundTripper, error) {
				if test.factsComplete {
					return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(test.tenantStatus)
						_, _ = io.WriteString(w, test.body)
					})}, nil
				}
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
				SchemaVersion       int64 `json:"schema_version"`
				MigrationsRunning   bool  `json:"migrations_running"`
				SchemaFactsComplete bool  `json:"schema_facts_complete"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if health.MigrationsRunning != test.migrating || health.SchemaFactsComplete != test.factsComplete || (test.schema != 0 && health.SchemaVersion != test.schema) {
				t.Fatalf("migration health = %+v", health)
			}
			if response.Code != test.want || (strings.Contains(response.Body.String(), `"status":"ok"`) != (test.want == http.StatusOK)) {
				t.Fatalf("health = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEntryHealthCompletesWithoutTenantDatabases(t *testing.T) {
	t.Parallel()
	admin := entryPostgresDatabase(t)
	f := newEntryFixtureWithConfig(t, nil, []func(*Config){entryPostgresConfig(t, admin)})
	if _, err := admin.ExecContext(t.Context(), "TRUNCATE platform.organizations CASCADE"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	var health struct {
		SchemaVersion            int64  `json:"schema_version"`
		HubSchemaApplicable      bool   `json:"hub_schema_applicable"`
		RegistrySchemaVersion    int64  `json:"registry_schema_version"`
		RegistrySchemaApplicable bool   `json:"registry_schema_applicable"`
		AuthSchemaVersion        int64  `json:"auth_schema_version"`
		AuthSchemaApplicable     bool   `json:"auth_schema_applicable"`
		PlatformSchemaVersion    int64  `json:"platform_schema_version"`
		PlatformSchemaApplicable bool   `json:"platform_schema_applicable"`
		SchemaFactsComplete      bool   `json:"schema_facts_complete"`
		Status                   string `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || health.Status != "ok" || !health.SchemaFactsComplete || health.HubSchemaApplicable || health.SchemaVersion != 0 || health.RegistrySchemaApplicable || health.RegistrySchemaVersion != 0 || health.AuthSchemaApplicable || health.AuthSchemaVersion != 0 || !health.PlatformSchemaApplicable || health.PlatformSchemaVersion == 0 {
		t.Fatalf("empty registry health = %d %+v", response.Code, health)
	}
}

func TestEntryHealthLifecycleApplicability(t *testing.T) {
	for _, test := range []struct {
		name          string
		state         string
		applicable    bool
		factsComplete bool
	}{
		{name: "requested tenant remains incomplete", state: "requested", applicable: true},
		{name: "allocating tenant remains incomplete", state: "allocating", applicable: true},
		{name: "failed tenant is not an active Hub child", state: "failed", factsComplete: true},
		{name: "disabled tenant is not an active Hub child", state: "disabled", factsComplete: true},
		{name: "deleting tenant is not an active Hub child", state: "deleting", factsComplete: true},
		{name: "deleted tenant does not require a Hub schema", state: "deleted", factsComplete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t)
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), `UPDATE organizations SET state=?`, test.state); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
			var health struct {
				HubSchemaApplicable bool   `json:"hub_schema_applicable"`
				SchemaFactsComplete bool   `json:"schema_facts_complete"`
				SchemaVersion       int64  `json:"schema_version"`
				Status              string `json:"status"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || health.Status != "ok" || health.HubSchemaApplicable != test.applicable || health.SchemaFactsComplete != test.factsComplete {
				t.Fatalf("state %s health = %d %+v", test.state, response.Code, health)
			}
			if !test.applicable && health.SchemaVersion != 0 {
				t.Fatalf("state %s has non-applicable Hub schema version %d", test.state, health.SchemaVersion)
			}
		})
	}
}
