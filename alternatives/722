//go:build !windows

package cloudentry

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
)

const provisioningFixtureTimeout = 2 * time.Minute

type processLauncher struct {
	configureDatabase func(*hubserver.Config)
	databases         map[string]*sql.DB
	billing           func() *hubserver.HostedBillingConfig
	plans             *hubserver.HostedPlansConfig
	provider          *fakeProvider
	key               ed25519.PrivateKey
	mu                sync.Mutex
	running           map[string]func()
	failures          map[string]error
	transports        map[string]*http.Transport
	useUnix           bool
	starts            int
	checks            map[string]func(context.Context) error
}

func (l *processLauncher) Start(_ context.Context, spec TenantSpec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.running[spec.Organization.ID]; ok {
		return nil
	}
	public, err := cloudassert.ParsePublicKey(spec.PublicKey)
	if err != nil {
		return err
	}
	config := hubserver.Config{
		GitHubDisabled:    true,
		InitialAdminToken: []byte(testAdminKey), Logger: slog.New(slog.DiscardHandler),
		Hosted: &hubserver.HostedConfig{
			OrganizationID: spec.Organization.ID, WorkOSOrganizationID: spec.Organization.ProviderID, PublicURL: spec.PublicURL, Provider: l.provider,
			StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"},
			SharedEntry: &hubserver.HostedSharedEntry{Issuer: spec.Issuer, PublicKeys: []ed25519.PublicKey{public}, Generation: spec.Organization.Generation, ResolveKey: spec.ResolveKey, ArchiveContents: spec.ArchiveContents, Content: spec.Content, RunDetails: spec.RunDetails, Icons: spec.Icons, ReadArchive: spec.ReadArchive},
			Plans:       l.plans,
		},
	}
	if spec.Organization.CheckoutPrice != "" {
		config.Hosted.Plans = hubserver.CheckoutHostedPlans(config.Hosted.Plans)
	}
	if l.billing != nil {
		config.Hosted.Billing = l.billing()
	}
	id := spec.Organization.ID
	delete(l.failures, id)
	if l.checks == nil {
		l.checks = make(map[string]func(context.Context) error)
	}
	l.checks[id] = spec.Check
	l.configureDatabase(&config)
	if l.useUnix {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		config.ListenAddress = "unix:" + spec.Socket
		go func() {
			defer close(done)
			if err := hubserver.Run(ctx, config); err != nil && ctx.Err() == nil {
				l.mu.Lock()
				if l.failures == nil {
					l.failures = map[string]error{}
				}
				l.failures[id] = err
				l.mu.Unlock()
			}
		}()
		l.starts++
		l.running[id] = func() { cancel(); <-done }
		return nil
	}
	service, err := hubserver.Open(context.Background(), config)
	if err != nil {
		return err
	}
	server := httptest.NewServer(service.Handler())
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	l.starts++
	l.transports[id] = transport
	//nolint:contextcheck // The fixture server and service expose context-free close methods.
	l.running[id] = func() {
		transport.CloseIdleConnections()
		server.CloseClientConnections()
		server.Close()
		_ = service.Close()
	}
	return nil
}

func (l *processLauncher) transport(organization Organization) (http.RoundTripper, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	transport := l.transports[organization.ID]
	if transport == nil {
		return nil, errors.New("tenant fixture is not running")
	}
	return transport, nil
}

