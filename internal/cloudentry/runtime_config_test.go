//go:build unix

package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type configurationLog struct{ records chan slog.Record }

func (h configurationLog) Enabled(context.Context, slog.Level) bool { return true }
func (h configurationLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h configurationLog) WithGroup(string) slog.Handler            { return h }
func (h configurationLog) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "cloud configuration reloaded" || strings.Contains(record.Message, "cloud configuration reload failed") {
		h.records <- record.Clone()
	}
	return nil
}

func entitlementFixture(t *testing.T, f entryFixture, token string) {
	t.Helper()
	for id, tenant := range f.fixtures {
		if err := tenant.service.Close(); err != nil {
			t.Fatal(err)
		}
		tenant.config.Hosted.EntitlementAdministrator = "test-operator"
		tenant.config.Hosted.EntitlementAdminToken = []byte(token)
		service, err := hubserver.Open(t.Context(), tenant.config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
		f.tenants[testSocketEndpoint(strings.TrimPrefix(id, "org_")+".sock")] = service.Handler()
	}
}

func TestCloudReloadSignupAllowlist(t *testing.T) {
	f := newEntryFixture(t)
	launcher := &silentLauncher{running: map[string]bool{"org_alpha": true, "org_beta": true}}
	f.service.config.Allocation = &AllocationConfig{
		TenantRoot: t.TempDir(), SocketRoot: t.TempDir(), MaxTenants: 2, MaxConcurrent: 1, RetryLimit: 5,
		Launcher: launcher, AllowedEmails: []string{"alice@example.test"},
	}
	f.service.config.ConfigureRuntime = func(update hubserver.HostedRuntimeUpdate) func(TenantSpec) ([]byte, error) {
		return func(TenantSpec) ([]byte, error) { return json.Marshal(update) }
	}
	next := f.service.config
	next.Runtime = &hubserver.HostedRuntimeUpdate{}
	f.service.config.Reload = func() (Config, error) { return next, nil }
	f.provider.users["user_staff"] = "staff@example.test"
	staff := newBrowser(t, f.service.Handler())
	staff.login("/auth/oidc/start", "user_staff:")
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")

	check := func(t *testing.T, emails, domains []string, allowed []bool) {
		t.Helper()
		for i, email := range []string{"alice@example.test", "bob@example.test", "member@other.test"} {
			got, err := f.service.canCreate(t.Context(), accountSession{Email: email})
			if err != nil || got != allowed[i] {
				t.Errorf("canCreate(%s) = %v, %v; want %v", email, got, err, allowed[i])
			}
		}
		response, body := staff.get("/api/cloud/platform/allowlist")
		var result struct {
			SelfService    bool     `json:"self_service"`
			Open           bool     `json:"open"`
			AllowedEmails  []string `json:"allowed_emails"`
			AllowedDomains []string `json:"allowed_domains"`
		}
		decodeJSON(t, body, &result)
		if response.StatusCode != http.StatusOK || !result.SelfService || result.Open != (len(emails)+len(domains) == 0) ||
			!reflect.DeepEqual(result.AllowedEmails, emails) || !reflect.DeepEqual(result.AllowedDomains, domains) {
			t.Errorf("allowlist = %d %s; want emails %v, domains %v", response.StatusCode, body, emails, domains)
		}
	}
	check(t, []string{"alice@example.test"}, []string{}, []bool{true, false, false})

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for reader := range 2 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := f.service.canCreate(t.Context(), accountSession{Email: "bob@example.test"}); err != nil {
						t.Error(err)
					}
					if reader == 1 {
						response, _ := staff.get("/api/cloud/platform/allowlist")
						if response.StatusCode != http.StatusOK {
							t.Errorf("concurrent allowlist read = %d", response.StatusCode)
							return
						}
					}
				}
			}
		})
	}
	defer func() { close(stop); readers.Wait() }()
	for _, test := range []struct {
		name    string
		emails  []string
		domains []string
		allowed []bool
		invalid bool
	}{
		{name: "empty", emails: []string{}, domains: []string{}, allowed: []bool{true, true, true}},
		{name: "wider", emails: []string{"alice@example.test", "bob@example.test"}, domains: []string{"other.test"}, allowed: []bool{true, true, true}},
		{name: "narrower", emails: []string{"bob@example.test"}, domains: []string{}, allowed: []bool{false, true, false}},
		{name: "invalid", emails: []string{}, domains: []string{}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			allocation := *f.service.config.Allocation
			allocation.AllowedEmails, allocation.AllowedDomains = test.emails, test.domains
			next.Allocation = &allocation
			next.PublicURL = "https://changed.example.test"
			if test.invalid {
				next.PublicURL = "invalid"
			}
			err := f.service.reloadConfiguration(t.Context())
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "validate cloud configuration") {
					t.Fatalf("invalid reload error = %v", err)
				}
				check(t, []string{"bob@example.test"}, []string{}, []bool{false, true, false})
			} else {
				if err != nil {
					t.Fatal(err)
				}
				check(t, test.emails, test.domains, test.allowed)
			}
			if f.service.config.PublicURL != testPublicURL {
				t.Error("reload changed restart-only public URL")
			}
			if launches, stops, running := launcher.counts(); launches != 0 || stops != 0 || running != 2 {
				t.Errorf("reload changed tenant processes: launches=%d stops=%d running=%d", launches, stops, running)
			}
			response, body := alice.get("/organizations/org_alpha/organization")
			if response.StatusCode != http.StatusOK || !strings.Contains(body, "Alpha secret project") {
				t.Errorf("existing organization session = %d %s", response.StatusCode, body)
			}
		})
	}
}

