package requestmetrics

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

const sharedMetricKeyLimit = 2048

type sharedMetricKey struct {
	tenant hubmaintenance.Tenant
	metric Key
}

type RequestMetrics struct {
	Repository metricsstore.Repository
	mu         sync.Mutex
	pending    map[sharedMetricKey]Latency
	flushing   map[hubmaintenance.Tenant]bool
	inflight   int
}

func NewRequestMetrics(repository metricsstore.Repository) (*RequestMetrics, error) {
	if repository == nil {
		return nil, errors.New("request metrics repository is required")
	}
	return &RequestMetrics{Repository: repository, pending: make(map[sharedMetricKey]Latency), flushing: make(map[hubmaintenance.Tenant]bool)}, nil
}

func (m *RequestMetrics) Record(tenant hubmaintenance.Tenant, now time.Time, route, kind, caller string, status int, elapsed time.Duration) bool {
	if now.IsZero() || tenant.Organization == "" || len(tenant.Organization) > 128 || tenant.Generation <= 0 || len(route) > 1024 || len(caller) > 256 || status < 100 || status >= 600 || elapsed < 0 {
		return false
	}
	switch kind {
	case "runner", "browser", "mcp", "ci", "anonymous":
	default:
		kind, caller = metricsstore.OverflowKind, metricsstore.OverflowCaller
	}
	key := sharedMetricKey{tenant: tenant, metric: Key{Minute: now.Unix() / 60 * 60, Route: route, Kind: kind, Caller: caller, Status: status / 100}}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.pending[key]; !exists && len(m.pending)+m.inflight >= sharedMetricKeyLimit-1 {
		key.metric.Route, key.metric.Kind, key.metric.Caller = "OTHER <overflow>", metricsstore.OverflowKind, metricsstore.OverflowCaller
		if _, exists := m.pending[key]; !exists && len(m.pending)+m.inflight >= sharedMetricKeyLimit {
			return false
		}
	}
	value := m.pending[key]
	value.Observe(elapsed)
	m.pending[key] = value
	return true
}

func (m *RequestMetrics) Flush(ctx context.Context, tenant hubmaintenance.Tenant, now time.Time) error {
	m.mu.Lock()
	if m.flushing[tenant] {
		m.mu.Unlock()
		return nil
	}
	batch := make(map[Key]Latency)
	for key, value := range m.pending {
		if key.tenant == tenant {
			batch[key.metric] = value
			delete(m.pending, key)
			if len(batch) == metricsstore.MergeLimit {
				break
			}
		}
	}
	if len(batch) == 0 {
		m.mu.Unlock()
		return nil
	}
	m.flushing[tenant] = true
	count := len(batch)
	m.inflight += count
	m.mu.Unlock()
	writer := &Service{Repository: m.Repository, Organization: tracker.OrganizationID(tenant.Organization)}
	err := writer.writeRepository(ctx, now, batch)
	m.mu.Lock()
	m.inflight -= count
	delete(m.flushing, tenant)
	for metric, value := range batch {
		key := sharedMetricKey{tenant: tenant, metric: metric}
		current := m.pending[key]
		current.Merge(value)
		m.pending[key] = current
	}
	m.mu.Unlock()
	return err
}

type Key struct {
	Minute int64
	Route  string
	Kind   string
	Caller string
	Status int
}

type Latency struct {
	HTTP      Usage     `json:"-"`
	Count     int64     `json:"count"`
	Sum       float64   `json:"sum_ms"`
	P50       float64   `json:"p50_ms"`
	P95       float64   `json:"p95_ms"`
	Max       float64   `json:"max_ms"`
	Histogram [64]int64 `json:"-"`
}

func (l *Latency) Observe(elapsed time.Duration) {
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

func (l *Latency) Merge(other Latency) {
	l.HTTP.Merge(other.HTTP)
	l.Count += other.Count
	l.Sum += other.Sum
	l.Max = max(l.Max, other.Max)
	for i, count := range other.Histogram {
		l.Histogram[i] += count
	}
}

type Top struct {
	Kind   string `json:"caller_kind,omitempty"`
	Caller string `json:"caller_id,omitempty"`
	Route  string `json:"route,omitempty"`
	Latency
}

type Report struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Callers []Top     `json:"top_callers"`
	Routes  []Top     `json:"top_routes"`
}

