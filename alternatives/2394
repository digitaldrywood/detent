package hubserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *database) openRequestMetrics(ctx context.Context, organization tracker.OrganizationID) (*tenantRequestMetrics, error) {
	if d.postgres == nil {
		return openRequestMetrics(ctx, d.path+".metrics.sqlite", organization)
	}
	if organization == "" {
		return nil, ErrHostedDatabaseBinding
	}
	return &tenantRequestMetrics{repository: d.postgres, organization: organization, pending: make(map[requestMetricKey]requestLatency), runners: make(map[string]*runnerRequestWindow)}, nil
}

func (m *tenantRequestMetrics) close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

func (m *tenantRequestMetrics) writeRepository(ctx context.Context, now time.Time, pending map[requestMetricKey]requestLatency) error {
	keys := make([]requestMetricKey, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b requestMetricKey) int {
		return cmp.Or(cmp.Compare(a.Minute, b.Minute), cmp.Compare(a.Route, b.Route), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Caller, b.Caller), cmp.Compare(a.Status, b.Status))
	})
	keys = keys[:min(len(keys), metricsstore.MergeLimit)]
	if len(keys) == 0 {
		return nil
	}
	records := make([]metricsstore.Metric, 0, len(keys))
	for _, key := range keys {
		l := pending[key]
		records = append(records, metricsstore.Metric{At: time.Unix(key.Minute, 0).UTC(), Route: key.Route, CallerKind: key.Kind, CallerID: key.Caller, StatusClass: int64(key.Status), Latency: metricsstore.Latency{Count: l.Count, Sum: l.Sum, Max: l.Max, Histogram: l.Histogram}})
	}
	if err := m.repository.MergeRequestMetrics(ctx, m.organization, now, records); err != nil {
		return err
	}
	for _, key := range keys {
		delete(pending, key)
	}
	return nil
}

func latencyFromMetric(m metricsstore.Metric) requestLatency {
	l := requestLatency{Count: m.Count, Sum: m.Sum, Max: m.Max, Histogram: m.Histogram}
	l.P50, l.P95 = m.Percentiles()
	return l
}

func (m *tenantRequestMetrics) repositoryReport(ctx context.Context, now time.Time) (requestMetricsReport, error) {
	to := now.UTC().Truncate(time.Minute)
	out := requestMetricsReport{From: to.Add(-time.Hour), To: to, Callers: []requestMetricTop{}, Routes: []requestMetricTop{}}
	callers, routes, err := m.repository.RequestMetricTotals(ctx, m.organization, out.From, out.To)
	if err != nil {
		return out, err
	}
	for _, row := range callers {
		out.Callers = append(out.Callers, requestMetricTop{Kind: row.CallerKind, Caller: row.CallerID, requestLatency: latencyFromMetric(row)})
	}
	for _, row := range routes {
		out.Routes = append(out.Routes, requestMetricTop{Route: row.Route, requestLatency: latencyFromMetric(row)})
	}
	return out, nil
}

func (m *tenantRequestMetrics) repositoryHistory(ctx context.Context, out routeLatencyHistory, bucket time.Duration) (routeLatencyHistory, error) {
	rows, err := m.repository.RequestMetricHistory(ctx, m.organization, out.From, out.To, bucket)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		out.Buckets = append(out.Buckets, routeLatencyBucket{From: row.At, Route: row.Route, StatusClass: fmt.Sprintf("%dxx", row.StatusClass), requestLatency: latencyFromMetric(row)})
	}
	return out, nil
}

func (m *tenantRequestMetrics) drain(ctx context.Context, now time.Time) error {
	for {
		if err := m.flush(ctx, now); err != nil {
			return err
		}
		m.mu.Lock()
		remaining := len(m.pending)
		m.mu.Unlock()
		if remaining == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
