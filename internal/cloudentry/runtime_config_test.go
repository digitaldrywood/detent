//go:build unix

package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func TestCloudSIGHUPRetainsInvalidAndAppliesValidRuntime(t *testing.T) {
	f := newEntryFixture(t)
	const token = "runtime-entitlement-admin-0123456789abcdef"
	entitlementFixture(t, f, token)
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
	next.Runtime = &hubserver.HostedRuntimeUpdate{Plans: &plans}
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
			var audits int
			if err := f.service.registry.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM entitlement_changes WHERE reason='capacity adjustment' AND staff_email='entitlement-administrator'").Scan(&audits); err != nil || audits != 2 {
				t.Fatalf("entry audit=%d %v", audits, err)
			}
		})
	}
}
