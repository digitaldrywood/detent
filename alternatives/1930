package hubserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func testRequestMetrics(t *testing.T, path string, organization tracker.OrganizationID) *tenantRequestMetrics {
	t.Helper()
	m, err := openRequestMetrics(t.Context(), path, organization)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.db.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func TestRequestMetricsPersistence(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Minute)
	dir := t.TempDir()
	m := testRequestMetrics(t, filepath.Join(dir, "one.metrics.sqlite"), "org_one")
	other := testRequestMetrics(t, filepath.Join(dir, "two.metrics.sqlite"), "org_two")
	m.record(now.Add(-15*24*time.Hour), "GET /expired", "browser", "old", 200, time.Second)
	if err := m.flush(t.Context(), now.Add(-15*24*time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		m.record(now.Add(-time.Minute), "GET /items/:item", "runner", "runner_one", 200, time.Duration(i+1)*time.Millisecond)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.flush(canceled, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled flush=%v", err)
	}
	if err := m.flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	m.record(now.Add(-time.Minute), "GET /items/:item", "runner", "runner_one", 429, 20*time.Millisecond)
	if err := m.flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Close(); err != nil {
		t.Fatal(err)
	}
	m = testRequestMetrics(t, filepath.Join(dir, "one.metrics.sqlite"), "org_one")
	report, err := m.report(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Callers) != 1 || len(report.Routes) != 1 {
		t.Fatalf("report=%+v", report)
	}
	caller := report.Callers[0]
	if caller.Caller != "runner_one" || caller.Count != 21 || caller.Sum != 230 || caller.P50 != 16.384 || caller.P95 != 20 || caller.Max != 20 {
		t.Fatalf("caller=%+v", caller)
	}
	var count, classes int
	if err := m.db.QueryRowContext(t.Context(), "SELECT count(*),count(DISTINCT status_class) FROM request_minutes").Scan(&count, &classes); err != nil {
		t.Fatal(err)
	}
	if count != 2 || classes != 2 {
		t.Fatalf("minute/status rows=%d classes=%d", count, classes)
	}
	if got, err := other.report(t.Context(), now); err != nil || len(got.Callers) != 0 {
		t.Fatalf("other tenant=%+v err=%v", got, err)
	}
	if foreign, err := openRequestMetrics(t.Context(), filepath.Join(dir, "one.metrics.sqlite"), "org_two"); !errors.Is(err, ErrHostedDatabaseBinding) {
		if foreign != nil {
			foreign.db.Close()
		}
		t.Fatalf("foreign binding accepted: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "one.metrics.sqlite"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("metrics file permissions: %v %v", info, err)
	}
}

func TestRequestMetricsFlushDoesNotUseTenantWriter(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	f.service.stopHealthDetector()
	m := f.service.requestMetrics
	if m == nil {
		t.Fatal("tenant Hub did not open metrics")
	}
	now := time.Now().UTC().Truncate(time.Minute)
	m.record(now.Add(-time.Minute), "GET /items/:item", "runner", "runner", 200, time.Millisecond)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := m.flush(ctx, now); err != nil {
		t.Fatalf("metrics flush waited for tenant writer: %v", err)
	}
	var count int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE name='request_minutes'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("metrics touched tenant schema: %d %v", count, err)
	}
	if _, err := m.report(ctx, now); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRequestBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		capacity, budget int
	}{{"zero capacity", 0, 120}, {"one slot", 1, 180}, {"four slots", 4, 360}} {
		t.Run(test.name, func(t *testing.T) {
			m := testRequestMetrics(t, filepath.Join(t.TempDir(), "metrics.sqlite"), "org_one")
			now := time.Now().UTC().Truncate(time.Minute).Add(15 * time.Second)
			for range test.budget {
				if retry := m.admit(now, "runner_one", "GET /items", test.capacity, []string{"project"}); retry != 0 {
					t.Fatalf("within budget throttled: %d", retry)
				}
			}
			if retry := m.admit(now, "runner_one", "GET /items", test.capacity, []string{"project"}); retry != 45 {
				t.Fatalf("retry=%d", retry)
			}
			if retry := m.admit(now, "runner_two", "GET /items", test.capacity, nil); retry != 0 {
				t.Fatal("other runner throttled")
			}
			findings := m.findings(now.Add(time.Minute))
			if len(findings) != 1 || findings[0].Subject.ID != "runner_one" || !strings.Contains(findings[0].Summary, "GET /items") {
				t.Fatalf("findings=%+v", findings)
			}
			if retry := m.admit(now.Add(time.Minute), "runner_one", "GET /items", test.capacity, nil); retry != 0 {
				t.Fatal("new minute throttled")
			}
			if len(m.findings(now.Add(3*time.Minute))) != 0 {
				t.Fatal("finding did not clear")
			}
		})
	}
}

func TestRunnerRequestBudgetHTTP(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	f.service.stopHealthDetector()
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	issue := f.create(t, "queued budget work")
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	f.service.config.Hosted = &HostedConfig{}
	f.service.database.hostedOrganization = f.project.OrganizationID
	hostedTestPlans(t, f.service, map[string]int64{"api_mutations": 0})
	f.service.config.Hosted = nil
	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	f.service.config.now = func() time.Time { return now }
	m, err := openRequestMetrics(t.Context(), f.service.database.path+".metrics.sqlite", f.project.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.requestMetrics = m
	for range 240 {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items", r.redemption.Credential, nil), http.StatusOK)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items", r.redemption.Credential, nil)
	requireNativeCode(t, response, http.StatusTooManyRequests, "runner_request_budget")
	if response.Header().Get("Retry-After") != "40" {
		t.Fatalf("retry=%q", response.Header().Get("Retry-After"))
	}
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "request-budget", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimed, &lease)
	for _, test := range []struct {
		name, path string
		body       any
		want       int
	}{
		{"heartbeat", f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat", map[string]any{"capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation}, http.StatusOK},
		{"lease renewal", f.base + "/leases/" + string(lease.ID) + "/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}, http.StatusOK},
		{"lease release", f.base + "/leases/" + string(lease.ID) + "/release", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}, http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, test.path, r.redemption.Credential, test.body), test.want)
		})
	}
	var used int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT coalesce(sum(amount),0) FROM hosted_usage_windows WHERE metric='api_mutations'").Scan(&used); err != nil || used != 0 {
		t.Fatalf("runner retained the replaced mutation allowance: used=%d err=%v", used, err)
	}
	if err := f.service.evaluateOrganizationHealth(t.Context(), f.project.OrganizationID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var summary string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT summary FROM health_findings WHERE signal='runner_request_budget' AND resolved_at IS NULL").Scan(&summary); err != nil || !strings.Contains(summary, "GET "+nativeBase+"/work-items") {
		t.Fatalf("budget finding: %q %v", summary, err)
	}
	if err := m.flush(t.Context(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	report, err := m.report(t.Context(), now.Add(time.Minute))
	if err != nil || len(report.Callers) != 1 || report.Callers[0].Kind != "runner" || report.Callers[0].Count != 245 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestRequestMetricsAdminReads(t *testing.T) {
	f := newHostedSecurityFixture(t)
	f.service.stopHealthDetector()
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	member := f.user(t, "member", "member", "member@example.test", "", "")
	path := "/api/v2/organizations/org_security/diagnostics/requests"
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, path, nil), http.StatusOK)
	requireNativeStatus(t, f.request(t, member, http.MethodGet, path, nil), http.StatusForbidden)
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/api/v2/organizations/org_other/diagnostics/requests", nil), http.StatusNotFound)
	f.grant(t, owner, false, false)
	for _, test := range []struct {
		access hostedProjectAccess
		scope  string
		want   int
	}{{hostedProjectsAll, "admin", http.StatusOK}, {hostedProjectsSelected, "admin", http.StatusForbidden}, {hostedProjectsAll, "read", http.StatusForbidden}} {
		request := map[string]any{"name": "metrics-key-" + string(test.access) + "-" + test.scope, "scope": test.scope, "expires_days": 30, "project_access": test.access}
		if test.access == hostedProjectsSelected {
			request["project_ids"] = []string{string(f.project)}
		}
		created := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/api-keys", request)
		requireNativeStatus(t, created, http.StatusCreated)
		var key tokenResponse
		decodeHubResponse(t, created, &key)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, key.Token, nil), test.want)
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	if err := f.service.requestMetrics.flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	report, err := f.service.requestMetrics.report(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, caller := range report.Callers {
		found = found || caller.Kind == "browser" && caller.Caller == "user_owner"
	}
	if !found {
		t.Fatalf("browser attribution=%+v", report)
	}
}
