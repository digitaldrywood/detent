//go:build !windows

package cloudentry

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type processLauncher struct {
	provider *fakeProvider
	key      ed25519.PrivateKey
	mu       sync.Mutex
	running  map[string]func()
	starts   int
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	config := hubserver.Config{
		DatabasePath: filepath.Join(spec.Directory, "hub.db"), ListenAddress: "unix:" + spec.Socket, GitHubDisabled: true,
		InitialAdminToken: []byte(testAdminKey), Logger: slog.New(slog.DiscardHandler),
		Hosted: &hubserver.HostedConfig{
			OrganizationID: spec.Organization.ID, WorkOSOrganizationID: spec.Organization.ProviderID, PublicURL: spec.PublicURL, Provider: l.provider,
			StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"},
			SharedEntry: &hubserver.HostedSharedEntry{Issuer: spec.Issuer, PublicKeys: []ed25519.PublicKey{public}, Generation: spec.Organization.Generation},
		},
	}
	go func() {
		defer close(done)
		_ = hubserver.Run(ctx, config)
	}()
	l.starts++
	l.running[spec.Organization.ID] = func() { cancel(); <-done }
	return nil
}

func (l *processLauncher) Stop(id string) error {
	l.mu.Lock()
	stop, ok := l.running[id]
	delete(l.running, id)
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
	service  *Service
	provider *fakeProvider
	launcher *processLauncher
	state    string
	roots    [2]string
	key      ed25519.PrivateKey
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

func newProvisioningFixture(t *testing.T, maxTenants int, mutate func(*AllocationConfig)) *provisioningFixture {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[1] = 7
	root := shortTempDir(t)
	f := &provisioningFixture{provider: newFakeProvider(), state: t.TempDir(), roots: [2]string{filepath.Join(root, "t"), filepath.Join(root, "s")}, key: ed25519.NewKeyFromSeed(seed)}
	for _, user := range []string{"dana", "eve", "fay"} {
		f.provider.users["user_"+user] = user + "@example.test"
	}
	f.launcher = &processLauncher{provider: f.provider, key: f.key, running: map[string]func(){}}
	f.open(t, maxTenants, mutate)
	return f
}

func (f *provisioningFixture) open(t *testing.T, maxTenants int, mutate func(*AllocationConfig)) {
	t.Helper()
	allocation := &AllocationConfig{TenantRoot: f.roots[0], SocketRoot: f.roots[1], MaxTenants: maxTenants, MaxConcurrent: 2, MaxPerIdentity: 1, RetryLimit: 3, Launcher: f.launcher}
	if mutate != nil {
		mutate(allocation)
	}
	service, err := Open(t.Context(), Config{PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: f.key, Provider: f.provider,
		StaffEmails: []string{"staff@example.test"}, StateDir: f.state, Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{}, Allocation: allocation})
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
	deadline := time.Now().Add(30 * time.Second)
	for {
		organization, err := f.service.registry.Organization(t.Context(), id)
		if err == nil {
			for _, state := range states {
				if organization.State == state {
					return organization
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("organization %s state = %+v, want %v (%v)", id, organization, states, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	t.Parallel()
	f := newProvisioningFixture(t, 3, nil)
	dana := newBrowser(t, f.service.Handler())
	dana.login("/auth/oidc/start", "user_dana:")
	_, chooser := dana.get("/organizations")
	if !strings.Contains(chooser, `href="/organizations/new"`) {
		t.Fatal("chooser does not offer organization creation")
	}
	response, body := dana.get("/organizations/new")
	_, rest, _ := strings.Cut(body, `name="creation_key" value="`)
	key, _, _ := strings.Cut(rest, `"`)
	form := url.Values{"name": {"Delta"}, "creation_key": {key}, "csrf": {csrfFrom(t, body)}}
	first := dana.do(http.MethodPost, "/organizations", form, nil)
	if response.StatusCode != http.StatusOK || first.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d: %s", first.StatusCode, first.Body)
	}
	id := organizationFromLocation(t, first.Header.Get("Location"))
	if again := dana.do(http.MethodPost, "/organizations", form, nil); again.Header.Get("Location") != first.Header.Get("Location") {
		t.Fatalf("retried intent created %q, want %q", again.Header.Get("Location"), first.Header.Get("Location"))
	}
	changed := url.Values{"name": {"Other"}, "creation_key": {key}, "csrf": form["csrf"]}
	if conflict := dana.do(http.MethodPost, "/organizations", changed, nil); conflict.StatusCode != http.StatusConflict {
		t.Fatalf("changed intent status = %d", conflict.StatusCode)
	}
	ready := f.waitState(t, id, "ready")
	if f.provider.creates != 1 || ready.ProviderID != "porg_"+id || ready.Generation != 1 {
		t.Fatalf("provider creates = %d, organization = %+v", f.provider.creates, ready)
	}
	if status, _ := dana.get("/organizations/" + id + "/provisioning"); status.StatusCode != http.StatusSeeOther {
		t.Fatalf("ready provisioning page = %d", status.StatusCode)
	}
	dana.login("/organizations/"+id+"/organization", "user_dana:porg_"+id)
	response, body = dana.get("/organizations/" + id + "/organization")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Delta") || !strings.Contains(body, `/organizations/`+id+`/delete`) {
		t.Fatalf("owner page = %d %s", response.StatusCode, body)
	}
	if quota := f.create(t, dana, "Second"); quota.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second organization status = %d", quota.StatusCode)
	}
	eve := newBrowser(t, f.service.Handler())
	eve.login("/auth/oidc/start", "user_eve:")
	if peek, _ := eve.get("/organizations/" + id + "/provisioning"); peek.StatusCode != http.StatusNotFound {
		t.Fatalf("another user saw provisioning status: %d", peek.StatusCode)
	}
	if denied, _ := eve.get("/organizations/" + id + "/delete"); denied.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner delete page = %d", denied.StatusCode)
	}
	_, deletePage := dana.get("/organizations/" + id + "/delete")
	if wrong := dana.do(http.MethodPost, "/organizations/"+id+"/delete", url.Values{"confirm": {"delta"}, "csrf": {csrfFrom(t, deletePage)}}, nil); wrong.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed delete = %d", wrong.StatusCode)
	}
	deleted := dana.do(http.MethodPost, "/organizations/"+id+"/delete", url.Values{"confirm": {"Delta"}, "csrf": {csrfFrom(t, deletePage)}}, nil)
	if deleted.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d: %s", deleted.StatusCode, deleted.Body)
	}
	f.waitState(t, id, "deleted")
	if gone, _ := dana.get("/organizations/" + id + "/organization"); gone.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted organization still routes: %d", gone.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(f.roots[0], id, "hub.db")); err != nil {
		t.Fatalf("deletion removed tenant data: %v", err)
	}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state = 'ready' WHERE id = ?", id); err == nil {
		t.Fatal("a deleted organization was resurrected")
	}
}