func TestCloudSIGHUPRetainsInvalidAndAppliesValidRuntime(t *testing.T) {
	f := newEntryFixture(t)
	const token = "runtime-entitlement-admin-0123456789abcdef"
	for id, tenant := range f.fixtures {
		tenant.config.Hosted.Billing = &hubserver.HostedBillingConfig{
			Mode: "test", AccountID: "acct_runtime", CustomerID: "cus_" + id, PortalConfigurationID: "bpc_runtime",
			WebhookSecret: []byte(testWebhookSecret), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: &fakeBilling{},
			Prices: []hubserver.HostedBillingPrice{{PriceID: "price_initial", Label: "Initial plan", Plan: tenant.config.Hosted.Plans.Plans[1].PlanReference}},
		}
	}
	entitlementFixture(t, f, token)
	for _, tenant := range f.fixtures {
		if err := f.service.linkRuntimePublication(filepath.Dir(tenant.config.DatabasePath)); err != nil {
			t.Fatal(err)
		}
	}
	allocation := &AllocationConfig{TenantRoot: t.TempDir(), SocketRoot: t.TempDir(), MaxTenants: 2, MaxConcurrent: 1, RetryLimit: 5, Launcher: &ExecLauncher{}, EntitlementAdminToken: []byte(token)}
	f.service.config.Allocation = allocation
	f.service.config.ConfigureRuntime = func(update hubserver.HostedRuntimeUpdate) func(TenantSpec) ([]byte, error) {
		return func(TenantSpec) ([]byte, error) { return json.Marshal(update) }
	}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed=1"); err != nil {
		t.Fatal(err)
	}
	type reading struct {
		config Config
		err    error
	}
	readings := make(chan reading, 1)
	f.service.config.Reload = func() (Config, error) { next := <-readings; return next.config, next.err }
	signals := make(chan os.Signal, 1)
	f.service.config.reloadSignals = signals
	logs := configurationLog{records: make(chan slog.Record, 2)}
	f.service.config.Logger = slog.New(logs)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.service.serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	awaitLog := func(want string) {
		t.Helper()
		select {
		case record := <-logs.records:
			if !strings.Contains(record.Message, want) {
				t.Fatalf("reload log=%s", record.Message)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("reload signal was not handled")
		}
	}
	readings <- reading{err: errors.New("invalid cloud file")}
	signals <- syscall.SIGHUP
	awaitLog("reload failed")
	if f.service.runtime.Load() != nil {
		t.Fatal("invalid file replaced runtime")
	}
	plans := *f.fixtures["org_alpha"].config.Hosted.Plans
	plan := plans.Plans[1]
	plan.Version++
	plans.Plans = append(plans.Plans, plan)
	next := f.service.config
	next.PublicURL = "https://changed.example.test"
	next.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &plans, Prices: f.fixtures["org_alpha"].config.Hosted.Billing.Prices}
	readings <- reading{config: next}
	signals <- syscall.SIGHUP
	awaitLog("configuration reloaded")
	if f.service.runtime.Load() == nil || f.service.config.PublicURL != testPublicURL {
		t.Fatal("reload did not publish runtime or changed restart-only origin")
	}
	for _, id := range []string{"org_alpha", "org_beta"} {
		org, err := f.service.registry.Organization(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		report, err := f.service.tenantEntitlements(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range report.Plans {
			found = found || p.ID == plan.ID && p.Version == plan.Version
		}
		if !found {
			t.Fatalf("%s did not receive reloaded plan", id)
		}
	}
	readRuntime := func(id string) hubserver.HostedRuntimeUpdate {
		t.Helper()
		organization, err := f.service.registry.Organization(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		status, body, err := f.service.machineCall(t.Context(), organization, http.MethodGet, tenantEntitlementsPath(id)+"/runtime", nil, token)
		var result hubserver.HostedRuntimeUpdate
		if err != nil || status != http.StatusOK {
			t.Fatalf("read runtime: %d %v", status, err)
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	beta, err := f.service.registry.Organization(t.Context(), "org_beta")
	if err != nil {
		t.Fatal(err)
	}
	priorBeta := readRuntime("org_beta")
	priorBeta.Plans.WindowSeconds = 7200
	priorBeta.Reason = "tenant-specific window policy"
	if err := f.service.changeTenantRuntime(t.Context(), beta, &priorBeta); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"later tenant refused", "reply lost after commit", "request canceled after commit", "persistent outage after prepare", "publication file refused"} {
		t.Run(failure, func(t *testing.T) {
			before := map[string]hubserver.HostedRuntimeUpdate{"org_alpha": readRuntime("org_alpha"), "org_beta": readRuntime("org_beta")}
			entryBefore := f.service.runtime.Load()
			candidate := *next.Runtime.Plans
			candidate.Plans = append([]hubserver.HostedPlan(nil), candidate.Plans...)
			stagedPlan := candidate.Plans[1]
			stagedPlan.Version += 10
			candidate.Plans = append(candidate.Plans, stagedPlan)
			candidate.WindowSeconds = 240
			attempt := next
			attempt.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &candidate, Prices: next.Runtime.Prices}
			attemptCtx, cancelAttempt := context.WithCancel(t.Context())
			defer cancelAttempt()
			original := f.service.config.transport
			defer func() { f.service.config.transport = original }()
			failed := false
			if failure == "publication file refused" {
				path := f.service.runtimePublicationPath() + ".tmp"
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(path); err != nil {
						t.Error(err)
					}
				})
			}
			f.service.config.transport = func(organization Organization) (http.RoundTripper, error) {
				transport, err := original(organization)
				if err != nil || organization.ID != "org_beta" {
					return transport, err
				}
				return runtimeTransport(func(r *http.Request) (*http.Response, error) {
					if failed && failure == "persistent outage after prepare" {
						return nil, errors.New("persistent tenant transport outage")
					}
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/entitlements/runtime") {
						return transport.RoundTrip(r)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var update hubserver.HostedRuntimeUpdate
					if json.Unmarshal(body, &update) == nil && !update.ValidateOnly && !failed && failure != "publication file refused" {
						failed = true
						if applied := readRuntime("org_alpha"); applied.Plans.WindowSeconds != before["org_alpha"].Plans.WindowSeconds {
							t.Errorf("prepared tenant exposed unpublished settings: window=%d", applied.Plans.WindowSeconds)
						}
						if failure != "later tenant refused" {
							applied, err := transport.RoundTrip(r)
							if err != nil {
								return nil, err
							}
							if applied.StatusCode != http.StatusOK {
								t.Errorf("fault injection did not commit: %d", applied.StatusCode)
							}
							if err := applied.Body.Close(); err != nil {
								return nil, err
							}
						}
						if failure == "request canceled after commit" {
							cancelAttempt()
						}
						return nil, errors.New("tenant transport unavailable")
					}
					return transport.RoundTrip(r)
				}), nil
			}
			readings <- reading{config: attempt}
			err := f.service.reloadConfiguration(attemptCtx)
			wantError := "tenant org_beta runtime reload"
			if failure == "publication file refused" {
				wantError = "publish runtime settings"
			} else if !failed {
				t.Fatal("tenant preparation failure was not injected")
			}
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("reload failure was not reported: %v", err)
			}
			f.service.config.transport = original
			if f.service.currentRuntime() != entryBefore {
				t.Fatal("failed reload published new tenant launch settings")
			}
			for _, id := range []string{"org_alpha", "org_beta"} {
				organization, err := f.service.registry.Organization(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				report, err := f.service.tenantEntitlements(t.Context(), organization)
				if err != nil {
					t.Fatal(err)
				}
				for _, plan := range report.Plans {
					if plan.ID == stagedPlan.ID && plan.Version == stagedPlan.Version {
						t.Fatal("failed reload exposed a prepared plan")
					}
				}
				if after := readRuntime(id); !reflect.DeepEqual(after, before[id]) {
					t.Fatalf("%s kept a partial reload: window=%d, want %d", id, after.Plans.WindowSeconds, before[id].Plans.WindowSeconds)
				}
			}
		})
	}
	retry := next
	candidate := *next.Runtime.Plans
	candidate.Plans = append([]hubserver.HostedPlan(nil), candidate.Plans...)
	stagedPlan := candidate.Plans[1]
	stagedPlan.Version += 10
	candidate.Plans = append(candidate.Plans, stagedPlan)
	candidate.WindowSeconds = 240
	retry.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &candidate, Prices: next.Runtime.Prices}
	readings <- reading{config: retry}
	if err := f.service.reloadConfiguration(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"org_alpha", "org_beta"} {
		if state := readRuntime(id); state.Plans.WindowSeconds != 240 {
			t.Fatalf("successful retry did not select the publication: tenant=%s window=%d", id, state.Plans.WindowSeconds)
		}
		organization, err := f.service.registry.Organization(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		report, err := f.service.tenantEntitlements(t.Context(), organization)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, plan := range report.Plans {
			found = found || plan.ID == stagedPlan.ID && plan.Version == stagedPlan.Version
		}
		if !found {
			t.Fatal("successful retry did not expose the prepared plan")
		}
	}

	for index, scenario := range []struct {
		name, step, pause      string
		refuse, publishFailure bool
	}{
		{name: "tenant starting during publication", step: "tenant_files", pause: "/internal/v1/health"},
		{name: "tenant resuming during publication", step: "tenant_start", pause: "/internal/v1/owner/bootstrap"},
		{name: "joining tenant preparation refused", step: "tenant_files", pause: "/internal/v1/health", refuse: true},
		{name: "joining tenant publication refused", step: "tenant_files", pause: "/internal/v1/health", publishFailure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before := readRuntime("org_beta")
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='allocating', step=? WHERE id='org_beta'", scenario.step); err != nil {
				t.Fatal(err)
			}
			organization, err := f.service.registry.Organization(t.Context(), "org_beta")
			if err != nil {
				t.Fatal(err)
			}
			organization.CreatorSubject, organization.CreatorEmail = "user_alice", "alice@example.test"
			allocation.Launcher = &silentLauncher{running: map[string]bool{}}
			original := f.service.config.transport
			paused, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var pauseOnce, resumeOnce sync.Once
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			f.service.config.transport = func(org Organization) (http.RoundTripper, error) {
				transport, err := original(org)
				if err != nil || org.ID != "org_beta" {
					return transport, err
				}
				return runtimeTransport(func(request *http.Request) (*http.Response, error) {
					if request.URL.Path == scenario.pause {
						pauseOnce.Do(func() { close(paused) })
						select {
						case <-resume:
						case <-request.Context().Done():
							return nil, request.Context().Err()
						}
					}
					if request.URL.Path == "/internal/v1/owner/bootstrap" {
						return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
					}
					if scenario.refuse && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/entitlements/runtime") {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: make(http.Header)}, nil
					}
					return transport.RoundTrip(request)
				}), nil
			}
			go func() {
				defer close(finished)
				f.service.provision(ctx, organization)
			}()
			t.Cleanup(func() {
				cancel()
				resumeOnce.Do(func() { close(resume) })
				<-finished
				f.service.config.transport = original
			})
			select {
			case <-paused:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			candidate := *retry.Runtime.Plans
			candidate.Plans = append([]hubserver.HostedPlan(nil), candidate.Plans...)
			plan := stagedPlan
			plan.Version += 20 + int64(index)
			candidate.Plans = append(candidate.Plans, plan)
			candidate.WindowSeconds = before.Plans.WindowSeconds + 120
			attempt := next
			price := hubserver.HostedBillingPrice{PriceID: "price_join_" + strconv.FormatInt(plan.Version, 10), Label: "Published plan", Plan: plan.PlanReference}
			attempt.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &candidate, Prices: append(append([]hubserver.HostedBillingPrice(nil), next.Runtime.Prices...), price)}
			readings <- reading{config: attempt}
			if err := f.service.reloadConfiguration(t.Context()); err != nil {
				t.Fatal(err)
			}
			awaitLog("configuration reloaded")
			if state := readRuntime("org_alpha"); state.Plans.WindowSeconds != candidate.WindowSeconds {
				t.Fatal("ready tenant did not receive the publication")
			}
			if state := readRuntime("org_beta"); state.Plans.WindowSeconds != before.Plans.WindowSeconds || !reflect.DeepEqual(state.Prices, before.Prices) {
				t.Fatal("starting tenant exposed settings before preparation")
			}
			publicationTemp := f.service.runtimePublicationPath() + ".tmp"
			if scenario.publishFailure {
				if err := os.Mkdir(publicationTemp, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			resumeOnce.Do(func() { close(resume) })
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			organization, err = f.service.registry.Organization(t.Context(), "org_beta")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.refuse || scenario.publishFailure {
				if organization.State == "ready" || readRuntime("org_beta").Plans.WindowSeconds != before.Plans.WindowSeconds {
					t.Fatal("failed runtime preparation made the tenant ready or exposed settings")
				}
				report, err := f.service.tenantEntitlements(t.Context(), organization)
				if err != nil {
					t.Fatal(err)
				}
				for _, prepared := range report.Plans {
					if prepared.ID == plan.ID && prepared.Version == plan.Version {
						t.Fatal("failed tenant publication exposed its prepared plan")
					}
				}
				if scenario.publishFailure {
					if err := os.Remove(publicationTemp); err != nil {
						t.Fatal(err)
					}
				}
				f.service.config.transport = original
				f.service.provision(t.Context(), organization)
			}
			for _, id := range []string{"org_alpha", "org_beta"} {
				organization, err := f.service.registry.Organization(t.Context(), id)
				if err != nil || organization.State != "ready" {
					t.Fatalf("tenant %s readiness=%s: %v", id, organization.State, err)
				}
				if state := readRuntime(id); state.Plans.WindowSeconds != candidate.WindowSeconds || !reflect.DeepEqual(state.Prices, attempt.Runtime.Prices) {
					t.Fatalf("tenant %s missed publication: window=%d want=%d", id, state.Plans.WindowSeconds, candidate.WindowSeconds)
				}
				report, err := f.service.tenantEntitlements(t.Context(), organization)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, published := range report.Plans {
					found = found || published.ID == plan.ID && published.Version == plan.Version
				}
				if !found {
					t.Fatalf("tenant %s missed the new plan", id)
				}
			}
		})
	}

}