func (l *processLauncher) WaitReady(ctx context.Context, id string) error {
	l.mu.Lock()
	check := l.checks[id]
	running := l.running[id] != nil
	l.mu.Unlock()
	if !running || check == nil {
		return errors.New("tenant fixture is not running")
	}
	for {
		if err := check(ctx); err == nil {
			return nil
		}
		timer := time.NewTimer(tenantProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *processLauncher) Failure(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failures[id]
}

func (l *processLauncher) Stop(id string) error {
	l.mu.Lock()
	stop, ok := l.running[id]
	delete(l.running, id)
	delete(l.transports, id)
	delete(l.checks, id)
	l.mu.Unlock()
	if ok {
		stop()
	}
	return nil
}

func (l *processLauncher) Close() error {
	l.mu.Lock()
	ids := make([]string, 0, len(l.running))
	for id := range l.running {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	for _, id := range ids {
		_ = l.Stop(id)
	}
	return nil
}

type provisioningFixture struct {
	configureEntry []func(*Config)
	objects        *spacesFixture
	billing        *BillingConfig
	service        *Service
	provider       *fakeProvider
	launcher       *processLauncher
	state          string
	roots          [2]string
	key            ed25519.PrivateKey
	timeout        time.Duration
	now            func() time.Time
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func socketFixtureFits(root string) bool {
	path := filepath.Join(root, "s", "org_"+strings.Repeat("f", 20)+".sock")
	return len(path) < len(syscall.RawSockaddrUnix{}.Path)
}

func newProvisioningFixture(t *testing.T, maxTenants int, mutate func(*AllocationConfig), configureEntry ...func(*Config)) *provisioningFixture {
	return newProvisioningFixtureWith(t, maxTenants, mutate, nil, configureEntry...)
}

func newProvisioningFixtureWith(t *testing.T, maxTenants int, mutate func(*AllocationConfig), configure func(*provisioningFixture), configureEntry ...func(*Config)) *provisioningFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("PostgreSQL tenant provisioning integration")
	}

	seed := make([]byte, ed25519.SeedSize)
	seed[1] = 7
	root := shortTempDir(t)
	f := &provisioningFixture{provider: newFakeProvider(), state: t.TempDir(), roots: [2]string{filepath.Join(root, "t"), filepath.Join(root, "s")}, key: ed25519.NewKeyFromSeed(seed), timeout: provisioningFixtureTimeout}
	for _, user := range []string{"member", "eve", "fay"} {
		f.provider.users["user_"+user] = user + "@example.test"
	}
	databases := make(map[string]*sql.DB)
	f.launcher = &processLauncher{configureDatabase: configurePostgresTenants(t, databases), databases: databases, provider: f.provider, key: f.key, plans: capacityPlans(), running: map[string]func(){}, transports: map[string]*http.Transport{}, useUnix: socketFixtureFits(root)}
	if configure != nil {
		configure(f)
	}
	f.objects = newSpacesFixture(t, false)
	f.configureEntry = append([]func(*Config){entryPostgresConfig(t)}, configureEntry...)
	f.open(t, maxTenants, mutate)
	return f
}

func (f *provisioningFixture) open(t *testing.T, maxTenants int, mutate func(*AllocationConfig)) {
	t.Helper()
	allocation := &AllocationConfig{TenantRoot: f.roots[0], SocketRoot: f.roots[1], MaxTenants: maxTenants, MaxConcurrent: 2, RetryLimit: 3, Launcher: f.launcher}
	if mutate != nil {
		mutate(allocation)
	}
	config := Config{Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: f.key, Provider: f.provider,
		StaffEmails: []string{"staff@example.test"}, StateDir: f.state, Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{}, Allocation: allocation, Billing: f.billing, tenantStartTimeout: f.timeout, now: f.now}
	if !f.launcher.useUnix {
		config.transport = f.launcher.transport
	}
	storage := f.objects.config()
	storage.Endpoint = "https://storage.example.test"
	config.Attachments, config.attachmentTransport = &storage, f.objects.transport
	for _, configure := range f.configureEntry {
		configure(&config)
	}
	service, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	t.Cleanup(func() { _ = f.service.Close() })
}

func (f *provisioningFixture) create(t *testing.T, b *browser, name string) page {
	t.Helper()
	response, body := b.get("/organizations/new")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("new organization page = %d", response.StatusCode)
	}
	_, rest, _ := strings.Cut(body, `name="creation_key" value="`)
	key, _, _ := strings.Cut(rest, `"`)
	return b.do(http.MethodPost, "/organizations", url.Values{"name": {name}, "creation_key": {key}, "csrf": {csrfFrom(t, body)}}, nil)
}

func (f *provisioningFixture) waitState(t *testing.T, id string, states ...string) Organization {
	t.Helper()
	var organization Organization
	testenv.WaitUntil(t, t.Context(), func() error {
		var err error
		organization, err = f.service.registry.Organization(t.Context(), id)
		if err != nil {
			return err
		}
		for _, state := range states {
			if organization.State == state {
				return nil
			}
		}
		return fmt.Errorf("organization state = %s, want %v", organization.State, states)
	})
	return organization
}

func organizationFromLocation(t *testing.T, location string) string {
	t.Helper()
	id, ok := strings.CutPrefix(location, "/organizations/")
	id, _, _ = strings.Cut(id, "/")
	if !ok || !ValidOrganizationID(id) {
		t.Fatalf("location = %q", location)
	}
	return id
}

func TestProvisioningSelfServiceJourney(t *testing.T) {
	for _, backend := range []string{"postgres"} {
		t.Run(backend, func(t *testing.T) { testProvisioningSelfServiceJourney(t, entryBackendConfig(t, backend)) })
	}
}

func testProvisioningSelfServiceJourney(t *testing.T, configureStorage func(*Config)) {
	if testing.Short() {
		t.Skip("live service integration")
	}

	t.Parallel()
	f := newProvisioningFixture(t, 3, nil, configureStorage)
	member := newBrowser(t, f.service.Handler())
	member.login("/auth/oidc/start", "user_member:")
	_, chooser := member.get("/organizations")
	if !strings.Contains(chooser, `href="/organizations/new"`) {
		t.Fatal("chooser does not offer organization creation")
	}
	response, body := member.get("/organizations/new")
	_, rest, _ := strings.Cut(body, `name="creation_key" value="`)
	key, _, _ := strings.Cut(rest, `"`)
	form := url.Values{"name": {"Delta"}, "creation_key": {key}, "csrf": {csrfFrom(t, body)}}
	first := member.do(http.MethodPost, "/organizations", form, nil)
	if response.StatusCode != http.StatusOK || first.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d: %s", first.StatusCode, first.Body)
	}
	id := organizationFromLocation(t, first.Header.Get("Location"))
	if again := member.do(http.MethodPost, "/organizations", form, nil); again.Header.Get("Location") != first.Header.Get("Location") {
		t.Fatalf("retried intent created %q, want %q", again.Header.Get("Location"), first.Header.Get("Location"))
	}
	changed := url.Values{"name": {"Other"}, "creation_key": {key}, "csrf": form["csrf"]}
	if conflict := member.do(http.MethodPost, "/organizations", changed, nil); conflict.StatusCode != http.StatusConflict {
		t.Fatalf("changed intent status = %d", conflict.StatusCode)
	}
	ready := f.waitState(t, id, "ready")
	if f.provider.creates != 1 || ready.ProviderID != "porg_"+id || ready.Generation != 1 {
		t.Fatalf("provider creates = %d, organization = %+v", f.provider.creates, ready)
	}
	if status, _ := member.get("/organizations/" + id + "/provisioning"); status.StatusCode != http.StatusSeeOther {
		t.Fatalf("ready provisioning page = %d", status.StatusCode)
	}
	member.login("/organizations/"+id+"/organization", "user_member:porg_"+id)
	response, body = member.get("/organizations/" + id + "/app/bootstrap")
	if response.StatusCode == http.StatusPermanentRedirect {
		response, body = member.get(response.Header.Get("Location"))
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owner page = %d %s", response.StatusCode, body)
	}
	if quota := f.create(t, member, "Second"); quota.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second organization without a paid plan status = %d", quota.StatusCode)
	}
	eve := newBrowser(t, f.service.Handler())
	eve.login("/auth/oidc/start", "user_eve:")
	if peek, _ := eve.get("/organizations/" + id + "/provisioning"); peek.StatusCode != http.StatusNotFound {
		t.Fatalf("another user saw provisioning status: %d", peek.StatusCode)
	}
	if denied, _ := eve.get("/organizations/" + id + "/delete"); denied.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner delete page = %d", denied.StatusCode)
	}
	_, deletePage := member.get("/organizations/" + id + "/delete")
	if !strings.Contains(deletePage, "Delta") {
		t.Fatal("delete confirmation does not identify the organization")
	}
	if wrong := member.do(http.MethodPost, "/organizations/"+id+"/delete", url.Values{"confirm": {"delta"}, "csrf": {csrfFrom(t, deletePage)}}, nil); wrong.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed delete = %d", wrong.StatusCode)
	}
	deleted := member.do(http.MethodPost, "/organizations/"+id+"/delete", url.Values{"confirm": {"Delta"}, "csrf": {csrfFrom(t, deletePage)}}, nil)
	if deleted.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d: %s", deleted.StatusCode, deleted.Body)
	}
	f.waitState(t, id, "deleted")
	if gone, _ := member.get("/organizations/" + id + "/app/bootstrap"); gone.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted organization still routes: %d", gone.StatusCode)
	}
	var retained int
	if err := f.launcher.databases[id].QueryRowContext(t.Context(), "SELECT count(*) FROM public.organizations WHERE organization_id=$1", id).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("deletion retained tenant data = %d, %v", retained, err)
	}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state = 'ready' WHERE id = ?", id); err == nil {
		t.Fatal("a deleted organization was resurrected")
	}
}

func TestProvisioningCapacityAndRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("live service integration")
	}

	t.Parallel()
	f := newProvisioningFixture(t, 1, func(a *AllocationConfig) { a.RetryLimit = 1 })
	member := newBrowser(t, f.service.Handler())
	member.login("/auth/oidc/start", "user_member:")
	f.provider.mu.Lock()
	f.provider.createFailures = 1
	f.provider.mu.Unlock()
	created := f.create(t, member, "Delta")
	id := organizationFromLocation(t, created.Header.Get("Location"))
	failed := f.waitState(t, id, "failed")
	if failed.ErrorCode != "provider_organization_failed" {
		t.Fatalf("failure = %+v", failed)
	}
	_, status := member.get("/organizations/" + id + "/provisioning")
	if !strings.Contains(status, "Resume setup") {
		t.Fatalf("failed status page = %s", status)
	}
	if resumed := member.do(http.MethodPost, "/organizations/"+id+"/provisioning/resume", url.Values{"csrf": {csrfFrom(t, status)}}, nil); resumed.StatusCode != http.StatusSeeOther {
		t.Fatalf("resume = %d", resumed.StatusCode)
	}
	f.waitState(t, id, "ready")
	eve := newBrowser(t, f.service.Handler())
	eve.login("/auth/oidc/start", "user_eve:")
	blocked := organizationFromLocation(t, f.create(t, eve, "Echo").Header.Get("Location"))
	capacity := f.waitState(t, blocked, "failed")
	if capacity.ErrorCode != "capacity" || capacity.ProviderID != "" {
		t.Fatalf("capacity failure = %+v", capacity)
	}
	if response, _ := member.get("/organizations/" + id + "/app/bootstrap"); response.StatusCode == http.StatusNotFound {
		t.Fatal("capacity refusal disturbed a ready organization")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t, 2, nil)
	if f.launcher.starts < 2 {
		t.Fatalf("restart did not relaunch ready tenants: %d starts", f.launcher.starts)
	}
	eveAgain := newBrowser(t, f.service.Handler())
	eveAgain.login("/auth/oidc/start", "user_eve:")
	_, status = eveAgain.get("/organizations/" + blocked + "/provisioning")
	eveAgain.do(http.MethodPost, "/organizations/"+blocked+"/provisioning/resume", url.Values{"csrf": {csrfFrom(t, status)}}, nil)
	f.waitState(t, blocked, "ready")
}

