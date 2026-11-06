package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestEntryPostgresHealthPreservesReadinessAndReadScope(t *testing.T) {
	admin := entryPostgresDatabase(t)
	f := newPostgresTenantEntryFixture(t, entryPostgresConfig(t, admin))
	if err := f.service.closeAllocator(); err != nil {
		t.Fatal(err)
	}
	pool := f.service.config.Storage.PostgresPool
	for _, test := range []struct {
		name             string
		state            string
		tenantFailure    bool
		incompleteSchema bool
		schemaFailure    bool
		closeDatabase    bool
		want             int
		complete         bool
	}{
		{name: "ready tenants and complete schemas", want: http.StatusOK, complete: true},
		{name: "startup requested", state: "requested", want: http.StatusOK},
		{name: "startup allocating", state: "allocating", want: http.StatusOK},
		{name: "tenant unavailable", tenantFailure: true, want: http.StatusServiceUnavailable},
		{name: "tenant schema incomplete", incompleteSchema: true, want: http.StatusOK},
		{name: "platform schema unavailable", schemaFailure: true, want: http.StatusServiceUnavailable},
		{name: "database unavailable", closeDatabase: true, want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state=? WHERE id=?", "ready", "org_beta"); err != nil {
				t.Fatal(err)
			}
			if test.state != "" {
				if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state=? WHERE id=?", test.state, "org_beta"); err != nil {
					t.Fatal(err)
				}
			}
			if test.schemaFailure {
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE public.goose_db_version RENAME TO example_unavailable_schema"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx := testenv.WaitContext(t, context.WithoutCancel(t.Context()))
					if _, err := admin.ExecContext(ctx, "ALTER TABLE public.example_unavailable_schema RENAME TO goose_db_version"); err != nil {
						t.Error(err)
					}
				})
			}
			var visited sync.Map
			f.service.config.transport = func(o Organization) (http.RoundTripper, error) {
				visited.Store(o.ID, true)
				transport := handlerTransport{f.tenants[o.Endpoint]}
				if o.ID == "org_beta" {
					if test.tenantFailure {
						return nil, errors.New("tenant unavailable")
					}
					if test.incompleteSchema {
						transport = handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "application/json")
							if _, err := w.Write([]byte(`{"schema_version":1,"schema_facts_complete":false}`)); err != nil {
								t.Error(err)
							}
						})}
					}
				}
				return runtimeTransport(func(request *http.Request) (*http.Response, error) {
					if health, _ := request.Context().Value(entryHealthReadKey{}).(bool); !health {
						t.Error("public health did not propagate its read-only health scope")
					}
					return transport.RoundTrip(request)
				}), nil
			}
			if test.closeDatabase {
				if err := pool.Close(); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(t.Context()))
			for _, id := range []string{"org_alpha", "org_beta"} {
				_, called := visited.Load(id)
				wantCall := !test.closeDatabase && !(id == "org_beta" && test.state != "")
				if called != wantCall {
					t.Errorf("tenant %s probed = %v, want %v", id, called, wantCall)
				}
			}
			var health struct {
				Status                    string `json:"status"`
				SchemaFactsComplete       bool   `json:"schema_facts_complete"`
				PlatformSchemaVersion     int64  `json:"platform_schema_version"`
				PlatformSchemaFingerprint string `json:"platform_schema_fingerprint"`
				HubSchemaApplicable       bool   `json:"hub_schema_applicable"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.want || health.SchemaFactsComplete != test.complete || (health.Status == "ok") != (test.want == http.StatusOK) {
				t.Fatalf("health = %d %s", response.Code, response.Body.String())
			}
			if test.complete && (!health.HubSchemaApplicable || health.PlatformSchemaVersion == 0 || health.PlatformSchemaFingerprint == "") {
				t.Fatalf("missing authoritative schema facts: %s", response.Body.String())
			}
		})
	}
}