func TestPlatformEntitlementsRESTAndMCPWithoutRestart(t *testing.T) {
	for _, transport := range []string{"REST", "MCP"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			const token = "runtime-entitlement-admin-0123456789abcdef"
			entitlementFixture(t, f, token)
			f.service.config.Allocation = &AllocationConfig{EntitlementAdminToken: []byte(token)}
			organization, err := f.service.registry.Organization(t.Context(), "org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.service.tenantEntitlements(t.Context(), organization)
			if err != nil {
				t.Fatal(err)
			}
			plan := entitlementPlan{PlatformPlanReference: planReference{ID: "extended_capacity", Version: 1}, Features: []string{"collaboration"}, Allowances: map[string]int64{"projects": 50}}
			command := entitlementChange{Action: "create_plan", IdempotencyKey: "create-runtime", ExpectedRevision: before.Revision, Plan: plan.PlatformPlanReference, Definition: &plan, Reason: "capacity adjustment"}
			sessionID := ""
			send := func(path string, raw []byte) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, testPublicURL+path, strings.NewReader(string(raw)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+token)
				if sessionID != "" {
					request.Header.Set("Mcp-Session-Id", sessionID)
					request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				return response
			}
			if transport == "MCP" {
				initialized := send("/api/cloud/platform/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"plan-test","version":"1"}}}`))
				sessionID = initialized.Header().Get("Mcp-Session-Id")
				if initialized.Code != http.StatusOK || sessionID == "" {
					t.Fatalf("MCP initialize=%d %s", initialized.Code, initialized.Body.String())
				}
				if response := send("/api/cloud/platform/mcp", []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); response.Code != http.StatusAccepted {
					t.Fatalf("MCP initialized=%d", response.Code)
				}
			}
			apply := func(change entitlementChange) {
				t.Helper()
				path := "/api/cloud/platform/organizations/org_alpha/entitlements"
				var payload any = change
				if transport == "MCP" {
					path = "/api/cloud/platform/mcp"
					payload = map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "platform_change_entitlement", "arguments": struct {
						OrganizationID string `json:"organization_id"`
						entitlementChange
					}{"org_alpha", change}}}
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				response := send(path, raw)
				if response.Code != http.StatusOK {
					t.Fatalf("plan change=%d %s", response.Code, response.Body.String())
				}
				if transport == "MCP" {
					var reply struct {
						Error  json.RawMessage `json:"error"`
						Result struct {
							IsError bool `json:"isError"`
						} `json:"result"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || len(reply.Error) > 0 || reply.Result.IsError {
						t.Fatalf("MCP change=%s %v", response.Body.String(), err)
					}
				}
			}
			apply(command)
			apply(command)
			command = entitlementChange{Action: "grant", IdempotencyKey: "grant-runtime", ExpectedRevision: before.Revision + 1, Plan: plan.PlatformPlanReference, Reason: "capacity adjustment"}
			apply(command)
			after, err := f.service.tenantEntitlements(t.Context(), organization)
			if err != nil || after.Revision != before.Revision+2 || len(after.Grants) != 1 || after.Grants[0].Plan != plan.PlatformPlanReference {
				t.Fatalf("runtime entitlement=%+v %v", after, err)
			}
			until := time.Now().Add(time.Hour)
			for index, assignment := range []struct {
				action string
				plan   planReference
				want   planReference
				source string
			}{
				{"base", plan.PlatformPlanReference, plan.PlatformPlanReference, "base"},
				{"subscription", before.Base, before.Base, "subscription"},
				{"end_subscription", before.Base, plan.PlatformPlanReference, "base"},
			} {
				apply(entitlementChange{Action: assignment.action, IdempotencyKey: assignment.action + "-runtime", ExpectedRevision: before.Revision + int64(index) + 2, Plan: assignment.plan, ExpiresAt: &until, Reason: "capacity adjustment"})
				after, err = f.service.tenantEntitlements(t.Context(), organization)
				if err != nil || after.Revision != before.Revision+int64(index)+3 || after.Base != plan.PlatformPlanReference || after.EffectiveBase != assignment.want || after.Source != assignment.source || len(after.Grants) != 1 || after.Grants[0].Plan != plan.PlatformPlanReference {
					t.Fatalf("%s entitlement=%+v %v", assignment.action, after, err)
				}
			}
			var audits int
			if err := f.service.registry.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM entitlement_changes WHERE reason='capacity adjustment' AND staff_email='entitlement-administrator'").Scan(&audits); err != nil || audits != 5 {
				t.Fatalf("entry audit=%d %v", audits, err)
			}
		})
	}
}
