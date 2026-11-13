package requestmetrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type sharedMetricRepository struct {
	metricsstore.Repository
	merge func(context.Context, tracker.OrganizationID, []metricsstore.Metric) error
}

func (r sharedMetricRepository) MergeRequestMetrics(ctx context.Context, org tracker.OrganizationID, _ time.Time, rows []metricsstore.Metric) error {
	return r.merge(ctx, org, rows)
}

func TestSharedMetricsCapIncludesInflightAndFailurePreservesCounts(t *testing.T) {
	first := hubmaintenance.Tenant{Organization: "org_first", Generation: 1}
	second := hubmaintenance.Tenant{Organization: "org_second", Generation: 2}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	entered, release := make(chan struct{}), make(chan struct{})
	failure := errors.New("metrics unavailable")
	collector, err := NewRequestMetrics(sharedMetricRepository{merge: func(context.Context, tracker.OrganizationID, []metricsstore.Metric) error {
		close(entered)
		<-release
		return failure
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !collector.Record(first, now, "GET /example", "runner", "example", 200, time.Millisecond) {
		t.Fatal("first sample refused")
	}
	done := make(chan error, 1)
	go func() { done <- collector.Flush(t.Context(), first, now) }()
	<-entered
	collector.Record(first, now, "GET /example", "runner", "example", 200, 2*time.Millisecond)
	collector.Record(second, now, "GET /example", "browser", "example", 200, time.Millisecond)
	for i := range sharedMetricKeyLimit * 2 {
		collector.Record(second, now, "GET /example", "browser", fmt.Sprintf("example_%d", i), 200, time.Millisecond)
	}
	if len(collector.pending)+collector.inflight > sharedMetricKeyLimit {
		t.Errorf("unbounded telemetry: pending=%d inflight=%d", len(collector.pending), collector.inflight)
	}
	close(release)
	if err := <-done; !errors.Is(err, failure) {
		t.Fatalf("flush failure lost: %v", err)
	}
	collector.Repository = sharedMetricRepository{merge: func(_ context.Context, org tracker.OrganizationID, rows []metricsstore.Metric) error {
		if org != "org_first" || len(rows) != 1 || rows[0].Count != 2 || rows[0].Sum != 3 {
			t.Errorf("retry lost or mixed samples: org=%s rows=%+v", org, rows)
		}
		return nil
	}}
	if err := collector.Flush(t.Context(), first, now); err != nil {
		t.Fatal(err)
	}
}

func TestSharedMetricsPostgresKeepsTenantGenerationSeparate(t *testing.T) {
	repository := hubtest.Repository(t)
	collector, err := NewRequestMetrics(repository)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for index, tenant := range []hubmaintenance.Tenant{{Organization: "org_first", Generation: 1}, {Organization: "org_second", Generation: 1}, {Organization: "org_first", Generation: 2}} {
		for range index + 1 {
			collector.Record(tenant, now.Add(-time.Minute), "GET /example", "runner", "example", 200, time.Millisecond)
		}
	}
	for _, tenant := range []hubmaintenance.Tenant{{Organization: "org_first", Generation: 1}, {Organization: "org_second", Generation: 1}} {
		if err := collector.Flush(t.Context(), tenant, now); err != nil {
			t.Fatal(err)
		}
		_, rows, err := repository.RequestMetricTotals(t.Context(), tracker.OrganizationID(tenant.Organization), now.Add(-time.Hour), now)
		want := int64(1)
		if tenant.Organization == "org_second" {
			want = 2
		}
		if err != nil || len(rows) != 1 || rows[0].Count != want {
			t.Fatalf("tenant/generation mixed: tenant=%+v rows=%+v err=%v", tenant, rows, err)
		}
	}
}

func testRequestMetrics(t *testing.T, organization tracker.OrganizationID) *Service {
	t.Helper()
	m, err := newPostgresTestMetrics(t, organization)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
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
		if got := Route(routes, test.method, test.path); got != test.want {
			t.Errorf("Route(%q, %q) = %q, want %q", test.method, test.path, got, test.want)
		}
	}
}

func TestRequestMetricsRejectInvalidClock(t *testing.T) {
	for _, now := range []time.Time{{}, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)} {
		t.Run(now.String(), func(t *testing.T) {
			shared, err := NewRequestMetrics(sharedMetricRepository{})
			if err != nil {
				t.Fatal(err)
			}
			accepted := shared.Record(hubmaintenance.Tenant{Organization: "org_example", Generation: 1}, now, "GET /example", "browser", "example", 500, time.Millisecond)
			local := &Service{pending: make(map[Key]Latency)}
			local.Record(now, "GET /example", "browser", "example", 500, time.Millisecond)
			want := 1
			if now.IsZero() {
				want = 0
			}
			if accepted != (want == 1) || len(shared.pending) != want || len(local.pending) != want {
				t.Fatalf("accepted=%v shared=%d local=%d want=%d", accepted, len(shared.pending), len(local.pending), want)
			}
		})
	}
}

func TestRequestHistoryKeepsBusiestRoutes(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	m := testRequestMetrics(t, "org_busy")
	for route := range routeLatencyRouteLimit + 5 {
		for range route + 1 {
			m.Record(now.Add(-time.Minute), fmt.Sprintf("GET /route/%02d", route), "browser", "user", 200, time.Millisecond)
			m.Record(now.Add(-48*time.Hour), fmt.Sprintf("GET /route/%02d", route), "browser", "user", 500, time.Millisecond)
		}
	}
	if err := m.Flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	history, err := m.History(t.Context(), now.Add(-2160*time.Hour), now, time.Hour)
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
			repo := hubtest.Repository(t)
			org := tracker.OrganizationID("org_example")
			plans := BillingPlan{WindowSeconds: 3600, RetentionWindows: 24}
			owner := &BillingOwner{Repository: repo, Organization: org, Plans: func() *BillingPlan { return &plans }}
			if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			pending := make(map[Key]Latency)
			for i, length := range test.lengths {
				request := httptest.NewRequest(http.MethodGet, "/health", nil)
				request.ContentLength = length
				c := e.NewContext(request, httptest.NewRecorder())
				key := Key{Minute: now.Unix() / 60 * 60, Route: fmt.Sprintf("/health/%d", i), Kind: "anonymous", Status: 2}
				pending[key] = Latency{HTTP: UsageFor(c, time.Duration(0))}
			}
			if err := (&Service{}).WriteUsage(t.Context(), owner, now, pending); err != nil {
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
				if retry := m.Admit(now, "runner_one", "GET /items", test.capacity, []string{"project"}); retry != 0 {
					t.Fatalf("within budget throttled: %d", retry)
				}
			}
			if retry := m.Admit(now, "runner_one", "GET /items", test.capacity, []string{"project"}); retry != 45 {
				t.Fatalf("retry=%d", retry)
			}
			if retry := m.Admit(now, "runner_two", "GET /items", test.capacity, nil); retry != 0 {
				t.Fatal("other runner throttled")
			}
			for range 10 * test.budget {
				if retry := m.Admit(now, "runner_one", "GET /go-test-receipts", test.capacity, nil); retry != 0 {
					t.Fatalf("receipt lookup within its budget throttled: %d", retry)
				}
			}
			if retry := m.Admit(now, "runner_one", "POST /go-test-receipts", test.capacity, nil); retry != 45 {
				t.Fatalf("receipt retry=%d", retry)
			}
			findings := m.Findings(now.Add(time.Minute))
			if len(findings) != 1 || findings[0].Subject.ID != "runner_one" || !strings.Contains(findings[0].Summary, "GET /items") {
				t.Fatalf("findings=%+v", findings)
			}
			if retry := m.Admit(now.Add(time.Minute), "runner_one", "GET /items", test.capacity, nil); retry != 0 {
				t.Fatal("new minute throttled")
			}
			if len(m.Findings(now.Add(3*time.Minute))) != 0 {
				t.Fatal("finding did not clear")
			}
		})
	}
}
