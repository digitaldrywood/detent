package hubserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func testRequestMetrics(t *testing.T, organization tracker.OrganizationID) *tenantRequestMetrics {
	t.Helper()
	m, err := newPostgresTestMetrics(t, organization)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func TestMetricRouteBoundsKeys(t *testing.T) {
	t.Parallel()
	routes := map[string]bool{"GET /api/v2/organizations/:organization/activity": true}
	for _, test := range []struct {
		method, path, want string
	}{
		{"GET", "/api/v2/organizations/:organization/activity", "GET /api/v2/organizations/:organization/activity"},
		{"PATCH", "/api/v2/organizations/:organization/activity", "PATCH <unmatched>"},
		{"GET", "/api/v2/organizations/:organization/", "GET <unmatched>"},
		{"GET", "", "GET <unmatched>"},
		{"BREW", "/api/v2/organizations/:organization/activity", "OTHER <unmatched>"},
		{"get", "/api/v2/organizations/:organization/activity", "OTHER <unmatched>"},
	} {
		if got := metricRoute(routes, test.method, test.path); got != test.want {
			t.Errorf("metricRoute(%q, %q) = %q, want %q", test.method, test.path, got, test.want)
		}
	}
}

func TestRequestHistoryKeepsBusiestRoutes(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	m := testRequestMetrics(t, "org_busy")
	for route := range routeLatencyRouteLimit + 5 {
		for range route + 1 {
			m.record(now.Add(-time.Minute), fmt.Sprintf("GET /route/%02d", route), "browser", "user", 200, time.Millisecond)
			m.record(now.Add(-48*time.Hour), fmt.Sprintf("GET /route/%02d", route), "browser", "user", 500, time.Millisecond)
		}
	}
	if err := m.flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	history, err := m.history(t.Context(), now.Add(-2160*time.Hour), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int64)
	for _, bucket := range history.Buckets {
		counts[bucket.Route] += bucket.Count
	}
	if len(counts) != routeLatencyRouteLimit {
		t.Fatalf("routes=%d want %d: %v", len(counts), routeLatencyRouteLimit, counts)
	}
	for route := 5; route < routeLatencyRouteLimit+5; route++ {
		if got := counts[fmt.Sprintf("GET /route/%02d", route)]; got != int64(2*(route+1)) {
			t.Fatalf("route %d count=%d want %d", route, got, 2*(route+1))
		}
	}
}

func TestHostedRequestUsageSaturatesOversizedLengths(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		lengths []int64
		want    int64
	}{
		{"single oversized length", []int64{1 << 62}, 1 << 50},
		{"overflowing sum", []int64{1 << 62, 1 << 62}, 1 << 50},
		{"ordinary lengths", []int64{3, 4}, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := projectRecordPostgres(t)
			org := tracker.OrganizationID("org_example")
			owner := &database{billingRepository: repo, hostedOrganization: org}
			plans := pilotHostedPlans()
			owner.hostedRuntime.Store(&hostedRuntimeConfig{Plans: plans})
			if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			pending := make(map[requestMetricKey]requestLatency)
			for i, length := range test.lengths {
				request := httptest.NewRequest(http.MethodGet, "/health", nil)
				request.ContentLength = length
				c := e.NewContext(request, httptest.NewRecorder())
				key := requestMetricKey{Minute: now.Unix() / 60 * 60, Route: fmt.Sprintf("/health/%d", i), Kind: "anonymous", Status: 2}
				pending[key] = requestLatency{http: hostedRequestUsageFor(c, time.Duration(0))}
			}
			if err := (&tenantRequestMetrics{}).writeHostedRequestUsage(t.Context(), owner, now, pending); err != nil {
				t.Fatalf("oversized request lengths blocked usage flush: %v", err)
			}
			var amount int64
			err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				amount, err = r.BillingUsage(t.Context(), time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0), "http_request_bytes")
				return err
			})
			if err != nil || amount != test.want {
				t.Fatalf("amount=%d want=%d err=%v", amount, test.want, err)
			}
			if len(pending) != 0 {
				t.Fatalf("flushed usage stayed pending: %d", len(pending))
			}
		})
	}
}

func TestRunnerRequestBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		capacity, budget int
	}{{"zero capacity", 0, 120}, {"one slot", 1, 180}, {"four slots", 4, 360}} {
		t.Run(test.name, func(t *testing.T) {
			m := testRequestMetrics(t, "org_one")
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
			for range 10 * test.budget {
				if retry := m.admit(now, "runner_one", "GET /go-test-receipts", test.capacity, nil); retry != 0 {
					t.Fatalf("receipt lookup within its budget throttled: %d", retry)
				}
			}
			if retry := m.admit(now, "runner_one", "POST /go-test-receipts", test.capacity, nil); retry != 45 {
				t.Fatalf("receipt retry=%d", retry)
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
	f := newRunnerPGFixture(t)
	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	f.service.config.now = func() time.Time { return now }
	m, err := f.service.database.openRequestMetrics(t.Context(), "org_startup")
	if err != nil {
		t.Fatal(err)
	}
	f.service.requestMetrics = m
	for _, binding := range f.bindings {
		credential := apiCredential{Runner: runnerauth.Identity{Binding: binding}}
		request := func(method, path string) *httptest.ResponseRecorder {
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(method, path, nil), response)
			c.SetPath(path)
			if err := f.service.admitRunnerRequest(c, credential); err != nil {
				t.Fatal(err)
			}
			return response
		}
		for range 180 {
			if response := request(http.MethodGet, "/items"); response.Code != http.StatusOK {
				t.Fatalf("within budget=%d", response.Code)
			}
		}
		response := request(http.MethodGet, "/items")
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "40" {
			t.Fatalf("budget=%d retry=%q", response.Code, response.Header().Get("Retry-After"))
		}
		if response := request(http.MethodGet, "/go-test-receipts"); response.Code != http.StatusOK {
			t.Fatal("receipt lookup exhausted ordinary budget")
		}
		if response := request(http.MethodPost, "/api/v2/runner/heartbeat"); response.Code != http.StatusOK {
			t.Fatal("heartbeat throttled")
		}
	}
}

func TestRequestMetricsAdminReads(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := accountPreferencesPostgresFixture(t, repository)
	f.project = "prj_example"
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example", CreatedAt: time.Now()}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"}); err != nil {
			return err
		}
		_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_security", ID: f.project, Name: "Example", Profile: "native"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	metrics, err := f.service.database.openRequestMetrics(t.Context(), "org_security")
	if err != nil {
		t.Fatal(err)
	}
	f.service.requestMetrics = metrics
	f.service.echo.Use(f.service.measureRequests)
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	member := f.user(t, "member", "member", "member@example.test", "", "")
	path := "/api/v2/organizations/org_security/diagnostics/requests"
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, path, nil), http.StatusOK)
	requireNativeStatus(t, f.request(t, member, http.MethodGet, path, nil), http.StatusForbidden)
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/api/v2/organizations/org_other/diagnostics/requests", nil), http.StatusNotFound)
	serverTiming := regexp.MustCompile(`^db;dur=[0-9.]+, compute;dur=[0-9.]+, encode;dur=[0-9.]+, total;dur=[0-9.]+(?:, pg_[a-z_]+;dur=[0-9.]+)*$`)
	for _, test := range []struct {
		name     string
		response *httptest.ResponseRecorder
		timed    bool
	}{
		{"authenticated admin", f.request(t, owner, http.MethodGet, path, nil), true},
		{"authenticated without access", f.request(t, member, http.MethodGet, path, nil), true},
		{"anonymous", performHubAPIRequest(t, f.service, http.MethodGet, path, "", nil), false},
		{"invalid token", performHubAPIRequest(t, f.service, http.MethodGet, path, "detent_invalid", nil), false},
	} {
		t.Run("server timing/"+test.name, func(t *testing.T) {
			header := test.response.Header().Get("Server-Timing")
			if test.timed != serverTiming.MatchString(header) || !test.timed && header != "" {
				t.Fatalf("status=%d Server-Timing=%q", test.response.Code, header)
			}
		})
	}
	historyResponse := f.request(t, owner, http.MethodGet, path+"?window=24h", nil)
	requireNativeStatus(t, historyResponse, http.StatusOK)
	var history requestMetricsResponse
	decodeHubResponse(t, historyResponse, &history)
	if history.RouteLatency == nil || history.RouteLatency.Bucket != "1h0m0s" {
		t.Fatalf("route latency history: %s", historyResponse.Body.String())
	}
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, path+"?window=1m", nil), http.StatusBadRequest)
	requireNativeStatus(t, f.request(t, member, http.MethodGet, path+"?window=1m", nil), http.StatusForbidden)
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: owner.identity.Subject, ProjectID: f.project}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		access hostedProjectAccess
		scope  string
		want   int
	}{{hostedProjectsAll, "admin", http.StatusOK}, {hostedProjectsSelected, "admin", http.StatusForbidden}, {hostedProjectsAll, "read", http.StatusForbidden}} {
		token, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		id := "metrics-key-" + string(test.access) + "-" + test.scope
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), owner.identity.Subject)
			if err != nil {
				return err
			}
			scope := "operator"
			if test.scope == "admin" {
				scope = "admin"
			}
			expires := time.Now().Add(time.Hour)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: id, Name: id, Hash: apikey.HashToken(token), Scope: scope, NativeOnly: true, CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: &expires, UserID: member.UserID, MembershipID: member.MembershipID, KeyScope: test.scope, ProjectAccess: string(test.access), Kind: "personal"}); err != nil {
				return err
			}
			if test.access == hostedProjectsSelected {
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: id, ProjectID: f.project})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, token, nil), test.want)
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