func TestProvisioningCapacityAndRecovery(t *testing.T) {
	t.Parallel()
	f := newProvisioningFixture(t, 1, func(a *AllocationConfig) { a.RetryLimit = 1 })
	dana := newBrowser(t, f.service.Handler())
	dana.login("/auth/oidc/start", "user_dana:")
	f.provider.mu.Lock()
	f.provider.createFailures = 1
	f.provider.mu.Unlock()
	created := f.create(t, dana, "Delta")
	id := organizationFromLocation(t, created.Header.Get("Location"))
	failed := f.waitState(t, id, "failed")
	if failed.ErrorCode != "provider_organization_failed" {
		t.Fatalf("failure = %+v", failed)
	}
	_, status := dana.get("/organizations/" + id + "/provisioning")
	if !strings.Contains(status, "Resume setup") {
		t.Fatalf("failed status page = %s", status)
	}
	if resumed := dana.do(http.MethodPost, "/organizations/"+id+"/provisioning/resume", url.Values{"csrf": {csrfFrom(t, status)}}, nil); resumed.StatusCode != http.StatusSeeOther {
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
	if response, _ := dana.get("/organizations/" + id + "/organization"); response.StatusCode == http.StatusNotFound {
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

func TestProvisioningRejectsIneligibleCreators(t *testing.T) {
	t.Parallel()
	f := newProvisioningFixture(t, 3, func(a *AllocationConfig) { a.AllowedDomains = []string{"allowed.test"} })
	dana := newBrowser(t, f.service.Handler())
	dana.login("/auth/oidc/start", "user_dana:")
	if response := f.create(t, dana, "Delta"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("ineligible create = %d", response.StatusCode)
	}
	for _, config := range []*AllocationConfig{
		{TenantRoot: "relative", SocketRoot: "/s", MaxTenants: 1, MaxConcurrent: 1, MaxPerIdentity: 1, RetryLimit: 1, Launcher: f.launcher},
		{TenantRoot: "/t", SocketRoot: "/s", MaxTenants: 0, MaxConcurrent: 1, MaxPerIdentity: 1, RetryLimit: 1, Launcher: f.launcher},
		{TenantRoot: "/t", SocketRoot: "/s", MaxTenants: 1, MaxConcurrent: 1, MaxPerIdentity: 1, RetryLimit: 1},
	} {
		if err := config.validate(); err == nil {
			t.Fatalf("invalid allocation accepted: %+v", config)
		}
	}
}

func TestProvisioningResumesInterruptedDeletionAndMemoryFloor(t *testing.T) {
	t.Parallel()
	f := newProvisioningFixture(t, 3, nil)
	dana := newBrowser(t, f.service.Handler())
	dana.login("/auth/oidc/start", "user_dana:")
	id := organizationFromLocation(t, f.create(t, dana, "Delta").Header.Get("Location"))
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
	t.Parallel()
	f := newProvisioningFixture(t, 3, func(a *AllocationConfig) { a.MaxPerIdentity = 1 })
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
	dana := newBrowser(t, f.service.Handler())
	dana.login("/auth/oidc/start", "user_dana:")
	for _, path := range []string{"/organizations", "/organizations/new", "/invitations/join"} {
		if response, body := dana.get(path); response.StatusCode != http.StatusOK || !strings.Contains(body, "detent-surface") {
			t.Fatalf("%s shell = %d", path, response.StatusCode)
		}
	}
	listing := dana.do(http.MethodGet, "/api/cloud/organizations", nil, map[string]string{"Accept": "application/json"})
	var chooser struct {
		CSRF      string               `json:"csrf"`
		CanCreate bool                 `json:"can_create"`
		Pending   []organizationChoice `json:"pending"`
	}
	if err := json.Unmarshal([]byte(listing.Body), &chooser); err != nil || !chooser.CanCreate || chooser.CSRF == "" {
		t.Fatalf("organizations JSON = %s %v", listing.Body, err)
	}
	jsonHeaders := map[string]string{"Accept": "application/json", "X-CSRF-Token": chooser.CSRF}
	created := dana.do(http.MethodPost, "/organizations", url.Values{"name": {"Delta"}, "creation_key": {"key_0123456789abcdef"}}, jsonHeaders)
	var result struct {
		Next         string            `json:"next"`
		Organization map[string]string `json:"organization"`
	}
	if created.StatusCode != http.StatusCreated || json.Unmarshal([]byte(created.Body), &result) != nil || !strings.HasSuffix(result.Next, "/provisioning") {
		t.Fatalf("JSON create = %d %s", created.StatusCode, created.Body)
	}
	id := result.Organization["id"]
	if response, body := dana.get("/organizations/" + id + "/provisioning"); response.StatusCode == http.StatusOK && !strings.Contains(body, "detent-surface") {
		t.Fatalf("provisioning shell = %s", body)
	}
	quota := dana.do(http.MethodPost, "/organizations", url.Values{"name": {"Echo"}, "creation_key": {"key_fedcba9876543210"}}, jsonHeaders)
	if quota.StatusCode != http.StatusTooManyRequests || !strings.Contains(quota.Body, `"code":"quota_reached"`) {
		t.Fatalf("JSON quota = %d %s", quota.StatusCode, quota.Body)
	}
	missing := dana.do(http.MethodPost, "/organizations", url.Values{"name": {"Echo"}, "creation_key": {"key_fedcba9876543210"}}, map[string]string{"Accept": "application/json"})
	if missing.StatusCode != http.StatusForbidden || !strings.Contains(missing.Body, `"code":"invalid_csrf"`) {
		t.Fatalf("JSON missing CSRF = %d %s", missing.StatusCode, missing.Body)
	}
	f.waitState(t, id, "ready")
	status := dana.do(http.MethodGet, "/api/cloud/organizations/"+id+"/provisioning", nil, map[string]string{"Accept": "application/json"})
	if !strings.Contains(status.Body, `"next":"/organizations/`+id+`/work"`) {
		t.Fatalf("ready provisioning JSON = %s", status.Body)
	}
	session := dana.do(http.MethodGet, "/api/cloud/session", nil, map[string]string{"Accept": "application/json"})
	if session.StatusCode != http.StatusOK || !strings.Contains(session.Body, `"csrf":"`+chooser.CSRF+`"`) {
		t.Fatalf("session JSON = %d %s", session.StatusCode, session.Body)
	}
	dana.login("/organizations/"+id+"/work", "user_dana:porg_"+id)
	if response, body := dana.get("/organizations/" + id + "/work"); response.StatusCode != http.StatusOK || !strings.Contains(body, `content="/organizations/`+id+`"`) {
		t.Fatalf("organization client home = %d %s", response.StatusCode, body)
	}
}
