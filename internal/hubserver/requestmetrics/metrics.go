package requestmetrics

import (
	"context"
	"fmt"
	"math/bits"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	sharedWindows  *RunnerWindows
	generation     int64
	localWindows   tenantRunnerWindows
	routesOnce     sync.Once
	routes         map[string]bool
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
	return m.flush(ctx, now, 0)
}

func (m *Service) HasPending() bool {
	if !m.flushMu.TryLock() {
		return true
	}
	defer m.flushMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) > 0 || m.BillingPending != nil
}

func (m *Service) FlushPage(ctx context.Context, now time.Time, limit int) error {
	if limit < 6 {
		return metricsstore.ErrInvalidMetric
	}
	if m.sharedWindows != nil {
		m.sharedWindows.expire(now)
	}
	return m.flush(ctx, now, limit)
}

func (m *Service) flush(ctx context.Context, now time.Time, limit int) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if limit > 0 && m.BillingPending != nil {
		return m.FlushBilling(ctx, now)
	}
	if err := m.FlushBilling(ctx, now); err != nil {
		return err
	}
	m.mu.Lock()
	pending, owner := m.pending, m.usage
	if limit == 0 {
		m.pending = make(map[Key]Latency)
	} else {
		pending = make(map[Key]Latency)
		for k, v := range m.pending {
			pending[k] = v
			delete(m.pending, k)
			if len(pending) == limit/6 {
				break
			}
		}
	}
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
			if limit == 0 {
				err = m.WriteUsage(ctx, owner, now, usagePending)
			} else {
				err = m.writeUsageWindows(ctx, owner, now, usagePending, min(4, limit/6))
			}
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

type minuteWindows[K comparable, V any] struct {
	minute  int64
	buckets [3]map[K]*V
}

func (b *minuteWindows[K, V]) advance(now time.Time) {
	minute := now.Unix() / 60
	if minute <= b.minute {
		return
	}
	elapsed := min(minute-b.minute, int64(len(b.buckets)))
	copy(b.buckets[elapsed:], b.buckets[:])
	clear(b.buckets[:elapsed])
	b.minute = minute
}

func (b *minuteWindows[K, V]) lookup(key K, record bool) *V {
	if v := b.buckets[0][key]; v != nil {
		return v
	}
	var v *V
	for _, bucket := range b.buckets[1:] {
		if v = bucket[key]; v != nil {
			if record {
				delete(bucket, key)
			}
			break
		}
	}
	if record {
		if v == nil {
			v = new(V)
		}
		if b.buckets[0] == nil {
			b.buckets[0] = make(map[K]*V)
		}
		b.buckets[0][key] = v
	}
	return v
}

type tenantRunnerWindows struct {
	mu      sync.Mutex
	runners minuteWindows[string, RunnerWindow]
}

type windowScope struct {
	organization tracker.OrganizationID
	generation   int64
}

type RunnerWindows struct {
	mu      sync.Mutex
	tenants minuteWindows[windowScope, tenantRunnerWindows]
}

func (w *RunnerWindows) expire(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tenants.advance(now)
}

func (m *Service) runnerWindows(now time.Time, record bool) *tenantRunnerWindows {
	w := m.sharedWindows
	if w == nil {
		return &m.localWindows
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tenants.advance(now)
	return w.tenants.lookup(windowScope{m.Organization, m.generation}, record)
}

func (m *Service) Admit(now time.Time, runner, route string, capacity int, projects []string) int {
	group := m.runnerWindows(now, true)
	group.mu.Lock()
	defer group.mu.Unlock()
	group.runners.advance(now)
	w := group.runners.lookup(runner, true)
	minute := now.Unix() / 60 * 60
	if w.Minute != minute || w.Routes == nil {
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
	findings := []healthsignals.Finding{}
	group := m.runnerWindows(now, false)
	if group == nil {
		return findings
	}
	group.mu.Lock()
	defer group.mu.Unlock()
	group.runners.advance(now)
	for _, bucket := range group.runners.buckets {
		for id, w := range bucket {
			if now.Sub(w.ExcessAt) <= 2*time.Minute {
				findings = append(findings, healthsignals.NewFinding("runner_request_budget", "instance", "runner", id, fmt.Sprintf("Runner exceeded its request budget (%d/%d per minute). Top routes: %s", w.ExcessCount, w.ExcessBudget, strings.Join(w.ExcessRoutes, ", ")), "The runner operator checks polling and read volume on the named routes.", w.Projects, healthsignals.Evidence{Counts: map[string]int{"requests": w.ExcessCount, "budget": w.ExcessBudget}, Signatures: w.ExcessRoutes}))
			}
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
