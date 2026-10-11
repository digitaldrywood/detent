package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
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
	Budget       int
	Routes       map[string]int
	Projects     []string
	ExcessAt     time.Time
	ExcessCount  int
	ExcessBudget int
	ExcessRoutes []string
}

type tenantRequestMetrics struct {
	db           *sql.DB
	organization tracker.OrganizationID
	mu           sync.Mutex
	flushMu      sync.Mutex
	pending      map[requestMetricKey]requestLatency
	runners      map[string]*runnerRequestWindow
}

func openRequestMetrics(ctx context.Context, path string, organization tracker.OrganizationID) (*tenantRequestMetrics, error) {
	if err := createPrivateDatabaseFile(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, time.Second))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := migrateRequestMetrics(ctx, db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO tenant(organization) SELECT ? WHERE NOT EXISTS(SELECT 1 FROM tenant)", organization); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	var bound string
	if err := db.QueryRowContext(ctx, "SELECT organization FROM tenant").Scan(&bound); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if bound != string(organization) {
		return nil, errors.Join(ErrHostedDatabaseBinding, db.Close())
	}
	return &tenantRequestMetrics{db: db, organization: organization, pending: make(map[requestMetricKey]requestLatency), runners: make(map[string]*runnerRequestWindow)}, nil
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
	m.mu.Lock()
	pending := m.pending
	m.pending = make(map[requestMetricKey]requestLatency)
	m.mu.Unlock()
	err := m.write(ctx, now, pending)
	if err != nil {
		m.mu.Lock()
		for key, l := range pending {
			current := m.pending[key]
			current.merge(l)
			m.pending[key] = current
		}
		m.mu.Unlock()
	}
	return err
}

func (m *tenantRequestMetrics) write(ctx context.Context, now time.Time, pending map[requestMetricKey]requestLatency) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cutoff := now.Add(-14*24*time.Hour).Unix() / 60 * 60
	if _, err := tx.ExecContext(ctx, "DELETE FROM request_minutes WHERE minute < ?", cutoff); err != nil {
		return err
	}
	for key, l := range pending {
		if key.Minute < cutoff {
			continue
		}
		var previous requestLatency
		var histogram string
		err := tx.QueryRowContext(ctx, "SELECT count,sum_ms,max_ms,histogram FROM request_minutes WHERE minute=? AND route=? AND caller_kind=? AND caller_id=? AND status_class=?", key.Minute, key.Route, key.Kind, key.Caller, key.Status).Scan(&previous.Count, &previous.Sum, &previous.Max, &histogram)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal([]byte(histogram), &previous.Histogram); err != nil {
				return err
			}
			l.merge(previous)
		}
		l.percentiles()
		raw, err := json.Marshal(l.Histogram)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO request_minutes VALUES(?,?,?,?,?,?,?,?,?,?,?)`, key.Minute, key.Route, key.Kind, key.Caller, key.Status, l.Count, l.Sum, l.P50, l.P95, l.Max, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *tenantRequestMetrics) report(ctx context.Context, now time.Time) (requestMetricsReport, error) {
	to := now.UTC().Truncate(time.Minute)
	out := requestMetricsReport{From: to.Add(-time.Hour), To: to, Callers: []requestMetricTop{}, Routes: []requestMetricTop{}}
	callers := make(map[[2]string]requestLatency)
	routes := make(map[string]requestLatency)
	rows, err := m.db.QueryContext(ctx, "SELECT route,caller_kind,caller_id,count,sum_ms,max_ms,histogram FROM request_minutes WHERE minute>=? AND minute<?", out.From.Unix(), out.To.Unix())
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var route, kind, caller, histogram string
		var l requestLatency
		if err := rows.Scan(&route, &kind, &caller, &l.Count, &l.Sum, &l.Max, &histogram); err != nil {
			return out, err
		}
		if err := json.Unmarshal([]byte(histogram), &l.Histogram); err != nil {
			return out, err
		}
		key := [2]string{kind, caller}
		a := callers[key]
		a.merge(l)
		callers[key] = a
		b := routes[route]
		b.merge(l)
		routes[route] = b
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	for key, l := range callers {
		l.percentiles()
		out.Callers = append(out.Callers, requestMetricTop{Kind: key[0], Caller: key[1], requestLatency: l})
	}
	for route, l := range routes {
		l.percentiles()
		out.Routes = append(out.Routes, requestMetricTop{Route: route, requestLatency: l})
	}
	for _, top := range [][]requestMetricTop{out.Callers, out.Routes} {
		slices.SortFunc(top, func(a, b requestMetricTop) int {
			if a.Count > b.Count {
				return -1
			}
			if a.Count < b.Count {
				return 1
			}
			return strings.Compare(a.Kind+a.Caller+a.Route, b.Kind+b.Caller+b.Route)
		})
	}
	out.Callers = out.Callers[:min(20, len(out.Callers))]
	out.Routes = out.Routes[:min(20, len(out.Routes))]
	return out, nil
}

func runnerRequestExempt(method, route string) bool {
	return method == "POST" && (route == nativeBase+"/claims" || strings.Contains(route, "/leases/") || strings.HasSuffix(route, "/heartbeat") || route == runnerBase+"/:runner/renew")
}

type runnerRequestBudgetContext struct{}

func (s *Service) admitRunnerRequest(c echo.Context, credential apiCredential) error {
	m := s.requestMetrics
	if m == nil || credential.Runner.RunnerID == "" {
		return nil
	}
	c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), runnerRequestBudgetContext{}, true)))
	if runnerRequestExempt(c.Request().Method, c.Path()) {
		return nil
	}
	var capacity int
	if err := s.database.reader.QueryRowContext(c.Request().Context(), "SELECT min(r.capacity_limit,r.reported_capacity,m.capacity) FROM runner_identities r JOIN machines m ON m.id=r.machine_id WHERE r.id=? AND r.organization_id=?", credential.Runner.RunnerID, m.organization).Scan(&capacity); err != nil {
		return s.nativeAPIError(c, err)
	}
	projects := make([]string, 0, len(credential.Runner.ProjectIDs))
	for _, id := range credential.Runner.ProjectIDs {
		projects = append(projects, string(id))
	}
	now := s.config.now().UTC()
	if retry := m.admit(now, credential.Runner.RunnerID, c.Request().Method+" "+c.Path(), capacity, projects); retry > 0 {
		c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
		return c.JSON(429, apiErrorResponse{Code: "runner_request_budget", Message: "Runner request budget exceeded; retry after the current minute"})
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
		w.Routes = make(map[string]int)
	}
	w.Budget = 120 + 60*min(10000, max(0, capacity))
	w.Projects = slices.Clone(projects)
	w.Count++
	w.Routes[route]++
	if w.Count <= w.Budget {
		return 0
	}
	w.ExcessAt = now
	w.ExcessCount = w.Count
	w.ExcessBudget = w.Budget
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
		route := c.Path()
		if route == "" {
			route = "<unmatched>"
		}
		status := c.Response().Status
		if err != nil {
			status = 500
			var failure *echo.HTTPError
			if errors.As(err, &failure) {
				status = failure.Code
			}
		}
		s.requestMetrics.record(s.config.now(), c.Request().Method+" "+route, kind, caller, status, time.Since(start))
		return err
	}
}
