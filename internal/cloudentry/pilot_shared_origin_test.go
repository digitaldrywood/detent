//go:build !windows

package cloudentry

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const pilotOperatorToken = "detent_pilot_entitlement_operator_0123456789abcdef"

type pilotTenantLauncher struct {
	provider   *fakeProvider
	billing    billing.Provider
	mu         sync.Mutex
	running    map[string]func()
	transports map[string]*http.Transport
	useUnix    bool
	specs      map[string]TenantSpec
	starts     int
}

func pilotAdminToken(organization string) string {
	return "detent_pilot_admin_" + organization + "_0123456789abcdef"
}

func (l *pilotTenantLauncher) Start(_ context.Context, spec TenantSpec) error {
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
		DatabasePath: filepath.Join(spec.Directory, "hub.db"), GitHubDisabled: true,
		InitialAdminToken: []byte(pilotAdminToken(spec.Organization.ID)), Logger: slog.New(slog.DiscardHandler),
		Hosted: &hubserver.HostedConfig{
			OrganizationID: spec.Organization.ID, WorkOSOrganizationID: spec.Organization.ProviderID, PublicURL: spec.PublicURL, Provider: l.provider,
			StaffEmails: []string{"staff@example.test"}, Plans: pilotPlans(),
			EntitlementAdministrator: "pilot-operator", EntitlementAdminToken: []byte(pilotOperatorToken),
			SharedEntry: &hubserver.HostedSharedEntry{Issuer: spec.Issuer, PublicKeys: []ed25519.PublicKey{public}, Generation: spec.Organization.Generation},
		},
	}
	if l.billing != nil {
		config.Conversation = &hubserver.ConversationConfig{Enabled: true}
		plans := pilotPlans()
		config.Hosted.Billing = &hubserver.HostedBillingConfig{Mode: "test", AccountID: "acct_fixture", CustomerID: "cus_fixture_" + spec.Organization.ID, PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_e2e_secret_value"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: l.billing,
			Prices: []hubserver.HostedBillingPrice{{PriceID: "price_fixture_plus", Label: "Pilot plus", Plan: plans.Plans[len(plans.Plans)-1].PlanReference}}}
	}
	if l.useUnix {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		config.ListenAddress = "unix:" + spec.Socket
		go func() {
			defer close(done)
			_ = hubserver.Run(ctx, config)
		}()
		l.starts++
		l.specs[spec.Organization.ID] = spec
		l.running[spec.Organization.ID] = func() { cancel(); <-done }
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
	l.specs[spec.Organization.ID] = spec
	l.transports[spec.Organization.ID] = transport
	//nolint:contextcheck // The fixture server and service expose context-free close methods.
	l.running[spec.Organization.ID] = func() {
		transport.CloseIdleConnections()
		server.CloseClientConnections()
		server.Close()
		_ = service.Close()
	}
	return nil
}

func (l *pilotTenantLauncher) transport(organization Organization) (http.RoundTripper, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	transport := l.transports[organization.ID]
	if transport == nil {
		return nil, fmt.Errorf("tenant fixture %s is not running", organization.ID)
	}
	return transport, nil
}

func (l *pilotTenantLauncher) Stop(id string) error {
	l.mu.Lock()
	stop, ok := l.running[id]
	delete(l.running, id)
	delete(l.transports, id)
	l.mu.Unlock()
	if ok {
		stop()
	}
	return nil
}

func (*pilotTenantLauncher) Failure(string) error { return nil }

func (l *pilotTenantLauncher) Close() error {
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

type swappableHandler struct{ handler atomic.Pointer[http.Handler] }

func (h *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*h.handler.Load()).ServeHTTP(w, r)
}

type sharedOriginPilot struct {
	service  *Service
	provider *fakeProvider
	launcher *pilotTenantLauncher
	server   *httptest.Server
	handler  *swappableHandler
	base     string
	state    string
	roots    [2]string
	key      ed25519.PrivateKey
	client   fs.FS
}

func newSharedOriginPilot(t *testing.T, maxTenants, retryLimit int) *sharedOriginPilot {
	t.Helper()
	return newSharedOriginPilotWith(t, maxTenants, retryLimit, fstest.MapFS{}, nil)
}

func newSharedOriginPilotWith(t *testing.T, maxTenants, retryLimit int, client fs.FS, billingProvider billing.Provider) *sharedOriginPilot {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[2] = 9
	root := shortTempDir(t)
	p := &sharedOriginPilot{provider: newFakeProvider(), state: t.TempDir(), roots: [2]string{filepath.Join(root, "t"), filepath.Join(root, "s")}, key: ed25519.NewKeyFromSeed(seed), handler: &swappableHandler{}, client: client}
	for _, user := range []string{"dana", "eve", "fay", "gus"} {
		p.provider.users["user_"+user] = user + "@example.test"
	}
	p.launcher = &pilotTenantLauncher{provider: p.provider, billing: billingProvider, running: map[string]func(){}, transports: map[string]*http.Transport{}, useUnix: socketFixtureFits(root), specs: map[string]TenantSpec{}}
	p.server = httptest.NewUnstartedServer(p.handler)
	p.base = "http://" + p.server.Listener.Addr().String()
	p.open(t, maxTenants, retryLimit)
	p.server.Start()
	t.Cleanup(p.server.Close)
	return p
}

func (p *sharedOriginPilot) open(t *testing.T, maxTenants, retryLimit int) {
	t.Helper()
	allocation := &AllocationConfig{TenantRoot: p.roots[0], SocketRoot: p.roots[1], MaxTenants: maxTenants, MaxConcurrent: 2, MaxPerIdentity: 1, RetryLimit: retryLimit, Launcher: p.launcher, EntitlementAdminToken: []byte(pilotOperatorToken)}
	config := Config{PublicURL: p.base, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: p.key, Provider: p.provider,
		StaffEmails: []string{"staff@example.test", "ops@example.test"}, EntitlementAdministrators: []string{"staff@example.test"}, StateDir: p.state, Logger: slog.New(slog.DiscardHandler), clientFS: p.client, Allocation: allocation}
	if !p.launcher.useUnix {
		config.transport = p.launcher.transport
	}
	service, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	p.service = service
	handler := service.Handler()
	p.handler.handler.Store(&handler)
	t.Cleanup(func() { _ = service.Close() })
}

