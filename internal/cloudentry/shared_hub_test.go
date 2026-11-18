package cloudentry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestSharedHubTransportBindsAuthenticatedRoute(t *testing.T) {
	listener := &hubListener{incoming: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Host) })}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	host := &sharedHub{transport: &http.Transport{DialContext: listener.dial}, tenants: map[string]*servedTenant{"org_a": {generation: 2, provider: "provider_a"}, "org_b": {generation: 1, provider: "provider_b"}}}
	t.Cleanup(func() {
		host.transport.CloseIdleConnections()
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	for _, test := range []struct {
		org  Organization
		want string
	}{
		{Organization{ID: "org_a", ProviderID: "provider_a", Generation: 2}, "org_a.2"},
		{Organization{ID: "org_b", ProviderID: "provider_b", Generation: 1}, "org_b.1"},
		{Organization{ID: "org_a", ProviderID: "provider_a", Generation: 1}, ""},
	} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://tenant/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "foreign.invalid"
		resp, err := (hubRoute{host: host, organization: test.org}).RoundTrip(r)
		if test.want == "" {
			if !errors.Is(err, ErrOrganizationConflict) {
				t.Fatalf("stale generation = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		if err := errors.Join(readErr, resp.Body.Close()); err != nil || string(raw) != test.want || r.Host != "foreign.invalid" {
			t.Fatalf("routed host = %q, input host = %q, error = %v", raw, r.Host, err)
		}
	}
}

func TestSharedHubIdleViewsAfterFleetTraffic(t *testing.T) {
	h := &sharedHub{idleLimit: hubmaintenance.TenantLimit, tenants: make(map[string]*servedTenant), closing: make(map[string]chan struct{})}
	busy := &servedTenant{id: "busy", service: new(hubserver.Service)}
	h.tenants[busy.id] = busy
	h.pin(busy)
	for i := range 10_000 {
		id := strconv.Itoa(i)
		tenant := &servedTenant{id: id, service: new(hubserver.Service)}
		h.tenants[id] = tenant
		h.pin(tenant)
		for _, retired := range h.unpin(tenant, true) {
			if retired == busy {
				t.Fatal("retired an organization with an admitted request")
			}
			close(h.closing[retired.id])
			delete(h.closing, retired.id)
			h.starting.Done()
		}
	}
	if len(h.tenants) != h.idleLimit+1 || h.tenants[busy.id] != busy || h.tenants["0"] != nil || h.tenants["9999"] == nil {
		t.Fatalf("retained %d services after fleet traffic", len(h.tenants))
	}
}

func newSharedEntryFixture(t *testing.T, configure ...func(*Config)) (entryFixture, *hubmaintenance.Scheduler) {
	t.Helper()
	configs := map[string]*hubserver.Config{}
	scheduled := make(chan *hubmaintenance.Scheduler, 1)
	entryConfig := func(cfg *Config) {
		cfg.LookupEnv = entrySecretEnvironment(1, 1)
		cfg.Allocation = &AllocationConfig{TenantRoot: t.TempDir(), SocketRoot: t.TempDir(), MaxConcurrent: 1, RetryLimit: 1}
		cfg.SharedHub = &SharedHubConfig{
			Postgres:  *configs["org_alpha"].Postgres,
			Configure: func(TenantSpec) ([]byte, error) { return nil, nil },
			TenantConfig: func(_ []byte, spec TenantSpec) (hubserver.Config, error) {
				return *configs[spec.Organization.ID], nil
			},
			runMaintenance: func(scheduler *hubmaintenance.Scheduler, ctx context.Context, _ func(error)) error {
				scheduled <- scheduler
				<-ctx.Done()
				return ctx.Err()
			},
		}
	}
	f := newEntryFixtureOn(t, entrySharedDatabase(t), slog.New(slog.DiscardHandler), append([]func(*Config){entryConfig}, configure...), func(cfg *hubserver.Config) { configs[cfg.Hosted.OrganizationID] = cfg })
	for _, tenant := range f.fixtures {
		if err := tenant.Service.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return f, <-scheduled
}

func TestManagedOrganizationsShareEntryHub(t *testing.T) {
	f, scheduler := newSharedEntryFixture(t)
	if _, err := f.database.ExecContext(t.Context(), "UPDATE platform.organizations SET managed=true WHERE id = 'org_alpha'"); err != nil {
		t.Fatal(err)
	}
	health, body := newBrowser(t, f.service.Handler()).Get("/health")
	if health.StatusCode != http.StatusOK || !strings.Contains(body, `"schema_facts_complete":true`) {
		t.Fatalf("shared health returned HTTP %d without schema facts", health.StatusCode)
	}
	browsers := map[string]func() (int, string){}
	for _, tc := range []struct{ id, login string }{{"org_alpha", "user_alice:porg_alpha"}, {"org_beta", "user_alice:porg_beta"}} {
		browser := newBrowser(t, f.service.Handler())
		path := "/organizations/" + tc.id + "/app/bootstrap"
		browser.Login(path, tc.login)
		browsers[tc.id] = func() (int, string) { resp, body := browser.Get(path); return resp.StatusCode, body }
		if tc.id == "org_beta" {
			resp, body := browser.Get("/organizations/org_alpha/app/bootstrap")
			if resp.StatusCode == http.StatusOK || strings.Contains(body, "Alpha secret project") {
				t.Fatalf("unauthorized organization returned HTTP %d", resp.StatusCode)
			}
		}
	}
	for _, id := range []string{"org_alpha", "org_beta", "org_alpha", "org_beta"} {
		status, body := browsers[id]()
		if status != http.StatusOK || strings.Contains(body, "Alpha secret project") != (id == "org_alpha") || strings.Contains(body, "Beta secret project") != (id == "org_beta") {
			t.Fatalf("organization %s bootstrap returned HTTP %d or foreign projects", id, status)
		}
	}
	host := f.service.config.Allocation.Launcher.(*sharedHub)
	t.Run("maintenance release leaves other tenant metrics for its own flush", func(t *testing.T) {
		org, err := f.service.registry.Organization(t.Context(), "org_alpha")
		if err != nil {
			t.Fatal(err)
		}
		active, err := host.acquireTenant(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		host.mu.Lock()
		priorLimit := host.idleLimit
		host.idleLimit = 0
		beta := host.tenants["org_beta"]
		host.mu.Unlock()
		defer func() { host.mu.Lock(); host.idleLimit = priorLimit; host.mu.Unlock() }()
		pending := beta != nil && beta.service.PendingRequestMetrics()
		selected := hubmaintenance.Tenant{Organization: org.ID, Generation: org.Generation}
		host.release((hubmaintenance.Budget{Items: 8, Bytes: 4096}).Context(t.Context(), selected), active)
		if !pending || !beta.service.PendingRequestMetrics() {
			t.Fatal("beta metrics did not survive alpha maintenance")
		}
	})
	if status, _ := browsers["org_alpha"](); status != http.StatusOK {
		t.Fatalf("resume alpha after maintenance release: HTTP %d", status)
	}
	var mu sync.Mutex
	seen := make(map[string]bool)
	host.mu.Lock()
	for id, tenant := range host.tenants {
		feature := tenant.features["board-dirty"]
		step := feature.Step
		feature.Step = func(ctx context.Context, selected hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			if selected.Organization != id || hubmaintenance.RequireTenant(ctx, selected) != nil {
				return hubmaintenance.Outcome{}, errors.New("maintenance received a foreign organization")
			}
			result, err := step(ctx, selected, budget)
			mu.Lock()
			seen[id] = err == nil
			mu.Unlock()
			return result, err
		}
		tenant.features[feature.Name] = feature
	}
	host.mu.Unlock()
	if err := scheduler.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !seen["org_alpha"] || !seen["org_beta"] {
		t.Fatalf("shared scheduler visited %v", seen)
	}
	selected := hubmaintenance.Tenant{Organization: "org_alpha", Generation: 1}
	budget := hubmaintenance.Budget{Items: hubmaintenance.MinItems, Bytes: hubmaintenance.ByteLimit / hubmaintenance.TenantLimit}
	admitted := make(chan struct{})
	finished := make(chan error, 1)
	jobctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host.mu.Lock()
	host.tenants["org_alpha"].features["board-dirty"] = hubmaintenance.Feature{Step: func(ctx context.Context, _ hubmaintenance.Tenant, _ hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		close(admitted)
		<-ctx.Done()
		return hubmaintenance.Outcome{}, ctx.Err()
	}}
	host.mu.Unlock()
	go func() {
		_, err := host.step(budget.Context(jobctx, selected), selected, budget, "board-dirty")
		finished <- err
	}()
	select {
	case <-admitted:
	case err := <-finished:
		t.Fatalf("job exited before admission: %v", err)
	}
	host.mu.Lock()
	host.idleLimit = 1
	host.mu.Unlock()
	if status, _ := browsers["org_beta"](); status != http.StatusOK {
		t.Fatalf("other tenant while job is active: HTTP %d", status)
	}
	if err := host.Stop("org_alpha"); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped tenant job = %v", err)
	}
	for _, id := range []string{"org_alpha", "org_beta", "org_alpha"} {
		if status, _ := browsers[id](); status != http.StatusOK {
			t.Fatalf("cold %s: HTTP %d", id, status)
		}
		host.mu.Lock()
		resident := len(host.tenants)
		host.mu.Unlock()
		if resident != 1 {
			t.Fatalf("after %s, retained %d idle services", id, resident)
		}
	}
	if status, body := browsers["org_beta"](); status != http.StatusOK || !strings.Contains(body, "Beta secret project") || strings.Contains(body, "Alpha secret project") {
		t.Fatalf("remaining organization returned HTTP %d or foreign projects", status)
	}
}