func TestProvisioningTenantStartKeepsSlowChild(t *testing.T) {
	t.Parallel()
	for _, readyAfter := range []time.Duration{time.Minute, 2 * time.Minute} {
		t.Run(readyAfter.String(), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				launcher := &silentLauncher{running: map[string]bool{}}
				service := &Service{config: Config{
					Allocation: &AllocationConfig{Launcher: launcher}, tenantStartTimeout: 30 * time.Second,
					SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), now: time.Now,
					transport: func(Organization) (http.RoundTripper, error) {
						return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							if time.Since(started) < readyAfter {
								w.WriteHeader(http.StatusServiceUnavailable)
								return
							}
							w.WriteHeader(http.StatusNoContent)
						})}, nil
					},
				}}
				organization := Organization{ID: "org_slow", Generation: 1}
				if err := service.runStep(t.Context(), &organization, "tenant_start"); err == nil {
					t.Fatal("unready tenant completed startup")
				}
				if launches, stops, running := launcher.counts(); launches != 1 || stops != 0 || running != 1 {
					t.Fatalf("readiness timeout discarded startup: launches=%d stops=%d running=%d", launches, stops, running)
				}
				time.Sleep(readyAfter - time.Since(started))
				if err := service.runStep(t.Context(), &organization, "tenant_start"); err != nil {
					t.Fatalf("slow tenant did not recover: %v", err)
				}
				if launches, stops, running := launcher.counts(); launches != 1 || stops != 0 || running != 1 {
					t.Fatalf("readiness retry replaced child: launches=%d stops=%d running=%d", launches, stops, running)
				}
			})
		})
	}
}

func TestProvisioningTenantStartFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	for _, tc := range []struct {
		name         string
		err          error
		failure      error
		retryLimit   int
		wantLaunches int
		wantDetail   string
	}{
		{name: "hub never becomes healthy", retryLimit: 1, wantLaunches: 1, wantDetail: "the tenant Hub did not become healthy within 100ms"},
		{name: "launcher refuses", err: errors.New("launch refused at /private/tenant"), retryLimit: 1, wantLaunches: 0},
		{name: "supervisor gave up before the retry limit", failure: &TenantExitError{Exits: 5}, retryLimit: 5, wantLaunches: 1, wantDetail: "the tenant Hub exited 5 times in a row without staying up (last: exited cleanly)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			launcher := &silentLauncher{err: tc.err, failure: tc.failure, running: map[string]bool{}}
			f := newProvisioningFixtureWith(t, 3, func(a *AllocationConfig) { a.RetryLimit = tc.retryLimit; a.Launcher = launcher }, func(f *provisioningFixture) {
				f.timeout = 100 * time.Millisecond
			})
			member := newBrowser(t, f.service.Handler())
			member.login("/auth/oidc/start", "user_member:")
			id := organizationFromLocation(t, f.create(t, member, "Delta").Header.Get("Location"))
			failed := f.waitState(t, id, "failed")
			if failed.ErrorCode != "tenant_start_failed" || failed.Step != "tenant_files" || failed.ErrorDetail != tc.wantDetail || failed.Attempts != tc.retryLimit && tc.failure == nil || tc.failure != nil && failed.Attempts != 1 {
				t.Fatalf("failure = %+v", failed)
			}
			status := member.do(http.MethodGet, "/api/cloud/organizations/"+id+"/provisioning", nil, map[string]string{"Accept": "application/json"})
			var body struct {
				State     string `json:"state"`
				Error     string `json:"error"`
				CanResume bool   `json:"can_resume"`
			}
			if err := json.Unmarshal([]byte(status.Body), &body); err != nil || body.State != "failed" || !body.CanResume || !strings.Contains(body.Error, "tenant_start_failed") || !strings.Contains(body.Error, tc.wantDetail) || strings.Contains(body.Error, "/private") {
				t.Fatalf("failed provisioning JSON = %s %v", status.Body, err)
			}
			if launches, stops, running := launcher.counts(); launches != tc.wantLaunches || stops != tc.wantLaunches || running != 0 {
				t.Fatalf("launches = %d, stops = %d, running = %d; a failed tenant start must not stay supervised", launches, stops, running)
			}
			launcher.mu.Lock()
			launcher.err = nil
			launcher.mu.Unlock()
			_, page := member.get("/organizations/" + id + "/provisioning")
			if resumed := member.do(http.MethodPost, "/organizations/"+id+"/provisioning/resume", url.Values{"csrf": {csrfFrom(t, page)}}, nil); resumed.StatusCode != http.StatusSeeOther {
				t.Fatalf("resume = %d", resumed.StatusCode)
			}
			f.waitState(t, id, "failed")
			if launches, _, _ := launcher.counts(); launches != tc.wantLaunches+1 {
				t.Fatalf("resume launches = %d, want a fresh launch", launches)
			}
		})
	}
}

