package cloudentry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type sharedRuntimeLauncher struct {
	*silentLauncher
	adopt func(context.Context, Organization, string, hubserver.HostedRuntimeUpdate) error
}

func (l *sharedRuntimeLauncher) AdoptRuntime(ctx context.Context, org Organization, expected string, update hubserver.HostedRuntimeUpdate) error {
	return l.adopt(ctx, org, expected, update)
}

func TestSharedRuntimePublicationDoesNotVisitTenants(t *testing.T) {
	f, _ := runtimePublicationFixture(t)
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE platform.organizations SET managed=true"); err != nil {
		t.Fatal(err)
	}
	plans := *f.fixtures["org_alpha"].config.Hosted.Plans
	f.service.config.Allocation = &AllocationConfig{
		TenantRoot: t.TempDir(), SocketRoot: t.TempDir(), MaxTenants: 2, MaxConcurrent: 1, RetryLimit: 5,
		Launcher: &sharedRuntimeLauncher{silentLauncher: &silentLauncher{}, adopt: func(context.Context, Organization, string, hubserver.HostedRuntimeUpdate) error {
			t.Error("publication adopted a tenant synchronously")
			return nil
		}},
	}
	f.service.config.ConfigureRuntime = nil
	next := f.service.config
	next.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &plans}
	f.service.config.Reload = func() (Config, error) { return next, nil }
	f.service.config.transport = func(Organization) (http.RoundTripper, error) {
		return nil, errors.New("shared publication must not use tenant HTTP")
	}
	if err := f.service.AdoptPublishedRuntime(t.Context(), hubmaintenance.Tenant{Organization: "org_alpha", Generation: 1}); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("missing publication did not fail closed: %v", err)
	}
	if err := f.service.reloadConfiguration(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state := f.service.currentRuntime(t.Context()); state == nil || state.Revision == "" {
		t.Fatal("desired runtime was not durable")
	}
	repo := tenantPostgresRepository(t, f.fixtures["org_alpha"])
	snapshots, err := hubserver.NewHostedRuntimeSnapshots(repo, tenantDetailFixture{&conversationArchiveFixture{}})
	if err != nil {
		t.Fatal(err)
	}
	failBeta := true
	calls := 0
	launcher := f.service.config.Allocation.Launcher.(*sharedRuntimeLauncher)
	launcher.adopt = func(ctx context.Context, org Organization, expected string, update hubserver.HostedRuntimeUpdate) error {
		calls++
		if !f.service.reloadMu.TryLock() {
			t.Fatal("publication mutex held during adoption")
		}
		f.service.reloadMu.Unlock()
		if failBeta && org.ID == "org_beta" {
			return errors.New("tenant adoption unavailable")
		}
		return snapshots.Adopt(ctx, hubmaintenance.Tenant{Organization: org.ID, Generation: org.Generation}, expected, update)
	}
	tenants, err := f.service.RuntimeTenantPage(t.Context(), "", 2)
	if err != nil || len(tenants) != 2 {
		t.Fatalf("tenant page=%+v error=%v", tenants, err)
	}
	stale := tenants[0]
	stale.Generation++
	if err := f.service.AdoptPublishedRuntime(t.Context(), stale); !errors.Is(err, corestore.ErrRevisionConflict) || calls != 0 {
		t.Fatalf("stale generation adopted: %v", err)
	}
	if err := f.service.AdoptPublishedRuntime(t.Context(), tenants[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.service.AdoptPublishedRuntime(t.Context(), tenants[1]); err == nil {
		t.Fatal("failed adoption reported success")
	}
	failBeta = false
	if err := f.service.AdoptPublishedRuntime(t.Context(), tenants[1]); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		if err := f.service.AdoptPublishedRuntime(t.Context(), tenant); err != nil || calls != 3 {
			t.Fatalf("applied revision repeated adoption: calls=%d error=%v", calls, err)
		}
	}
	plans.WindowSeconds = 480
	if err := f.service.reloadConfiguration(t.Context()); err != nil || calls != 3 {
		t.Fatalf("new desired revision visited tenants: %v", err)
	}
	if err := f.service.AdoptPublishedRuntime(t.Context(), tenants[0]); err != nil {
		t.Fatal(err)
	}
	for i, tenant := range tenants {
		update, err := snapshots.Load(t.Context(), tenant)
		want := plans.WindowSeconds
		if i == 1 {
			want = f.fixtures["org_beta"].config.Hosted.Plans.WindowSeconds
		}
		if err != nil || update.Plans.WindowSeconds != want {
			t.Fatalf("%s applied window=%+v want=%d error=%v", tenant.Organization, update.Plans, want, err)
		}
	}
	f.service.runtime.Store(nil)
	if state, err := f.service.loadRuntime(t.Context()); err != nil || state.Update.Plans.WindowSeconds != 480 {
		t.Fatalf("restart lost desired revision: %v", err)
	}
}

func runtimePublicationFixture(t *testing.T) (entryFixture, *sql.DB) {
	t.Helper()
	admin := entryPostgresDatabase(t)
	var database string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 32"); err != nil {
		t.Fatal(err)
	}
	return newEntryFixtureWithConfig(t, slog.New(slog.DiscardHandler), []func(*Config){entryPostgresConfig(t, admin)}, func(cfg *hubserver.Config) { configurePostgresTenant(t, cfg, admin) }), admin
}