type RunnerWindow struct {
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

type Service struct {
	usage          *BillingOwner
	BillingPending *hostedRequestBillingBatch
	Repository     metricsstore.Repository
	Organization   tracker.OrganizationID
	mu             sync.Mutex
	flushMu        sync.Mutex
	pending        map[Key]Latency
	runners        map[string]*RunnerWindow
	routesOnce     sync.Once
	routes         map[string]bool
	ingested       atomic.Int64
}

func (m *Service) Record(now time.Time, route, kind, caller string, status int, elapsed time.Duration) {
	if now.IsZero() {
		return
	}
	key := Key{now.Unix() / 60 * 60, route, kind, caller, status / 100}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.pending[key]
	l.Observe(elapsed)
	m.pending[key] = l
}

func (m *Service) Flush(ctx context.Context, now time.Time) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if err := m.FlushBilling(ctx, now); err != nil {
		return err
	}
	m.mu.Lock()
	pending, owner := m.pending, m.usage
	m.pending = make(map[Key]Latency)
	m.mu.Unlock()
	latencies := make(map[Key]Latency)
	for key, value := range pending {
		if value.Count > 0 {
			latencies[key] = value
		}
	}
	err := m.write(ctx, now, latencies)
	if err == nil && m.Repository == nil {
		clear(latencies)
	}
	usagePending := make(map[Key]Latency, len(pending))
	for key, value := range pending {
		usagePending[key] = Latency{HTTP: value.HTTP}
	}
	if err == nil {
		if owner != nil {
			err = m.WriteUsage(ctx, owner, now, usagePending)
		} else {
			clear(usagePending)
		}
	}
	for key := range pending {
		remaining := latencies[key]
		remaining.HTTP = usagePending[key].HTTP
		pending[key] = remaining
	}
	m.mu.Lock()
	for key, value := range pending {
		if value.Count == 0 && value.HTTP.Requests == 0 {
			continue
		}
		current := m.pending[key]
		current.Merge(value)
		m.pending[key] = current
	}
	m.mu.Unlock()
	return err
}

func (m *Service) write(ctx context.Context, now time.Time, pending map[Key]Latency) error {

	return m.writeRepository(ctx, now, pending)
}

func (m *Service) Report(ctx context.Context, now time.Time) (Report, error) {

	return m.repositoryReport(ctx, now)
}

func RunnerExempt(method, route string) bool {
	return method == "POST" && (route == hubauth.NativeBase+"/claims" || strings.Contains(route, "/leases/") || strings.HasSuffix(route, "/heartbeat") || route == "/api/v2/organizations/:organization/runners"+"/:runner/renew")
}

type RunnerBudgetContext struct{}

func (m *Service) Admit(now time.Time, runner, route string, capacity int, projects []string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	minute := now.Unix() / 60 * 60
	w := m.runners[runner]
	if w == nil {
		w = &RunnerWindow{}
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

func (m *Service) Findings(now time.Time) []healthsignals.Finding {
	m.mu.Lock()
	defer m.mu.Unlock()
	findings := []healthsignals.Finding{}
	for id, w := range m.runners {
		if now.Sub(w.ExcessAt) <= 2*time.Minute {
			findings = append(findings, healthsignals.NewFinding("runner_request_budget", "instance", "runner", id, fmt.Sprintf("Runner exceeded its request budget (%d/%d per minute). Top routes: %s", w.ExcessCount, w.ExcessBudget, strings.Join(w.ExcessRoutes, ", ")), "The runner operator checks polling and read volume on the named routes.", w.Projects, healthsignals.Evidence{Counts: map[string]int{"requests": w.ExcessCount, "budget": w.ExcessBudget}, Signatures: w.ExcessRoutes}))
		}
		if now.Unix()/60*60 > w.Minute+120 {
			delete(m.runners, id)
		}
	}
	return findings
}

func Caller(credential hubauth.Credential) (string, string) {
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

const unmatchedMetricRoute = "<unmatched>"

var metricMethods = map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true}

func Route(routes map[string]bool, method, path string) string {
	if !metricMethods[method] {
		method = "OTHER"
	}
	if !routes[method+" "+path] {
		path = unmatchedMetricRoute
	}
	return method + " " + path
}

func (m *Service) KnownRoutes(e *echo.Echo) map[string]bool {
	m.routesOnce.Do(func() {
		m.routes = make(map[string]bool)
		for _, route := range e.Routes() {
			m.routes[route.Method+" "+route.Path] = true
		}
	})
	return m.routes
}