func TestTenantStartKeepsSlowReadyTenantSupervised(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	for _, tc := range []struct {
		name        string
		state       string
		wantRunning int
	}{
		{name: "provisioning tenant stays supervised for retry", state: "allocating", wantRunning: 1},
		{name: "ready tenant stays supervised after a restart", state: "ready", wantRunning: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			launcher := &silentLauncher{running: map[string]bool{}}
			f := newProvisioningFixtureWith(t, 3, func(a *AllocationConfig) { a.RetryLimit = 1; a.Launcher = launcher }, func(f *provisioningFixture) {
				f.timeout = 100 * time.Millisecond
			})
			member := newBrowser(t, f.service.Handler())
			member.login("/auth/oidc/start", "user_member:")
			organization := f.waitState(t, organizationFromLocation(t, f.create(t, member, "Delta").Header.Get("Location")), "failed")
			organization.State = tc.state
			err := f.service.runStep(t.Context(), &organization, "tenant_start")
			if err == nil || !strings.Contains(failureDetail(err), "did not become healthy within 100ms") {
				t.Fatalf("tenant_start = %v", err)
			}
			if _, _, running := launcher.counts(); running != tc.wantRunning {
				t.Fatalf("running = %d, want %d; a ready tenant still migrating must not be abandoned", running, tc.wantRunning)
			}
		})
	}
}

func TestProvisioningFailsWhenTenantExitsEveryStart(t *testing.T) {
	if testing.Short() {
		t.Skip("live service integration")
	}

	t.Parallel()
	launcher := &ExecLauncher{Binary: "/usr/bin/false", RestartLimit: 2, Logger: slog.New(slog.DiscardHandler), Configure: func(TenantSpec) ([]byte, error) { return []byte("{}\n"), nil }}
	f := newProvisioningFixtureWith(t, 3, func(a *AllocationConfig) { a.RetryLimit = 2; a.Launcher = launcher }, func(f *provisioningFixture) {
		f.timeout = 20 * time.Second
	})
	member := newBrowser(t, f.service.Handler())
	member.login("/auth/oidc/start", "user_member:")
	id := organizationFromLocation(t, f.create(t, member, "Delta").Header.Get("Location"))
	failed := f.waitState(t, id, "failed")
	want := "the tenant Hub exited 2 times in a row without staying up (last: exit status 1)"
	if failed.ErrorCode != "tenant_start_failed" || failed.ErrorDetail != want || failed.Attempts > 2 {
		t.Fatalf("failure = %+v", failed)
	}
	status := member.do(http.MethodGet, "/api/cloud/organizations/"+id+"/provisioning", nil, map[string]string{"Accept": "application/json"})
	var body struct {
		State     string `json:"state"`
		Error     string `json:"error"`
		CanResume bool   `json:"can_resume"`
	}
	if err := json.Unmarshal([]byte(status.Body), &body); err != nil || body.State != "failed" || !body.CanResume || !strings.Contains(body.Error, want) {
		t.Fatalf("failed provisioning JSON = %s %v", status.Body, err)
	}
	launcher.mu.Lock()
	running := len(launcher.running)
	launcher.mu.Unlock()
	if running != 0 {
		t.Fatal("a tenant that never stays up is still supervised after provisioning failed")
	}
}

func TestProvisioningStatusShowsFailureReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		organization Organization
		want         string
	}{
		{name: "first attempt", organization: Organization{State: "allocating", Step: "tenant_files"}},
		{name: "retrying with reason", organization: Organization{State: "allocating", Attempts: 2, ErrorDetail: "the tenant Hub did not become healthy within 30s"}, want: "Attempt 2 of 5 failed: the tenant Hub did not become healthy within 30s. Retrying automatically."},
		{name: "retrying without reason", organization: Organization{State: "allocating", Attempts: 1}},
		{name: "failed with reason", organization: Organization{State: "failed", ErrorCode: "tenant_start_failed", ErrorDetail: "the tenant Hub exited once without staying up (last: exit status 1)"}, want: "Setup stopped (tenant_start_failed): the tenant Hub exited once without staying up (last: exit status 1). Your request and any completed steps are kept."},
		{name: "failed without reason", organization: Organization{State: "failed", ErrorCode: "owner_conflict"}, want: "Setup stopped (owner_conflict). Your request and any completed steps are kept."},
		{name: "capacity", organization: Organization{State: "failed", ErrorCode: "capacity"}, want: "The service is at capacity. Your request is saved; resume it later without creating another organization."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := provisioningStatus(tc.organization, 5).Error; got != tc.want {
				t.Fatalf("Error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProvisioningPollsReuseProviderSessionVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	for _, tc := range []struct {
		name      string
		polls     int
		advance   time.Duration
		mutate    bool
		revoke    bool
		wantCalls int
		wantOK    bool
	}{
		{name: "polls within the interval share one verification", polls: 5, wantCalls: 1, wantOK: true},
		{name: "poll just inside the interval", polls: 1, advance: providerSessionRecheck - time.Second, wantCalls: 1, wantOK: true},
		{name: "poll after the interval verifies again", polls: 1, advance: providerSessionRecheck, wantCalls: 2, wantOK: true},
		{name: "mutation always verifies", polls: 1, mutate: true, wantCalls: 2, wantOK: true},
		{name: "revocation is trusted only within the interval", polls: 1, revoke: true, wantCalls: 1, wantOK: true},
		{name: "revocation lands after the interval", polls: 1, advance: providerSessionRecheck, revoke: true, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			clock := time.Now().UTC()
			now := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}
			launcher := &silentLauncher{err: errors.New("no tenant in this test"), running: map[string]bool{}}
			f := newProvisioningFixtureWith(t, 3, func(a *AllocationConfig) { a.RetryLimit = 1; a.Launcher = launcher }, func(f *provisioningFixture) { f.now = now })
			member := newBrowser(t, f.service.Handler())
			member.login("/auth/oidc/start", "user_member:")
			id := organizationFromLocation(t, f.create(t, member, "Delta").Header.Get("Location"))
			poll := func() page {
				return member.do(http.MethodGet, "/api/cloud/organizations/"+id+"/provisioning", nil, map[string]string{"Accept": "application/json"})
			}
			if first := poll(); first.StatusCode != http.StatusOK {
				t.Fatalf("first poll = %d", first.StatusCode)
			}
			f.provider.mu.Lock()
			before := f.provider.verifications
			f.provider.mu.Unlock()
			if tc.revoke {
				f.provider.mu.Lock()
				clear(f.provider.sessions)
				f.provider.mu.Unlock()
			}
			mu.Lock()
			clock = clock.Add(tc.advance)
			mu.Unlock()
			if tc.mutate {
				_, body := member.get("/organizations/" + id + "/provisioning")
				member.do(http.MethodPost, "/organizations/"+id+"/provisioning/resume", url.Values{"csrf": {csrfFrom(t, body)}}, nil)
			}
			last := page{}
			for range tc.polls {
				last = poll()
			}
			f.provider.mu.Lock()
			calls := f.provider.verifications - before
			f.provider.mu.Unlock()
			if calls != tc.wantCalls-1 {
				t.Fatalf("provider verifications after the first poll = %d, want %d", calls, tc.wantCalls-1)
			}
			if (last.StatusCode == http.StatusOK) != tc.wantOK {
				t.Fatalf("last poll = %d, want ok %v", last.StatusCode, tc.wantOK)
			}
		})
	}
}

