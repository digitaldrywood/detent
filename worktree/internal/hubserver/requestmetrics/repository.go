package requestmetrics

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
)

func (m *Service) Close() error {
	return nil
}

func (m *Service) writeRepository(ctx context.Context, now time.Time, pending map[Key]Latency) error {
	keys := make([]Key, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b Key) int {
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
	if err := m.Repository.MergeRequestMetrics(ctx, m.Organization, now, records); err != nil {
		return err
	}
	for _, key := range keys {
		delete(pending, key)
	}
	return nil
}

func latencyFromMetric(m metricsstore.Metric) Latency {
	l := Latency{Count: m.Count, Sum: m.Sum, Max: m.Max, Histogram: m.Histogram}
	l.P50, l.P95 = m.Percentiles()
	return l
}

func (m *Service) repositoryReport(ctx context.Context, now time.Time) (Report, error) {
	to := now.UTC().Truncate(time.Minute)
	out := Report{From: to.Add(-time.Hour), To: to, Callers: []Top{}, Routes: []Top{}}
	callers, routes, err := m.Repository.RequestMetricTotals(ctx, m.Organization, out.From, out.To)
	if err != nil {
		return out, err
	}
	for _, row := range callers {
		out.Callers = append(out.Callers, Top{Kind: row.CallerKind, Caller: row.CallerID, Latency: latencyFromMetric(row)})
	}
	for _, row := range routes {
		out.Routes = append(out.Routes, Top{Route: row.Route, Latency: latencyFromMetric(row)})
	}
	return out, nil
}

func (m *Service) repositoryHistory(ctx context.Context, out History, bucket time.Duration) (History, error) {
	rows, err := m.Repository.RequestMetricHistory(ctx, m.Organization, out.From, out.To, bucket)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		out.Buckets = append(out.Buckets, Bucket{From: row.At, Route: row.Route, StatusClass: fmt.Sprintf("%dxx", row.StatusClass), Latency: latencyFromMetric(row)})
	}
	return out, nil
}

func (m *Service) Drain(ctx context.Context, now time.Time) error {
	for {
		if err := m.Flush(ctx, now); err != nil {
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
