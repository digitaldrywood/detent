package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type requestMetricKey struct {
	Minute int64
	Route  string
	Kind   string
	Caller string
	Status int
}

type requestLatency struct {
	http      hostedRequestUsage
	Count     int64     `json:"count"`
	Sum       float64   `json:"sum_ms"`
	P50       float64   `json:"p50_ms"`
	P95       float64   `json:"p95_ms"`
	Max       float64   `json:"max_ms"`
	Histogram [64]int64 `json:"-"`
}

func (l *requestLatency) observe(elapsed time.Duration) {
	us := elapsed.Microseconds()
	if elapsed%time.Microsecond > 0 {
		us++
	}
	us = max(int64(1), us)
	i := min(bits.Len64(uint64(us-1)), len(l.Histogram)-1)
	l.Histogram[i]++
	l.Count++
	ms := float64(elapsed) / float64(time.Millisecond)
	l.Sum += ms
	l.Max = max(l.Max, ms)
}

func (l *requestLatency) merge(other requestLatency) {
	l.http.merge(other.http)
	l.Count += other.Count
	l.Sum += other.Sum
	l.Max = max(l.Max, other.Max)
	for i, count := range other.Histogram {
		l.Histogram[i] += count
	}
}

func (l *requestLatency) percentiles() {
	var count int64
	for i, n := range l.Histogram {
		count += n
		if count-n < (l.Count+1)/2 && count >= (l.Count+1)/2 {
			l.P50 = min(l.Max, float64(uint64(1)<<i)/1000)
		}
		if count-n < (l.Count*95+99)/100 && count >= (l.Count*95+99)/100 {
			l.P95 = min(l.Max, float64(uint64(1)<<i)/1000)
		}
	}
}

type requestMetricTop struct {
	Kind   string `json:"caller_kind,omitempty"`
	Caller string `json:"caller_id,omitempty"`
	Route  string `json:"route,omitempty"`
	requestLatency
}

type requestMetricsReport struct {
	From    time.Time          `json:"from"`
	To      time.Time          `json:"to"`
	Callers []requestMetricTop `json:"top_callers"`
	Routes  []requestMetricTop `json:"top_routes"`
}

type runnerRequestWindow struct {
	Minute       int64
	Count        int
	ReceiptCount int
	Budget       int
	Routes       map[string]int
	Projects     []string
	ExcessAt     time.Time
	ExcessCount  int
	ExcessBudget int
	ExcessRoutes []string
}

type tenantRequestMetrics struct {
	usage          *database
	billingPending *hostedRequestBillingBatch
	db             *sql.DB
	repository     metricsstore.Repository
	organization   tracker.OrganizationID
	mu             sync.Mutex
	flushMu        sync.Mutex
	pending        map[requestMetricKey]requestLatency
	runners        map[string]*runnerRequestWindow
	routesOnce     sync.Once
	routes         map[string]bool
	ingested       atomic.Int64
}

func (m *tenantRequestMetrics) record(now time.Time, route, kind, caller string, status int, elapsed time.Duration) {
	key := requestMetricKey{now.Unix() / 60 * 60, route, kind, caller, status / 100}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.pending[key]
	l.observe(elapsed)
	m.pending[key] = l
}

func (m *tenantRequestMetrics) flush(ctx context.Context, now time.Time) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if err := m.flushHostedBillingBatch(ctx, now); err != nil {
		return err
	}
	m.mu.Lock()
	pending, owner := m.pending, m.usage
	m.pending = make(map[requestMetricKey]requestLatency)
	m.mu.Unlock()
	latencies := make(map[requestMetricKey]requestLatency)
	for key, value := range pending {
		if value.Count > 0 {
			latencies[key] = value
		}
	}
	err := m.write(ctx, now, latencies)
	if err == nil && m.repository == nil {
		clear(latencies)
	}
	usagePending := make(map[requestMetricKey]requestLatency, len(pending))
	for key, value := range pending {
		usagePending[key] = requestLatency{http: value.http}
	}
	if err == nil {
		if owner != nil {
			err = m.writeHostedRequestUsage(ctx, owner, now, usagePending)
		} else {
			clear(usagePending)
		}
	}
	for key := range pending {
		remaining := latencies[key]
		remaining.http = usagePending[key].http
		pending[key] = remaining
	}
	m.mu.Lock()
	for key, value := range pending {
		if value.Count == 0 && value.http.requests == 0 {
			continue
		}
		current := m.pending[key]
		current.merge(value)
		m.pending[key] = current
	}
	m.mu.Unlock()
	return err
}

func (m *tenantRequestMetrics) write(ctx context.Context, now time.Time, pending map[requestMetricKey]requestLatency) error {

	return m.writeRepository(ctx, now, pending)
}

func (m *tenantRequestMetrics) report(ctx context.Context, now time.Time) (requestMetricsReport, error) {

	return m.repositoryReport(ctx, now)
}

func runnerRequestExempt(method, route string) bool {
	return method == "POST" && (route == nativeBase+"/claims" || strings.Contains(route, "/leases/") || strings.HasSuffix(route, "/heartbeat") || route == runnerBase+"/:runner/renew")
}

type runnerRequestBudgetContext struct{}