func TestRuntimePublicationTenantIsolation(t *testing.T) {
	f, admin := runtimePublicationFixture(t)
	organizations := []tracker.OrganizationID{"org_alpha", "org_beta"}
	repos := make(map[tracker.OrganizationID]tenantstore.Repository)
	stage := func(org tracker.OrganizationID, id string, published bool) {
		t.Helper()
		if err := repos[org].WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StageRuntime(t.Context(), billingstore.RuntimeRevision{ID: id, Published: published, Reference: billingstore.AuditReference{Key: "example/" + string(org) + "/" + id}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	current := func(org tracker.OrganizationID, want string) {
		t.Helper()
		if err := repos[org].ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.CurrentRuntime(t.Context())
			if err == nil && got.ID != want {
				t.Fatalf("%s runtime=%s want=%s", org, got.ID, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range organizations {
		repos[org] = tenantPostgresRepository(t, f.fixtures[string(org)])
		stage(org, "initial-"+string(org), true)
		stage(org, "shared-publication", false)
		current(org, "initial-"+string(org))
	}
	if err := f.service.registry.store.db.postgres.PublishRuntime(t.Context(), "shared-publication", "entry", "example/payload", organizations); err != nil {
		t.Fatal(err)
	}
	for _, org := range organizations {
		current(org, "shared-publication")
		stage(org, "failed", false)
		current(org, "shared-publication")
	}
	stage(organizations[1], "newer", false)
	if err := f.service.registry.store.db.postgres.PublishRuntime(t.Context(), "failed", "entry", "example/failed", organizations); err == nil {
		t.Fatal("published overwritten preparation")
	}
	for _, org := range organizations {
		current(org, "shared-publication")
	}
	if err := repos[organizations[0]].ReadTenant(t.Context(), organizations[0], corestore.Serving, func(r tenantstore.Reader) error {
		published, err := r.RuntimePublished(t.Context(), "initial-org_beta")
		if published {
			t.Fatal("foreign tenant runtime visible")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.registry.store.db.ExecContext(pgtest.UnboundProbe(t.Context()), "UPDATE public.hosted_runtime_heads SET organization_id=organization_id WHERE organization_id=$1", "org_alpha"); pgtransport.SQLState(err) != "42501" {
		t.Fatalf("platform runtime head write = %v", err)
	}
	connection := f.fixtures["org_alpha"].config.Postgres.Connection
	native := entryPostgresTemplate.NativeConfig(t, admin, connection.User, connection.Password)
	runtimeDB, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeDB.Close()
	if _, err := runtimeDB.Exec(pgtest.UnboundProbe(t.Context()), "INSERT INTO hosted_runtime_publications(id) VALUES('forged')"); pgtransport.SQLState(err) != "42501" {
		t.Fatalf("tenant platform publication = %v", err)
	}
}

func TestRuntimePublicationRefreshesAllowanceConsumers(t *testing.T) {
	for _, billing := range []bool{false, true} {
		t.Run(fmt.Sprint(billing), func(t *testing.T) {
			f, _ := runtimePublicationFixture(t)
			const token = "runtime-entitlement-admin-0123456789abcdef"
			for id, tenant := range f.fixtures {
				if billing {
					tenant.config.Hosted.Billing = &hubserver.HostedBillingConfig{Mode: "test", AccountID: "acct_runtime", CustomerID: "cus_" + id, PortalConfigurationID: "bpc_runtime", WebhookSecret: []byte(testWebhookSecret), GraceSeconds: 3600, ReconcileSeconds: 3600, Provider: &fakeBilling{}, Prices: []hubserver.HostedBillingPrice{{PriceID: "price_initial", Label: "Initial plan", Plan: tenant.config.Hosted.Plans.Plans[1].PlanReference}}}
				}
			}
			entitlementFixture(t, f, token)
			f.service.config.Allocation = &AllocationConfig{TenantRoot: t.TempDir(), SocketRoot: t.TempDir(), MaxTenants: 2, MaxConcurrent: 1, RetryLimit: 5, Launcher: &ExecLauncher{}, EntitlementAdminToken: []byte(token)}
			f.service.config.ConfigureRuntime = func(update hubserver.HostedRuntimeUpdate) func(TenantSpec) ([]byte, error) {
				return func(TenantSpec) ([]byte, error) { return json.Marshal(update) }
			}
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE platform.organizations SET managed=true"); err != nil {
				t.Fatal(err)
			}
			plans := *f.fixtures["org_alpha"].config.Hosted.Plans
			next := f.service.config
			next.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &plans}
			if billing {
				next.Runtime.Prices = f.fixtures["org_alpha"].config.Hosted.Billing.Prices
			}
			f.service.config.Reload = func() (Config, error) { return next, nil }
			readers := make(map[string]func() int64)
			for _, id := range []string{"org_alpha", "org_beta"} {
				org, err := f.service.registry.Organization(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				repo := tenantPostgresRepository(t, f.fixtures[id])
				publisher := "artifact-publisher-test-token-" + id
				credentialID := "publisher_" + id
				binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "hosted", HostedOptIn: true, PublisherTokenID: credentialID}
				err = repo.WriteTenant(t.Context(), tracker.OrganizationID(id), corestore.Serving, func(w tenantstore.Writer) error {
					projects, err := w.Projects(t.Context(), "", 10)
					if err != nil {
						return err
					}
					if len(projects) != 1 {
						return fmt.Errorf("fixture projects=%d", len(projects))
					}
					now := time.Now()
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(id), ID: credentialID, Name: "Example publisher", Hash: apikey.HashToken(publisher), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(id), TokenID: credentialID, ProjectID: projects[0].ID}); err != nil {
						return err
					}
					return w.BindArtifactService(t.Context(), projects[0].ID, binding)
				})
				if err != nil {
					t.Fatal(err)
				}
				readers[id] = func() int64 {
					t.Helper()
					raw, err := json.Marshal(artifact.Usage{ObservedAt: time.Now()})
					if err != nil {
						t.Fatal(err)
					}
					status, body, err := f.service.machineCall(t.Context(), org, http.MethodPost, "/api/v2/organizations/"+id+"/artifact-allowances/"+binding.ServiceID, raw, publisher)
					if err != nil || status != http.StatusOK {
						t.Fatalf("allowance %s: status=%d error=%v body=%s", id, status, err, body)
					}
					var limits artifact.Limits
					if err := json.Unmarshal(body, &limits); err != nil {
						t.Fatal(err)
					}
					return limits.WindowSeconds
				}
				if got := readers[id](); got != 3600 {
					t.Fatalf("initial allowance window=%d", got)
				}
			}
			plans.WindowSeconds = 240
			if err := f.service.reloadConfiguration(t.Context()); err != nil {
				t.Fatal(err)
			}
			for id, read := range readers {
				if got := read(); got != 240 {
					t.Errorf("%s allowance window after publication=%d, want 240", id, got)
				}
			}
			beta, err := f.service.registry.Organization(t.Context(), "org_beta")
			if err != nil {
				t.Fatal(err)
			}
			local := plans
			local.WindowSeconds = 600
			update := hubserver.HostedRuntimeUpdate{Plans: &local, Prices: next.Runtime.Prices, Reason: "tenant override before adoption"}
			if err := f.service.changeTenantRuntime(t.Context(), beta, &update); err != nil {
				t.Fatal(err)
			}
			if got := readers["org_beta"](); got != 600 {
				t.Fatalf("local window=%d", got)
			}
			if err := f.service.publishTenantRuntime(t.Context(), beta); err != nil {
				t.Fatal(err)
			}
			if got := readers["org_beta"](); got != 240 {
				t.Errorf("joining tenant allowance window=%d, want 240", got)
			}
			originalTransport := f.service.config.transport
			refreshes := make(map[string]int)
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			f.service.config.transport = func(org Organization) (http.RoundTripper, error) {
				transport, err := originalTransport(org)
				if err != nil {
					return nil, err
				}
				return runtimeTransport(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/entitlements/runtime") {
						refreshes[org.ID]++
						if f.service.reloadMu.TryLock() {
							f.service.reloadMu.Unlock()
						} else {
							t.Error("publication lock held during refresh notification")
						}
						if org.ID == "org_alpha" {
							return nil, errors.New("tenant refresh unavailable")
						}
					}
					return transport.RoundTrip(request)
				}), nil
			}
			plans.WindowSeconds = 480
			if err := f.service.reloadConfiguration(t.Context()); err != nil {
				t.Fatalf("notification failure failed publication: %v", err)
			}
			if got := f.service.currentRuntime(t.Context()).Update.Plans.WindowSeconds; got != 480 {
				t.Fatalf("entry publication window=%d", got)
			}
			for id, want := range map[string]int64{"org_alpha": 240, "org_beta": 480} {
				if got := readers[id](); got != want {
					t.Errorf("%s window=%d want=%d", id, got, want)
				}
				if refreshes[id] != 1 {
					t.Errorf("%s refresh calls=%d", id, refreshes[id])
				}
			}
			if !strings.Contains(logs.String(), "published runtime tenant refresh failed") {
				t.Error("refresh failure was not logged")
			}
			f.service.config.transport = originalTransport
			plans.WindowSeconds = 600
			if err := f.service.reloadConfiguration(t.Context()); err != nil {
				t.Fatal(err)
			}
			for id, read := range readers {
				if got := read(); got != 600 {
					t.Errorf("%s did not refresh on next publication: %d", id, got)
				}
			}

		})
	}
}