func (p *sharedOriginPilot) waitState(t *testing.T, id string, states ...string) Organization {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		organization, err := p.service.registry.Organization(t.Context(), id)
		if err == nil {
			for _, state := range states {
				if organization.State == state {
					return organization
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("organization %s = %+v, want %v (%v)", id, organization, states, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type pilotBrowser struct {
	t      *testing.T
	base   string
	client *http.Client
}

type pilotResponse struct {
	status   int
	location string
	header   http.Header
	body     string
}

func (p *sharedOriginPilot) browser(t *testing.T) *pilotBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &pilotBrowser{t: t, base: p.base, client: client}
}

func (b *pilotBrowser) request(method, path string, body io.Reader, headers map[string]string) pilotResponse {
	b.t.Helper()
	request, err := http.NewRequestWithContext(b.t.Context(), method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if method != http.MethodGet {
		request.Header.Set("Origin", "null")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		b.t.Fatal(err)
	}
	return pilotResponse{status: response.StatusCode, location: response.Header.Get("Location"), header: response.Header, body: string(raw)}
}

func (b *pilotBrowser) get(path string) pilotResponse {
	b.t.Helper()
	return b.request(http.MethodGet, path, nil, nil)
}

func (b *pilotBrowser) form(path string, values url.Values) pilotResponse {
	b.t.Helper()
	return b.request(http.MethodPost, path, strings.NewReader(values.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func (b *pilotBrowser) json(method, path, csrf string, value any) pilotResponse {
	b.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		b.t.Fatal(err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if csrf != "" {
		headers["X-CSRF-Token"] = csrf
	}
	return b.request(method, path, bytes.NewReader(raw), headers)
}

func (b *pilotBrowser) login(target, code string) pilotResponse {
	b.t.Helper()
	response := b.get(target)
	if response.status != http.StatusSeeOther {
		b.t.Fatalf("GET %s = %d: %s", target, response.status, response.body)
	}
	location := response.location
	if strings.HasPrefix(location, "/auth/oidc/start") {
		location = b.get(location).location
	}
	provider, err := url.Parse(location)
	if err != nil || provider.Host != "identity.example.test" {
		b.t.Fatalf("login redirect = %q", location)
	}
	return b.get("/auth/oidc/callback?" + url.Values{"code": {code}, "state": {provider.Query().Get("state")}}.Encode())
}

func (b *pilotBrowser) csrf(path string) string {
	b.t.Helper()
	response := b.get(path)
	if response.status != http.StatusOK {
		b.t.Fatalf("GET %s = %d", path, response.status)
	}
	return csrfFrom(b.t, response.body)
}

func (b *pilotBrowser) createOrganization(name string) pilotResponse {
	b.t.Helper()
	page := b.get("/organizations/new")
	if page.status != http.StatusOK {
		b.t.Fatalf("new organization page = %d", page.status)
	}
	_, rest, _ := strings.Cut(page.body, `name="creation_key" value="`)
	key, _, _ := strings.Cut(rest, `"`)
	return b.form("/organizations", url.Values{"name": {name}, "creation_key": {key}, "csrf": {csrfFrom(b.t, page.body)}})
}

func pilotStage(t *testing.T, name string, stage func()) {
	t.Helper()
	t.Logf("PILOT shared_origin stage %q started", name)
	stage()
	t.Logf("PILOT shared_origin stage %q passed", name)
}

func pilotRecord(t *testing.T, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PILOT shared_origin %s %s", name, raw)
}

func pilotStatus(t *testing.T, name string, got pilotResponse, want int) {
	t.Helper()
	if got.status != want {
		t.Fatalf("%s status = %d, want %d: %.400s", name, got.status, want, got.body)
	}
}

func pilotDecode(t *testing.T, response pilotResponse, value any) {
	t.Helper()
	if err := json.Unmarshal([]byte(response.body), value); err != nil {
		t.Fatalf("decode %.300s: %v", response.body, err)
	}
}

func pilotPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

type pilotRunner struct {
	runnerauth.Binding
	credential string
}

type pilotOrganization struct {
	id, project, projectName string
	owner                    *pilotBrowser
	ownerCSRF                string
}

func (o pilotOrganization) page() string { return "/organizations/" + o.id + "/organization" }
func (o pilotOrganization) api() string  { return "/api/v2/organizations/" + o.id }
func (o pilotOrganization) projectAPI() string {
	return o.api() + "/projects/" + o.project
}

func (p *sharedOriginPilot) enrollRunner(t *testing.T, o, other pilotOrganization, hostname string) pilotRunner {
	t.Helper()
	binding := runnerauth.NewBinding()
	request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(o.project)}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 900}
	response := o.owner.json(http.MethodPost, o.api()+"/runner-enrollments", o.ownerCSRF, request)
	pilotStatus(t, "runner enrollment", response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	pilotDecode(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: hostname, DisplayName: hostname, Capacity: 1, Version: "test", OS: "linux", Architecture: "amd64"}
	machine := p.browser(t)
	wrong := machine.request(http.MethodPost, other.api()+"/runner-enrollments/redeem", jsonBody(t, redemption), map[string]string{"Authorization": "Bearer " + enrollment.Token, "Content-Type": "application/json"})
	pilotStatus(t, "redeem enrollment at another organization", wrong, http.StatusUnauthorized)
	redeemed := machine.request(http.MethodPost, o.api()+"/runner-enrollments/redeem", jsonBody(t, redemption), map[string]string{"Authorization": "Bearer " + enrollment.Token, "Content-Type": "application/json"})
	pilotStatus(t, "runner redemption", redeemed, http.StatusCreated)
	return pilotRunner{Binding: binding, credential: credential}
}

func jsonBody(t *testing.T, value any) io.Reader {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}

func (p *sharedOriginPilot) machine(t *testing.T, method, path, token string, value any) pilotResponse {
	t.Helper()
	client := p.browser(t)
	var body io.Reader
	headers := map[string]string{"Authorization": "Bearer " + token}
	if value != nil {
		body = jsonBody(t, value)
		headers["Content-Type"] = "application/json"
	}
	return client.request(method, path, body, headers)
}

type pilotStream struct {
	status      int
	contentType string
	events      chan string
}

func (s pilotStream) next(timeout time.Duration) (string, bool, error) {
	select {
	case line, ok := <-s.events:
		return line, ok, nil
	case <-time.After(timeout):
		return "", true, context.DeadlineExceeded
	}
}

func pumpEvents(response *http.Response, events chan<- string, done chan<- struct{}) {
	defer close(done)
	defer close(events)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			select {
			case events <- strings.TrimSpace(line):
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (b *pilotBrowser) stream(t *testing.T, path string) pilotStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := *b.client
	client.Timeout = 0
	response, err := client.Do(request) //nolint:bodyclose // pumpEvents closes the streamed body.
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stream := pilotStream{status: response.StatusCode, contentType: response.Header.Get("Content-Type"), events: make(chan string, 64)}
	done := make(chan struct{})
	go pumpEvents(response, stream.events, done)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if response.StatusCode != http.StatusOK {
		cancel()
	}
	return stream
}

// TestSharedOriginPilotAcceptance exercises the September 8 shared hosted-site
// checklist through one loopback origin: self-service organizations, a member
// of two organizations in concurrent tabs, scoped mutations, SSE, runner
// enrollment and execution, artifact endpoints, entitlements, durable
// provisioning retry, capacity refusal and entry restart.
func TestSharedOriginPilotAcceptance(t *testing.T) {
	p := newSharedOriginPilot(t, 2, 1)
	origin, err := url.Parse(p.base)
	if err != nil {
		t.Fatal(err)
	}
	pilotRecord(t, "origin", map[string]any{"scheme": origin.Scheme, "host_is_loopback": strings.HasPrefix(origin.Host, "127.0.0.1:"), "hostnames_used": 1})
	var alpha, beta pilotOrganization
	var alphaSecondProject string

	pilotStage(t, "unrelated users create organizations on one origin", func() {
		dana, eve := p.browser(t), p.browser(t)
		pilotStatus(t, "dana sign-in", dana.login("/auth/oidc/start", "user_dana:"), http.StatusSeeOther)
		pilotStatus(t, "eve sign-in", eve.login("/auth/oidc/start", "user_eve:"), http.StatusSeeOther)
		p.provider.mu.Lock()
		p.provider.createFailures = 1
		p.provider.mu.Unlock()
		created := dana.createOrganization("Alpha Labs")
		pilotStatus(t, "dana create", created, http.StatusSeeOther)
		alpha.id = organizationFromLocation(t, created.location)
		failed := p.waitState(t, alpha.id, "failed")
		if failed.ErrorCode != "provider_organization_failed" {
			t.Fatalf("injected failure = %+v", failed)
		}
		status := dana.get("/organizations/" + alpha.id + "/provisioning")
		if !strings.Contains(status.body, "Resume setup") {
			t.Fatalf("failed provisioning page = %s", status.body)
		}
		resumed := dana.form("/organizations/"+alpha.id+"/provisioning/resume", url.Values{"csrf": {csrfFrom(t, status.body)}})
		pilotStatus(t, "resume", resumed, http.StatusSeeOther)
		readyAlpha := p.waitState(t, alpha.id, "ready")
		created = eve.createOrganization("Beta Works")
		pilotStatus(t, "eve create", created, http.StatusSeeOther)
		beta.id = organizationFromLocation(t, created.location)
		readyBeta := p.waitState(t, beta.id, "ready")
		if alpha.id == beta.id || readyAlpha.Endpoint == readyBeta.Endpoint || !readyAlpha.Managed || !readyBeta.Managed || p.provider.creates != 2 {
			t.Fatalf("organizations are not separate: %+v %+v creates=%d", readyAlpha, readyBeta, p.provider.creates)
		}
		for _, organization := range []Organization{readyAlpha, readyBeta} {
			spec := p.launcher.specs[organization.ID]
			if spec.PublicURL != p.base || !strings.HasPrefix(organization.Endpoint, "unix:") || spec.Directory == "" {
				t.Fatalf("tenant %s is not private behind the shared origin: %+v", organization.ID, spec)
			}
		}
		pilotStatus(t, "dana organization sign-in", dana.login("/organizations/"+alpha.id+"/organization", "user_dana:porg_"+alpha.id), http.StatusSeeOther)
		pilotStatus(t, "eve organization sign-in", eve.login("/organizations/"+beta.id+"/organization", "user_eve:porg_"+beta.id), http.StatusSeeOther)
		alpha.owner, beta.owner = dana, eve
		for _, o := range []*pilotOrganization{&alpha, &beta} {
			page := o.owner.get(o.page())
			pilotStatus(t, "owner page", page, http.StatusOK)
			o.ownerCSRF = csrfFrom(t, page.body)
		}
		if alpha.ownerCSRF == beta.ownerCSRF {
			t.Fatal("organizations share a CSRF token")
		}
		foreign := eve.get(alpha.page())
		if foreign.status != http.StatusSeeOther || !strings.HasPrefix(foreign.location, "/auth/oidc/start?organization="+alpha.id) {
			t.Fatalf("eve reached alpha: %d %q", foreign.status, foreign.location)
		}
		if response := eve.login(alpha.page(), "user_eve:porg_"+alpha.id); response.status == http.StatusSeeOther {
			t.Fatalf("eve authenticated into alpha: %q", response.location)
		}
		pilotRecord(t, "self_service", map[string]any{
			"organizations": 2, "provider_organizations_created": p.provider.creates, "operator_registry_calls": 0,
			"tenant_endpoints": "private unix sockets", "injected_failure": failed.ErrorCode, "resumed_to": readyAlpha.State,
			"cross_user_page_status": foreign.status,
		})
	})

	pilotStage(t, "MCP authority survives the shared entry boundary", func() {
		initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"pilot","version":"1"}}}`
		path := "/organizations/" + alpha.id + "/mcp"
		headers := map[string]string{"Content-Type": "application/json", "Origin": p.base, "X-CSRF-Token": alpha.ownerCSRF}
		initialized := alpha.owner.request(http.MethodPost, path, strings.NewReader(initialize), headers)
		pilotStatus(t, "MCP initialize", initialized, http.StatusOK)
		session := initialized.header.Get("Mcp-Session-Id")
		if session == "" {
			t.Fatal("shared entry lost MCP session identity")
		}
		headers["Mcp-Session-Id"] = session
		pilotStatus(t, "MCP initialized notification", alpha.owner.request(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), headers), http.StatusAccepted)
		listed := alpha.owner.request(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), headers)
		pilotStatus(t, "MCP list", listed, http.StatusOK)
		for _, name := range []string{"fleet_health", "telemetry_usage", "recent_activity"} {
			if strings.Contains(listed.body, `"name":"`+name+`"`) {
				t.Fatalf("absent daemon service advertised: %s", listed.body)
			}
		}
		for _, name := range []string{"board_state", "explain_item"} {
			if !strings.Contains(listed.body, `"name":"`+name+`"`) {
				t.Fatalf("native work read %s was not advertised", name)
			}
		}
		call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explain_item","arguments":{"project_id":"foreign","reference":"wi_foreign"}}}`
		denied := alpha.owner.request(http.MethodPost, path, strings.NewReader(call), headers)
		pilotStatus(t, "MCP foreign project", denied, http.StatusOK)
		if !strings.Contains(denied.body, `"isError":true`) {
			t.Fatalf("foreign project authorized: %s", denied.body)
		}
		for _, browser := range []*pilotBrowser{beta.owner, p.browser(t)} {
			response := browser.request(http.MethodPost, path, strings.NewReader(call), headers)
			if response.status < 400 || strings.Contains(response.body, "Alpha secret") {
				t.Fatalf("foreign identity: %d %s", response.status, response.body)
			}
		}
	})

	pilotStage(t, "capacity refusal keeps ready organizations", func() {
		gus := p.browser(t)
		gus.login("/auth/oidc/start", "user_gus:")
		created := gus.createOrganization("Gamma")
		pilotStatus(t, "gus create", created, http.StatusSeeOther)
		gamma := p.waitState(t, organizationFromLocation(t, created.location), "failed")
		if gamma.ErrorCode != "capacity" || gamma.ProviderID != "" || p.provider.creates != 2 {
			t.Fatalf("capacity refusal = %+v creates=%d", gamma, p.provider.creates)
		}
		for _, o := range []pilotOrganization{alpha, beta} {
			pilotStatus(t, "ready organization after refusal", o.owner.get(o.page()), http.StatusOK)
		}
		pilotRecord(t, "capacity", map[string]any{"max_tenants": 2, "refused_state": gamma.State, "error_code": gamma.ErrorCode, "provider_calls_for_refused": 0})
	})

	pilotStage(t, "owners create projects and entitlements stay separate", func() {
		for _, o := range []*pilotOrganization{&alpha, &beta} {
			o.projectName = map[string]string{alpha.id: "Alpha secret project", beta.id: "Beta secret project"}[o.id]
			created := o.owner.form("/organizations/"+o.id+"/projects", url.Values{"name": {o.projectName}, "grant_access": {"true"}, "csrf": {o.ownerCSRF}})
			pilotStatus(t, "project", created, http.StatusSeeOther)
			prefix := "/organizations/" + o.id + "/projects/"
			if !strings.HasPrefix(created.location, prefix) {
				t.Fatalf("project location = %q", created.location)
			}
			o.project = strings.TrimPrefix(created.location, prefix)
		}
		second := alpha.owner.form("/organizations/"+alpha.id+"/projects", url.Values{"name": {"Alpha second project"}, "grant_access": {"true"}, "csrf": {alpha.ownerCSRF}})
		pilotStatus(t, "free plan second project", second, http.StatusTooManyRequests)
		var before hubserver.HostedEntitlement
		report := alpha.owner.get("/organizations/" + alpha.id + "/api/cloud/billing")
		pilotStatus(t, "alpha entitlement", report, http.StatusOK)
		var usage struct {
			Entitlement hubserver.HostedEntitlement `json:"entitlement"`
		}
		pilotDecode(t, report, &usage)
		before = usage.Entitlement
		if before.OrganizationID != alpha.id || before.EffectiveBase.ID != "pilot_free" || len(before.Grants) != 0 {
			t.Fatalf("alpha entitlement = %+v", before)
		}
		grant := map[string]any{"idempotency_key": "alpha-comp-1", "action": "grant", "expected_revision": before.Revision, "plan": map[string]any{"id": "pilot_plus", "version": 1}, "grant_id": "comp_alpha", "scope": []string{"projects"}, "reason": "pilot complimentary access"}
		pilotStatus(t, "entitlement without operator token", p.machine(t, http.MethodPost, alpha.api()+"/entitlements", pilotAdminToken(beta.id), grant), http.StatusForbidden)
		pilotStatus(t, "complimentary grant", p.machine(t, http.MethodPost, alpha.api()+"/entitlements", pilotOperatorToken, grant), http.StatusNoContent)
		granted := alpha.owner.form("/organizations/"+alpha.id+"/projects", url.Values{"name": {"Alpha second project"}, "grant_access": {"true"}, "csrf": {alpha.ownerCSRF}})
		pilotStatus(t, "granted second project", granted, http.StatusSeeOther)
		alphaSecondProject = strings.TrimPrefix(granted.location, "/organizations/"+alpha.id+"/projects/")
		for _, test := range []struct {
			o      pilotOrganization
			grants int
		}{{alpha, 1}, {beta, 0}} {
			response := test.o.owner.get("/organizations/" + test.o.id + "/api/cloud/billing")
			pilotStatus(t, "entitlement report", response, http.StatusOK)
			pilotDecode(t, response, &usage)
			if usage.Entitlement.OrganizationID != test.o.id || len(usage.Entitlement.Grants) != test.grants {
				t.Fatalf("%s entitlement = %+v", test.o.id, usage.Entitlement)
			}
		}
		foreign := beta.owner.get("/organizations/" + alpha.id + "/api/cloud/billing")
		if foreign.status == http.StatusOK || strings.Contains(foreign.body, "comp_alpha") {
			t.Fatalf("beta owner read alpha billing: %d", foreign.status)
		}
		checkout := alpha.owner.form("/organizations/"+alpha.id+"/organization/billing/checkout", url.Values{"price": {"price_unconfigured"}, "csrf": {alpha.ownerCSRF}})
		pilotStatus(t, "checkout without billing provider", checkout, http.StatusServiceUnavailable)
		pilotRecord(t, "entitlements", map[string]any{
			"base_plan": before.EffectiveBase.ID, "free_project_limit_status": http.StatusTooManyRequests, "complimentary_grant_status": http.StatusNoContent,
			"project_after_grant_status": http.StatusSeeOther, "grants_alpha": 1, "grants_beta": 0, "cross_org_billing_status": foreign.status,
			"checkout_without_provider_status": checkout.status, "stripe_configured": false,
		})
	})

	fay := p.browser(t)
	var fayCSRF map[string]string

	pilotStage(t, "third user joins both organizations", func() {
		fayCSRF = map[string]string{}
		for _, o := range []pilotOrganization{alpha, beta} {
			invite := o.owner.form("/organizations/"+o.id+"/organization/invite", url.Values{"email": {"fay@example.test"}, "role": {"member"}, "csrf": {o.ownerCSRF}})
			pilotStatus(t, "invite", invite, http.StatusSeeOther)
			start := fay.get("/invite?invitation_token=inv_fay")
			pilotStatus(t, "invitation start", start, http.StatusSeeOther)
			provider, err := url.Parse(start.location)
			if err != nil {
				t.Fatal(err)
			}
			accepted := fay.get("/auth/oidc/callback?" + url.Values{"code": {"user_fay:"}, "state": {provider.Query().Get("state")}}.Encode())
			if accepted.status != http.StatusSeeOther || !strings.HasPrefix(accepted.location, "/auth/oidc/start?organization="+o.id) {
				t.Fatalf("accepted invitation = %d %q", accepted.status, accepted.location)
			}
			pilotStatus(t, "member sign-in", fay.login(accepted.location, "user_fay:porg_"+o.id), http.StatusSeeOther)
			grant := o.owner.form("/organizations/"+o.id+"/organization/grants", url.Values{"user": {"user_fay"}, "project": {o.project}, "write": {"true"}, "csrf": {o.ownerCSRF}})
			pilotStatus(t, "grant", grant, http.StatusSeeOther)
		}
		for _, o := range []pilotOrganization{alpha, beta} {
			fayCSRF[o.id] = fay.csrf(o.page())
		}
		if fayCSRF[alpha.id] == fayCSRF[beta.id] {
			t.Fatal("member shares one CSRF token across organizations")
		}
		chooser := fay.get("/organizations")
		for _, o := range []pilotOrganization{alpha, beta} {
			if !strings.Contains(chooser.body, `href="/organizations/`+o.id+`/organization"`) {
				t.Fatalf("chooser lacks %s", o.id)
			}
		}
		pilotRecord(t, "membership", map[string]any{"member_organizations": 2, "invitations_accepted": 2, "distinct_csrf_per_organization": true})
	})

	pilotStage(t, "concurrent tabs stay scoped", func() {
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		requests := atomic.Int64{}
		for _, tab := range []struct {
			self, other pilotOrganization
		}{{alpha, beta}, {beta, alpha}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 8 {
					page := fay.get(tab.self.projectAPI())
					requests.Add(1)
					if page.status != http.StatusOK || !strings.Contains(page.body, tab.self.projectName) || strings.Contains(page.body, tab.other.projectName) {
						errs <- fmt.Errorf("tab %s read %d", tab.self.id, page.status)
						return
					}
					issue := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("tab-%s-%d", tab.self.id, i)}, Title: fmt.Sprintf("%s tab issue %d", tab.self.projectName, i), State: "Todo"}
					created := fay.json(http.MethodPost, tab.self.projectAPI()+"/work-items", fayCSRF[tab.self.id], issue)
					requests.Add(1)
					if created.status != http.StatusOK {
						errs <- fmt.Errorf("tab %s create %d: %.200s", tab.self.id, created.status, created.body)
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		for _, test := range []struct{ self, other pilotOrganization }{{alpha, beta}, {beta, alpha}} {
			list := fay.get(test.self.projectAPI() + "/work-items")
			pilotStatus(t, "issue list", list, http.StatusOK)
			if strings.Count(list.body, test.self.projectName+" tab issue") != 8 || strings.Contains(list.body, test.other.projectName) {
				t.Fatalf("%s issues crossed organizations: %.600s", test.self.id, list.body)
			}
		}
		second := p.browser(t)
		second.login(beta.page(), "user_fay:porg_"+beta.id)
		pilotStatus(t, "second session beta", second.get(beta.page()), http.StatusOK)
		if response := second.get(alpha.page()); response.status != http.StatusSeeOther {
			t.Fatalf("second session reached alpha without authorizing it: %d", response.status)
		}
		pilotRecord(t, "concurrent_tabs", map[string]any{"tabs": 2, "requests": requests.Load(), "issues_per_organization": 8, "cross_organization_items": 0, "independent_session_scoped": true})
	})

	pilotStage(t, "mutations reject the other organization", func() {
		issue := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "cross"}, Title: "Cross organization attempt", State: "Todo"}
		results := map[string]int{}
		for _, test := range []struct {
			name, path, csrf string
			want             int
		}{
			{"other organization token", alpha.projectAPI() + "/work-items", fayCSRF[beta.id], http.StatusForbidden},
			{"project under other organization", "/api/v2/organizations/" + beta.id + "/projects/" + alpha.project + "/work-items", fayCSRF[beta.id], http.StatusNotFound},
			{"missing token", alpha.projectAPI() + "/work-items", "", http.StatusForbidden},
		} {
			response := fay.json(http.MethodPost, test.path, test.csrf, issue)
			pilotStatus(t, test.name, response, test.want)
			results[test.name] = response.status
		}
		cross := fay.request(http.MethodPost, alpha.projectAPI()+"/work-items", jsonBody(t, issue), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": fayCSRF[alpha.id], "Origin": "https://attacker.example.test", "Sec-Fetch-Site": "cross-site"})
		pilotStatus(t, "cross-origin mutation", cross, http.StatusForbidden)
		results["cross-origin"] = cross.status
		opaque := fay.request(http.MethodPost, alpha.projectAPI()+"/work-items", jsonBody(t, issue), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": fayCSRF[alpha.id], "Sec-Fetch-Site": "cross-site"})
		pilotStatus(t, "cross-site opaque origin", opaque, http.StatusForbidden)
		results["cross-site opaque origin"] = opaque.status
		for _, o := range []pilotOrganization{alpha, beta} {
			if list := fay.get(o.projectAPI() + "/work-items"); strings.Contains(list.body, "Cross organization attempt") {
				t.Fatalf("rejected mutation reached %s", o.id)
			}
		}
		pilotRecord(t, "mutations", results)
	})

	var alphaRunner, betaRunner pilotRunner
	var alphaIssue tracker.NativeIssue

	pilotStage(t, "runners enroll and execute within one organization", func() {
		for _, o := range []pilotOrganization{alpha, beta} {
			pilotStatus(t, "runner grant", o.owner.form("/organizations/"+o.id+"/organization/grants", url.Values{"user": {"user_" + map[string]string{alpha.id: "dana", beta.id: "eve"}[o.id]}, "project": {o.project}, "write": {"true"}, "runner": {"true"}, "csrf": {o.ownerCSRF}}), http.StatusSeeOther)
		}
		pilotStatus(t, "second project runner grant", alpha.owner.form("/organizations/"+alpha.id+"/organization/grants", url.Values{"user": {"user_dana"}, "project": {alphaSecondProject}, "write": {"true"}, "runner": {"true"}, "csrf": {alpha.ownerCSRF}}), http.StatusSeeOther)
		pilotStatus(t, "foreign enrollment", beta.owner.json(http.MethodPost, alpha.api()+"/runner-enrollments", beta.ownerCSRF, runnerauth.EnrollmentRequest{Binding: runnerauth.NewBinding(), ProjectIDs: []tracker.ProjectID{tracker.ProjectID(alpha.project)}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}), http.StatusUnauthorized)
		alphaRunner = p.enrollRunner(t, alpha, beta, "alpha-runner.example.test")
		betaRunner = p.enrollRunner(t, beta, alpha, "beta-runner.example.test")
		identity := map[string]int{}
		for _, test := range []struct {
			name   string
			runner pilotRunner
			o      pilotOrganization
			want   int
		}{
			{"alpha runner at alpha", alphaRunner, alpha, http.StatusOK},
			{"alpha runner at beta", alphaRunner, beta, http.StatusUnauthorized},
			{"beta runner at alpha", betaRunner, alpha, http.StatusUnauthorized},
			{"beta runner at beta", betaRunner, beta, http.StatusOK},
		} {
			response := p.machine(t, http.MethodGet, test.o.api()+"/runners/"+test.runner.RunnerID, test.runner.credential, nil)
			pilotStatus(t, test.name, response, test.want)
			identity[test.name] = response.status
		}
		descriptor := pilotPolicy()
		pilotStatus(t, "policy", alpha.owner.json(http.MethodPut, alpha.projectAPI()+"/onboarding/policy", alpha.ownerCSRF, policy.Change{Policy: descriptor}), http.StatusOK)
		created := alpha.owner.json(http.MethodPost, alpha.projectAPI()+"/work-items", alpha.ownerCSRF, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "runner-work"}, Title: "Runner work", State: "Todo"})
		pilotStatus(t, "runner issue", created, http.StatusOK)
		pilotDecode(t, created, &alphaIssue)
		claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: alphaIssue.WorkItemID, MachineID: alphaRunner.MachineID, SessionID: "shared-origin", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
		pilotStatus(t, "claim with foreign runner", p.machine(t, http.MethodPost, alpha.projectAPI()+"/claims", betaRunner.credential, claim), http.StatusUnauthorized)
		pilotStatus(t, "claim under foreign organization", p.machine(t, http.MethodPost, "/api/v2/organizations/"+beta.id+"/projects/"+alpha.project+"/claims", alphaRunner.credential, claim), http.StatusUnauthorized)
		response := p.machine(t, http.MethodPost, alpha.projectAPI()+"/claims", alphaRunner.credential, claim)
		pilotStatus(t, "claim", response, http.StatusOK)
		var lease tracker.NativeLease
		pilotDecode(t, response, &lease)
		run, attempt := "run_"+strings.Repeat("a", 32), "attempt_"+strings.Repeat("b", 32)
		start := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
			Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}, LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: run, AttemptID: attempt}}
		finish := start
		finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
		events := alpha.projectAPI() + "/work-items/" + string(alphaIssue.WorkItemID) + "/events"
		for _, event := range []tracker.NativeRunEvent{start, finish} {
			pilotStatus(t, event.Type, p.machine(t, http.MethodPost, events, alphaRunner.credential, event), http.StatusOK)
		}
		pilotStatus(t, "release", p.machine(t, http.MethodPost, alpha.projectAPI()+"/leases/"+string(lease.ID)+"/release", alphaRunner.credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		attempts := alpha.owner.get(alpha.projectAPI() + "/work-items/" + string(alphaIssue.WorkItemID) + "/attempts")
		pilotStatus(t, "attempt history", attempts, http.StatusOK)
		if !strings.Contains(attempts.body, "succeeded") {
			t.Fatalf("attempt history = %.400s", attempts.body)
		}
		pilotStatus(t, "foreign owner reads attempts", beta.owner.get(alpha.projectAPI()+"/work-items/"+string(alphaIssue.WorkItemID)+"/attempts"), http.StatusUnauthorized)
		pilotRecord(t, "runners", map[string]any{"identity": identity, "foreign_enrollment_status": http.StatusUnauthorized, "claim_status": http.StatusOK, "run_outcome": "succeeded", "lease_released": true})
	})

	pilotStage(t, "artifact endpoints stay scoped", func() {
		item := alpha.projectAPI() + "/work-items/" + string(alphaIssue.WorkItemID)
		results := map[string]int{}
		for _, test := range []struct {
			name string
			do   func() pilotResponse
			want int
		}{
			{"member lists alpha artifacts", func() pilotResponse { return fay.get(item + "/artifacts") }, http.StatusOK},
			{"alpha item under beta", func() pilotResponse {
				return fay.get("/api/v2/organizations/" + beta.id + "/projects/" + alpha.project + "/work-items/" + string(alphaIssue.WorkItemID) + "/artifacts")
			}, http.StatusNotFound},
			{"read grant with beta token", func() pilotResponse {
				return fay.json(http.MethodPost, item+"/artifacts/artifact_missing/access", fayCSRF[beta.id], map[string]int64{"revision": 1})
			}, http.StatusForbidden},
			{"beta owner requests alpha grant", func() pilotResponse {
				return beta.owner.json(http.MethodPost, item+"/artifacts/artifact_missing/access", beta.ownerCSRF, map[string]int64{"revision": 1})
			}, http.StatusUnauthorized},
			{"beta runner requests alpha upload authority", func() pilotResponse {
				return p.machine(t, http.MethodPost, item+"/artifact-authority", betaRunner.credential, map[string]string{})
			}, http.StatusUnauthorized},
			{"beta owner lists alpha artifact services", func() pilotResponse { return beta.owner.get(alpha.projectAPI() + "/artifact-services") }, http.StatusUnauthorized},
		} {
			response := test.do()
			pilotStatus(t, test.name, response, test.want)
			results[test.name] = response.status
		}
		pilotRecord(t, "artifacts", results)
	})

	pilotStage(t, "event streams stay scoped and end on revocation", func() {
		results := map[string]int{}
		for _, test := range []struct {
			name, path string
			want       int
		}{
			{"alpha project under beta", "/organizations/" + beta.id + "/projects/" + alpha.project + "/events", http.StatusForbidden},
			{"beta project under alpha", "/organizations/" + alpha.id + "/projects/" + beta.project + "/events", http.StatusForbidden},
		} {
			stream := fay.stream(t, test.path)
			if stream.status != test.want {
				t.Fatalf("%s = %d, want %d", test.name, stream.status, test.want)
			}
			results[test.name] = stream.status
		}
		if foreign := beta.owner.stream(t, "/organizations/"+alpha.id+"/projects/"+alpha.project+"/events"); foreign.status == http.StatusOK {
			t.Fatal("beta owner streamed alpha events")
		} else {
			results["beta owner streams alpha"] = foreign.status
		}
		streams := map[string]pilotStream{}
		for _, o := range []pilotOrganization{alpha, beta} {
			stream := fay.stream(t, "/organizations/"+o.id+"/projects/"+o.project+"/events")
			if stream.status != http.StatusOK || !strings.HasPrefix(stream.contentType, "text/event-stream") {
				t.Fatalf("%s stream = %d %q", o.id, stream.status, stream.contentType)
			}
			if line, open, err := stream.next(10 * time.Second); err != nil || !open || !strings.HasPrefix(line, "data:") {
				t.Fatalf("%s first event = %q %v %v", o.id, line, open, err)
			}
			streams[o.id] = stream
		}
		p.provider.removeMember("user_fay", "porg_"+alpha.id)
		p.provider.expireAccess()
		if response := fay.get(alpha.page()); response.status == http.StatusOK {
			t.Fatal("removed member still reads alpha after the access token refresh")
		}
		ended := false
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
			if _, open, _ := streams[alpha.id].next(time.Second); !open {
				ended = true
				break
			}
		}
		if !ended {
			t.Fatal("alpha stream continued after membership removal")
		}
		if line, open, err := streams[beta.id].next(10 * time.Second); err != nil || !open || !strings.HasPrefix(line, "data:") {
			t.Fatalf("beta stream after alpha revocation = %q %v %v", line, open, err)
		}
		if response := fay.get(alpha.page()); response.status == http.StatusOK {
			t.Fatal("removed member still reads alpha")
		}
		pilotStatus(t, "beta after alpha revocation", fay.get(beta.page()), http.StatusOK)
		pilotRecord(t, "sse", map[string]any{"denials": results, "alpha_stream_ended_after_membership_removal": ended, "beta_stream_continued": true})
	})

	pilotStage(t, "sign-out ends one browser session only", func() {
		other := p.browser(t)
		other.login(beta.page(), "user_fay:porg_"+beta.id)
		logout := fay.form("/organizations/"+beta.id+"/logout", url.Values{"csrf": {fayCSRF[beta.id]}})
		pilotStatus(t, "logout", logout, http.StatusSeeOther)
		if response := fay.get(beta.page()); response.status != http.StatusSeeOther {
			t.Fatalf("signed-out browser still reads beta: %d", response.status)
		}
		pilotStatus(t, "other browser after logout", other.get(beta.page()), http.StatusOK)
		pilotRecord(t, "sign_out", map[string]any{"signed_out_browser_status": http.StatusSeeOther, "other_browser_status": http.StatusOK})
	})

	pilotStage(t, "entry restart relaunches tenants and keeps sessions", func() {
		starts := p.launcher.starts
		if err := p.service.Close(); err != nil {
			t.Fatal(err)
		}
		p.open(t, 2, 1)
		deadline := time.Now().Add(30 * time.Second)
		for _, tenant := range []struct {
			name    string
			org     pilotOrganization
			content string
		}{{"alpha", alpha, "Alpha secret project"}, {"beta", beta, ""}} {
			for {
				response := tenant.org.owner.get(tenant.org.page())
				if response.status == http.StatusOK && strings.Contains(response.body, tenant.content) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s after restart = %d: %.200s", tenant.name, response.status, response.body)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		if p.launcher.starts <= starts {
			t.Fatalf("restart did not relaunch tenants: %d -> %d", starts, p.launcher.starts)
		}
		pilotRecord(t, "restart", map[string]any{"tenant_starts_before": starts, "tenant_starts_after": p.launcher.starts, "sessions_survived": true})
	})
}

func (p *sharedOriginPilot) previewAuthorize(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	p.provider.mu.Lock()
	type choice struct{ label, code string }
	var choices []choice
	for _, user := range []string{"dana", "eve", "fay", "gus", "hal"} {
		subject := "user_" + user
		if _, ok := p.provider.users[subject]; !ok {
			continue
		}
		choices = append(choices, choice{user + "@example.test (account)", subject + ":"})
		for _, membership := range p.provider.memberships {
			if membership.UserID == subject {
				choices = append(choices, choice{user + "@example.test into " + membership.OrganizationID, subject + ":" + membership.OrganizationID})
			}
		}
	}
	p.provider.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var page strings.Builder
	page.WriteString(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Fixture identity provider</title></head><body style="font-family:sans-serif;padding:16px"><h1>Fixture identity provider</h1><p>Synthetic accounts only. No WorkOS request is made.</p><ul>`)
	for _, item := range choices {
		fmt.Fprintf(&page, `<li><a href="/auth/oidc/callback?%s">%s</a></li>`, url.Values{"code": {item.code}, "state": {state}}.Encode(), item.label)
	}
	page.WriteString(`</ul></body></html>`)
	_, _ = io.WriteString(w, page.String())
}

func seedSharedOriginPreview(t *testing.T, p *sharedOriginPilot) map[string]string {
	t.Helper()
	organizations := map[string]pilotOrganization{}
	for _, seed := range []struct{ user, name, project string }{{"dana", "Alpha Labs", "Alpha secret project"}, {"eve", "Beta Works", "Beta secret project"}} {
		owner := p.browser(t)
		owner.login("/auth/oidc/start", "user_"+seed.user+":")
		created := owner.createOrganization(seed.name)
		id := organizationFromLocation(t, created.location)
		p.waitState(t, id, "ready")
		owner.login("/organizations/"+id+"/organization", "user_"+seed.user+":porg_"+id)
		o := pilotOrganization{id: id, owner: owner, projectName: seed.project}
		o.ownerCSRF = owner.csrf(o.page())
		project := owner.form("/organizations/"+id+"/projects", url.Values{"name": {seed.project}, "grant_access": {"true"}, "csrf": {o.ownerCSRF}})
		o.project = strings.TrimPrefix(project.location, "/organizations/"+id+"/projects/")
		pilotStatus(t, "invite", owner.form("/organizations/"+id+"/organization/invite", url.Values{"email": {"fay@example.test"}, "role": {"member"}, "csrf": {o.ownerCSRF}}), http.StatusSeeOther)
		fay := p.browser(t)
		start := fay.get("/invite?invitation_token=inv_fay")
		provider, err := url.Parse(start.location)
		if err != nil {
			t.Fatal(err)
		}
		fay.get("/auth/oidc/callback?" + url.Values{"code": {"user_fay:"}, "state": {provider.Query().Get("state")}}.Encode())
		pilotStatus(t, "grant", owner.form("/organizations/"+id+"/organization/grants", url.Values{"user": {"user_fay"}, "project": {o.project}, "write": {"true"}, "csrf": {o.ownerCSRF}}), http.StatusSeeOther)
		issue := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "seed"}, Title: seed.name + " native issue", State: "Todo"}
		pilotStatus(t, "issue", owner.json(http.MethodPost, o.projectAPI()+"/work-items", o.ownerCSRF, issue), http.StatusOK)
		organizations[seed.user] = o
	}
	p.provider.mu.Lock()
	p.provider.users["user_hal"] = "hal@example.test"
	p.provider.mu.Unlock()
	alpha, beta := organizations["dana"], organizations["eve"]
	return map[string]string{
		"origin": p.base, "alpha": alpha.id, "beta": beta.id,
		"alpha_project": p.base + "/organizations/" + alpha.id + "/projects/" + alpha.project,
		"beta_project":  p.base + "/organizations/" + beta.id + "/projects/" + beta.project,
		"stop":          p.base + "/__preview/stop",
	}
}

// TestSharedOriginPilotPreview serves the shared-origin fixture on an
// ephemeral loopback port for browser screenshots. It is skipped unless
// DETENT_SHARED_ORIGIN_PREVIEW names a private directory for the fixture file.
func TestSharedOriginPilotPreview(t *testing.T) {
	directory := os.Getenv("DETENT_SHARED_ORIGIN_PREVIEW")
	if directory == "" {
		t.Skip("set DETENT_SHARED_ORIGIN_PREVIEW to a private directory to serve the isolated shared-origin browser preview")
	}
	p := newSharedOriginPilot(t, 3, 3)
	stop := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__preview/authorize", p.previewAuthorize)
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(stop) })
		w.WriteHeader(http.StatusNoContent)
	})
	service := p.service.Handler()
	mux.Handle("/", service)
	var handler http.Handler = mux
	p.handler.handler.Store(&handler)
	fixture := seedSharedOriginPreview(t, p)
	p.provider.mu.Lock()
	p.provider.authorizeBase = p.base
	p.provider.mu.Unlock()
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "shared-origin-preview.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("shared-origin preview: %s", p.base)
	timer := time.NewTimer(20 * time.Minute)
	defer timer.Stop()
	select {
	case <-stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}

func TestSharedOriginAPIKeyConnection(t *testing.T) {
	p := newSharedOriginPilot(t, 1, 1)
	owner := p.browser(t)
	pilotStatus(t, "sign-in", owner.login("/auth/oidc/start", "user_dana:"), http.StatusSeeOther)
	created := owner.createOrganization("API key organization")
	pilotStatus(t, "organization", created, http.StatusSeeOther)
	o := pilotOrganization{id: organizationFromLocation(t, created.location), owner: owner}
	p.waitState(t, o.id, "ready")
	pilotStatus(t, "organization sign-in", owner.login(o.page(), "user_dana:porg_"+o.id), http.StatusSeeOther)
	o.ownerCSRF = owner.csrf(o.page())
	project := owner.form("/organizations/"+o.id+"/projects", url.Values{"name": {"API key project"}, "grant_access": {"true"}, "csrf": {o.ownerCSRF}})
	pilotStatus(t, "project", project, http.StatusSeeOther)
	o.project = strings.TrimPrefix(project.location, "/organizations/"+o.id+"/projects/")
	mount := "/organizations/" + o.id
	api := mount + o.api()
	pilotStatus(t, "seed readable work", owner.json(http.MethodPost, api+"/projects/"+o.project+"/work-items", o.ownerCSRF, map[string]any{"title": "Shared key read", "state": "Todo", "idempotency_key": "shared-key-read"}), http.StatusOK)
	issued := owner.json(http.MethodPost, api+"/api-keys", o.ownerCSRF, map[string]any{"name": "external-client", "scope": "read", "expires_days": 7, "project_ids": []string{o.project}})
	pilotStatus(t, "create shared key", issued, http.StatusCreated)
	var key struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Expiry string `json:"expires_at"`
	}
	pilotDecode(t, issued, &key)
	if key.ID == "" || key.Token == "" || key.Expiry == "" {
		t.Fatal("expiring key missing")
	}
	external := p.browser(t)
	external.client.Jar = nil
	headers := map[string]string{"Authorization": "Bearer " + key.Token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Origin": ""}
	read := external.request(http.MethodGet, api+"/projects/"+o.project, nil, headers)
	pilotStatus(t, "direct API read", read, http.StatusOK)
	if !strings.Contains(read.body, o.project) {
		t.Fatal("project context missing")
	}
	pilotStatus(t, "key read cannot write", external.request(http.MethodPost, api+"/projects/"+o.project+"/work-items", strings.NewReader(`{"title":"denied","state":"Todo","idempotency_key":"denied"}`), headers), http.StatusForbidden)
	initialized := external.request(http.MethodPost, mount+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"shared-key-regression","version":"1"}}}`), headers)
	pilotStatus(t, "MCP initialize", initialized, http.StatusOK)
	session := initialized.header.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatal("MCP session missing")
	}
	headers["Mcp-Session-Id"] = session
	headers["Mcp-Protocol-Version"] = "2025-11-25"
	pilotStatus(t, "MCP initialized", external.request(http.MethodPost, mount+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), headers), http.StatusAccepted)
	listed := external.request(http.MethodPost, mount+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), headers)
	pilotStatus(t, "MCP tools", listed, http.StatusOK)
	if !strings.Contains(listed.body, `"work_list"`) {
		t.Fatal("authorized read tool missing")
	}
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"work_list","arguments":{"project_id":"` + o.project + `","limit":20}}}`
	result := external.request(http.MethodPost, mount+"/mcp", strings.NewReader(call), headers)
	pilotStatus(t, "MCP authorized read", result, http.StatusOK)
	if !strings.Contains(result.body, o.project) || strings.Contains(result.body, `"isError":true`) {
		t.Fatal("authorized MCP read failed")
	}
	pilotStatus(t, "foreign organization", external.request(http.MethodGet, "/organizations/org_foreign/api/v2/organizations/org_foreign/projects/"+o.project, nil, headers), http.StatusNotFound)
	pilotStatus(t, "revoke shared key", owner.json(http.MethodDelete, api+"/api-keys/"+key.ID, o.ownerCSRF, nil), http.StatusNoContent)
	pilotStatus(t, "revoked API key", external.request(http.MethodGet, api+"/projects/"+o.project, nil, headers), http.StatusUnauthorized)
	pilotStatus(t, "revoked MCP session", external.request(http.MethodPost, mount+"/mcp", strings.NewReader(call), headers), http.StatusUnauthorized)
}