func (s *Service) admitRunnerRequest(c echo.Context, credential apiCredential) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	m := s.requestMetrics
	if m == nil || credential.Runner.RunnerID == "" {
		return nil
	}
	c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), runnerRequestBudgetContext{}, true)))
	if runnerRequestExempt(c.Request().Method, c.Path()) {
		return nil
	}
	var capacity int

	if s.config.TenantRepository == nil || m.organization == "" {
		return s.nativeAPIError(c, ErrHostedDatabaseBinding)
	}
	var runnerCapacity int64
	err := s.config.TenantRepository.ReadTenant(ctx, m.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		identity, err := reader.RunnerIdentity(ctx, credential.Runner.RunnerID)
		if err != nil {
			return err
		}
		machine, err := reader.Machine(ctx, string(identity.MachineID))
		if err != nil {
			return err
		}
		runnerCapacity = min(identity.CapacityLimit, machine.Capacity)
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	capacity = int(runnerCapacity)

	projects := make([]string, 0, len(credential.Runner.ProjectIDs))
	for _, id := range credential.Runner.ProjectIDs {
		projects = append(projects, string(id))
	}
	now := s.config.now().UTC()
	if retry := m.admit(now, credential.Runner.RunnerID, c.Request().Method+" "+c.Path(), capacity, projects); retry > 0 {
		c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
		return s.refusedAPIError(c, http.StatusTooManyRequests, "runner_request_budget", "Runner request budget exceeded; retry after the current minute", apiErrorResponse{Code: "runner_request_budget", Message: "Runner request budget exceeded; retry after the current minute"})
	}
	return nil
}

func (m *tenantRequestMetrics) admit(now time.Time, runner, route string, capacity int, projects []string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	minute := now.Unix() / 60 * 60
	w := m.runners[runner]
	if w == nil {
		w = &runnerRequestWindow{}
		m.runners[runner] = w
	}
	if w.Minute != minute {
		w.Minute = minute
		w.Count = 0
		w.ReceiptCount = 0
		w.Routes = make(map[string]int)
	}
	w.Budget = 120 + 60*min(10000, max(0, capacity))
	w.Projects = slices.Clone(projects)
	w.Routes[route]++
	count, budget := &w.Count, w.Budget
	if strings.Contains(route, "/go-test-receipts") {
		count, budget = &w.ReceiptCount, 10*w.Budget
	}
	*count++
	if *count <= budget {
		return 0
	}
	w.ExcessAt = now
	w.ExcessCount = *count
	w.ExcessBudget = budget
	top := make([]string, 0, len(w.Routes))
	for route := range w.Routes {
		top = append(top, route)
	}
	slices.SortFunc(top, func(a, b string) int {
		if w.Routes[a] != w.Routes[b] {
			return w.Routes[b] - w.Routes[a]
		}
		return strings.Compare(a, b)
	})
	w.ExcessRoutes = slices.Clone(top[:min(5, len(top))])
	return int(minute + 60 - now.Unix())
}

func (m *tenantRequestMetrics) findings(now time.Time) []healthFinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	findings := []healthFinding{}
	for id, w := range m.runners {
		if now.Sub(w.ExcessAt) <= 2*time.Minute {
			findings = append(findings, newHealthFinding("runner_request_budget", "instance", "runner", id, fmt.Sprintf("Runner exceeded its request budget (%d/%d per minute). Top routes: %s", w.ExcessCount, w.ExcessBudget, strings.Join(w.ExcessRoutes, ", ")), "The runner operator checks polling and read volume on the named routes.", w.Projects, healthEvidence{Counts: map[string]int{"requests": w.ExcessCount, "budget": w.ExcessBudget}, Signatures: w.ExcessRoutes}))
		}
		if now.Unix()/60*60 > w.Minute+120 {
			delete(m.runners, id)
		}
	}
	return findings
}

func metricCaller(credential apiCredential) (string, string) {
	if credential.Runner.RunnerID != "" {
		return "runner", credential.Runner.RunnerID
	}
	if credential.SessionHash != "" && credential.Hosted != nil {
		return "browser", credential.Hosted.Subject
	}
	if credential.HostedKeyScope != "" {
		return "mcp", credential.ID
	}
	if credential.ID != "" {
		return "ci", credential.ID
	}
	return "anonymous", ""
}

func (s *Service) measureRequests(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		err := next(c)
		if s.requestMetrics == nil {
			return err
		}
		credential := apiCredential{}
		if current, ok := c.Get("hub_api_credential").(apiCredential); ok {
			credential = current
		}
		kind, caller := metricCaller(credential)
		if kind == "anonymous" {
			if session, ok := c.Get("hosted_session").(auth.Session); ok && session.Identity != nil {
				kind, caller = "browser", session.Identity.Subject
			}
		}
		status := c.Response().Status
		if err != nil {
			status = 500
			var failure *echo.HTTPError
			if errors.As(err, &failure) {
				status = failure.Code
			}
		}
		s.requestMetrics.record(s.config.now(), metricRoute(s.requestMetrics.knownRoutes(s.echo), c.Request().Method, c.Path()), kind, caller, status, time.Since(start))
		return err
	}
}

const unmatchedMetricRoute = "<unmatched>"

var metricMethods = map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true}

// metricRoute keys a request by method and registered route template. Unknown
// methods and paths that match no registered route collapse to constants, so
// clients cannot grow the metrics store with arbitrary keys.
func metricRoute(routes map[string]bool, method, path string) string {
	if !metricMethods[method] {
		method = "OTHER"
	}
	if !routes[method+" "+path] {
		path = unmatchedMetricRoute
	}
	return method + " " + path
}

func (m *tenantRequestMetrics) knownRoutes(e *echo.Echo) map[string]bool {
	m.routesOnce.Do(func() {
		m.routes = make(map[string]bool)
		for _, route := range e.Routes() {
			m.routes[route.Method+" "+route.Path] = true
		}
	})
	return m.routes
}