func TestProvisioningRejectsIneligibleCreators(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL tenant provisioning integration")
	}

	t.Parallel()
	f := newProvisioningFixture(t, 3, func(a *AllocationConfig) { a.AllowedDomains = []string{"allowed.test"} })
	member := newBrowser(t, f.service.Handler())
	member.login("/auth/oidc/start", "user_member:")
	if response := f.create(t, member, "Delta"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("ineligible create = %d", response.StatusCode)
	}
	for _, config := range []*AllocationConfig{
		{TenantRoot: "relative", SocketRoot: "/s", MaxTenants: 1, MaxConcurrent: 1, RetryLimit: 1, Launcher: f.launcher},
		{TenantRoot: "/t", SocketRoot: "/s", MaxTenants: 0, MaxConcurrent: 1, RetryLimit: 1, Launcher: f.launcher},
		{TenantRoot: "/t", SocketRoot: "/s", MaxTenants: 1, MaxConcurrent: 1, RetryLimit: 1},
	} {
		if err := config.validate(); err == nil {
			t.Fatalf("invalid allocation accepted: %+v", config)
		}
	}
}

func TestProvisioningResumesInterruptedDeletionAndMemoryFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("live service integration")
	}

	t.Parallel()
	f := newProvisioningFixture(t, 3, nil)
	member := newBrowser(t, f.service.Handler())
	member.login("/auth/oidc/start", "user_member:")
	id := organizationFromLocation(t, f.create(t, member, "Delta").Header.Get("Location"))
	f.waitState(t, id, "ready")
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state = 'deleting' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t, 3, func(a *AllocationConfig) { a.MinAvailableMemoryBytes = 1 << 62 })
	f.service.wakeAllocator()
	f.waitState(t, id, "deleted")
	eve := newBrowser(t, f.service.Handler())
	eve.login("/auth/oidc/start", "user_eve:")
	blocked := organizationFromLocation(t, f.create(t, eve, "Echo").Header.Get("Location"))
	if failed := f.waitState(t, blocked, "failed"); failed.ErrorCode != "capacity" {
		t.Fatalf("memory floor admission = %+v", failed)
	}
}

func TestEntryServesClientAndJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("live service integration")
	}

	t.Parallel()
	f := newProvisioningFixture(t, 3, func(a *AllocationConfig) {
		a.Prices = []hubserver.HostedBillingPrice{{PriceID: "price_starter", Label: "Starter", Plan: hubserver.PlanReference{ID: "starter", Version: 1}}}
	})
	f.service.config.Billing = &BillingConfig{}
	f.service.config.clientFS = fstest.MapFS{"app/conversation/index.html": {Data: []byte(`<html><head><script src="/static/app/conversation/app.js"></script></head><body><div id="root"></div></body></html>`)}}
	stranger := newBrowser(t, f.service.Handler())
	if response, body := stranger.get("/"); response.StatusCode != http.StatusOK || !strings.Contains(body, `<meta name="detent-surface" content="entry">`) || !strings.Contains(body, `content=""`) {
		t.Fatalf("sign-in shell = %d %s", response.StatusCode, body)
	}
	if response, _ := stranger.get("/organizations"); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("chooser without a session = %d", response.StatusCode)
	}
	if response := stranger.do(http.MethodGet, "/api/cloud/organizations", nil, map[string]string{"Accept": "application/json"}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("organizations JSON without a session = %d", response.StatusCode)
	}
	member := newBrowser(t, f.service.Handler())
	member.login("/auth/oidc/start", "user_member:")
	for _, path := range []string{"/organizations", "/organizations/new"} {
		if response, body := member.get(path); response.StatusCode != http.StatusOK || !strings.Contains(body, "detent-surface") {
			t.Fatalf("%s shell = %d", path, response.StatusCode)
		}
	}
	listing := member.do(http.MethodGet, "/api/cloud/organizations", nil, map[string]string{"Accept": "application/json"})
	var chooser struct {
		CSRF      string               `json:"csrf"`
		CanCreate bool                 `json:"can_create"`
		Pending   []organizationChoice `json:"pending"`
	}
	if err := json.Unmarshal([]byte(listing.Body), &chooser); err != nil || !chooser.CanCreate || chooser.CSRF == "" {
		t.Fatalf("organizations JSON = %s %v", listing.Body, err)
	}
	jsonHeaders := map[string]string{"Accept": "application/json", "X-CSRF-Token": chooser.CSRF}
	created := member.do(http.MethodPost, "/organizations", url.Values{"name": {"Delta"}, "creation_key": {"key_0123456789abcdef"}}, jsonHeaders)
	var result struct {
		Next         string            `json:"next"`
		Organization map[string]string `json:"organization"`
	}
	if created.StatusCode != http.StatusCreated || json.Unmarshal([]byte(created.Body), &result) != nil || !strings.HasSuffix(result.Next, "/provisioning") {
		t.Fatalf("JSON create = %d %s", created.StatusCode, created.Body)
	}
	id := result.Organization["id"]
	for _, path := range []string{"/api/cloud/organizations", "/api/cloud/session"} {
		var limited struct {
			CanCreate      bool `json:"can_create"`
			FreeSlotUsed   bool `json:"free_slot_used"`
			CreationPrices []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"creation_prices"`
		}
		response := member.do(http.MethodGet, path, nil, map[string]string{"Accept": "application/json"})
		if err := json.Unmarshal([]byte(response.Body), &limited); err != nil || !limited.CanCreate {
			t.Fatalf("%s with a used Free slot = %s %v", path, response.Body, err)
		}
		if path == "/api/cloud/session" && (!limited.FreeSlotUsed || len(limited.CreationPrices) != 1 || limited.CreationPrices[0].ID != "price_starter" || limited.CreationPrices[0].Label != "Starter") {
			t.Fatalf("creation plans JSON = %s", response.Body)
		}
	}
	if response, body := member.get("/organizations/" + id + "/provisioning"); response.StatusCode == http.StatusOK && !strings.Contains(body, "detent-surface") {
		t.Fatalf("provisioning shell = %s", body)
	}
	quota := member.do(http.MethodPost, "/organizations", url.Values{"name": {"Echo"}, "creation_key": {"key_fedcba9876543210"}}, jsonHeaders)
	if quota.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(quota.Body, `"code":"paid_plan_required"`) {
		t.Fatalf("JSON paid plan required = %d %s", quota.StatusCode, quota.Body)
	}
	missing := member.do(http.MethodPost, "/organizations", url.Values{"name": {"Echo"}, "creation_key": {"key_fedcba9876543210"}}, map[string]string{"Accept": "application/json"})
	if missing.StatusCode != http.StatusForbidden || !strings.Contains(missing.Body, `"code":"invalid_csrf"`) {
		t.Fatalf("JSON missing CSRF = %d %s", missing.StatusCode, missing.Body)
	}
	f.waitState(t, id, "ready")
	status := member.do(http.MethodGet, "/api/cloud/organizations/"+id+"/provisioning", nil, map[string]string{"Accept": "application/json"})
	if !strings.Contains(status.Body, `"next":"/delta/work"`) {
		t.Fatalf("ready provisioning JSON = %s", status.Body)
	}
	session := member.do(http.MethodGet, "/api/cloud/session", nil, map[string]string{"Accept": "application/json"})
	if session.StatusCode != http.StatusOK || !strings.Contains(session.Body, `"csrf":"`+chooser.CSRF+`"`) {
		t.Fatalf("session JSON = %d %s", session.StatusCode, session.Body)
	}
	ready := member.do(http.MethodGet, "/api/cloud/organizations", nil, map[string]string{"Accept": "application/json"})
	if !strings.Contains(ready.Body, `"id":"`+id+`"`) || !strings.Contains(ready.Body, `"role":"owner"`) {
		t.Fatalf("ready organizations JSON = %s", ready.Body)
	}
}

func TestCanCreateOrganizations(t *testing.T) {
	tests := []struct {
		name       string
		allocation bool
		session    accountSession
		want       bool
	}{
		{name: "ordinary account", allocation: true, session: accountSession{Email: "owner@example.test"}, want: true},
		{name: "platform member", allocation: true, session: accountSession{Email: "staff@example.test"}, want: true},
		{name: "support session", allocation: true, session: accountSession{Identity: auth.HostedIdentity{SupportActor: "support@example.test"}}},
		{name: "no allocation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &Service{}
			if test.allocation {
				s.config.Allocation = &AllocationConfig{}
			}
			got, err := s.canCreate(t.Context(), test.session)
			if err != nil || got != test.want {
				t.Fatalf("canCreate = %v, %v; want %v", got, err, test.want)
			}
		})
	}
}

func TestOwnerFreeAllowance(t *testing.T) {
	tests := []struct {
		name, subject, base, role, price                             string
		subscribed, grant, expired, featureOnly, deleted, concurrent bool
		wantPaid                                                     bool
	}{
		{name: "first organization", subject: "user_carol", base: "free"},
		{name: "owned Free requires plan", subject: "user_alice", base: "free", wantPaid: true},
		{name: "second organization selects paid", subject: "user_alice", base: "free", price: "price_starter"},
		{name: "upgraded organization frees slot", subject: "user_alice", base: "free", subscribed: true},
		{name: "complimentary organization", subject: "user_alice", base: "free", grant: true},
		{name: "expired grant consumes slot", subject: "user_alice", base: "free", grant: true, expired: true, wantPaid: true},
		{name: "feature grant still consumes slot", subject: "user_alice", base: "free", grant: true, featureOnly: true, wantPaid: true},
		{name: "legacy pilot", subject: "user_alice", base: "pilot_free"},
		{name: "invited membership", subject: "user_bob", base: "free"},
		{name: "deleted organization", subject: "user_alice", base: "free", deleted: true},
		{name: "platform admin dual role", subject: "user_alice", base: "free", role: "admin"},
		{name: "entitlement administrator", subject: "user_alice", base: "free", role: "billing"},
		{name: "concurrent creations", subject: "user_carol", base: "free", concurrent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			plans := hubserver.CheckoutHostedPlans(nil)
			repository := tenantPostgresRepository(t, f.fixtures["org_alpha"])
			if err := repository.WriteTenant(t.Context(), "org_alpha", corestore.Serving, func(w tenantstore.Writer) error {
				for _, plan := range plans.Plans {
					if plan.ID != "free" && plan.ID != "starter" {
						continue
					}
					if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: "org_alpha", PlanReference: billingstore.PlanReference{ID: plan.ID, Version: plan.Version}, Name: plan.Name, Allowances: plan.Allowances, Features: plan.Features}); err != nil {
						return err
					}
				}
				assignment, err := w.BillingAssignment(t.Context())
				if err != nil {
					return err
				}
				revision := assignment.Revision
				assignment.Base = billingstore.PlanReference{ID: test.base, Version: 1}
				if test.subscribed {
					assignment.Subscription = &billingstore.PlanReference{ID: "starter", Version: 1}
					expiry := time.Now().Add(time.Hour)
					assignment.ExpiresAt = &expiry
				}
				if err := w.SetBillingAssignment(t.Context(), assignment, revision); err != nil {
					return err
				}
				if test.grant {
					grant := billingstore.Grant{OrganizationID: "org_alpha", ID: "comp_starter", Plan: billingstore.PlanReference{ID: "starter", Version: 1}, Scope: []string{"projects", "native_execution"}, StartsAt: time.Now().Add(-time.Hour)}
					if test.expired {
						expiry := time.Now().Add(-time.Minute)
						grant.ExpiresAt = &expiry
					}
					if test.featureOnly {
						grant.Scope = []string{"model_choice"}
					}
					return w.CreateBillingGrant(t.Context(), grant)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if test.deleted {
				if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='deleted' WHERE id='org_alpha'"); err != nil {
					t.Fatal(err)
				}
			}
			email := f.provider.users[test.subject]
			if test.role != "" {
				if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members(email,role,added_by,added_at,updated_at) VALUES(?,?,'test',?,?)", email, test.role, formatTime(time.Now()), formatTime(time.Now())); err != nil {
					t.Fatal(err)
				}
			}
			f.service.config.Allocation = &AllocationConfig{SocketRoot: filepath.Join(t.TempDir(), "s"), Prices: []hubserver.HostedBillingPrice{{PriceID: "price_starter", Label: "Starter", Plan: hubserver.PlanReference{ID: "starter", Version: 1}}}}
			f.service.config.Billing = &BillingConfig{}
			session := accountSession{Subject: test.subject, Email: email}
			attempts := 1
			if test.concurrent {
				attempts = 2
			}
			start := make(chan struct{})
			results := make(chan error, attempts)
			for i := range attempts {
				go func() {
					<-start
					id, err := f.service.createOrganizationFor(t.Context(), session, "New organization", fmt.Sprintf("creation_key_%016d", i), test.price)
					if err == nil {
						organization, readErr := f.service.registry.Organization(t.Context(), id)
						if readErr != nil {
							err = readErr
						} else if organization.CheckoutPrice != test.price {
							err = fmt.Errorf("checkout price = %q, want %q", organization.CheckoutPrice, test.price)
						}
					}
					results <- err
				}()
			}
			close(start)
			paid, succeeded := 0, 0
			for range attempts {
				err := <-results
				switch {
				case errors.Is(err, errPaidPlanRequired):
					paid++
				case err == nil:
					succeeded++
				default:
					t.Fatal(err)
				}
			}
			wantPaid := 0
			if test.wantPaid || test.concurrent {
				wantPaid = 1
			}
			if paid != wantPaid || succeeded != attempts-wantPaid {
				t.Fatalf("created=%d, paid selection required=%d; want %d, %d", succeeded, paid, attempts-wantPaid, wantPaid)
			}
		})
	}
}

func TestAllocatorShutdownMissingLauncher(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	close(done)
	s := &Service{config: Config{Allocation: &AllocationConfig{}}, stopAllocator: cancel, allocatorDone: done}
	if err := s.closeAllocator(); err == nil {
		t.Fatal("shutdown accepted an allocation without a launcher")
	}
	if ctx.Err() == nil {
		t.Fatal("shutdown did not cancel allocator work")
	}
}
