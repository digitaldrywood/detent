package postgres

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ metricsstore.Repository = (*Repository)(nil)

func (r *Repository) MergeRequestMetrics(ctx context.Context, organization tracker.OrganizationID, now time.Time, records []metricsstore.Metric) error {
	if len(records) > metricsstore.MergeLimit {
		return metricsstore.ErrInvalidMetric
	}
	pending := slices.Clone(records)
	for i := range pending {
		m := &pending[i]
		if !m.Valid() || m.At.IsZero() || len(m.Route) == 0 || len(m.Route) > 1024 || len(m.CallerID) > 512 || m.StatusClass < 0 || m.StatusClass > 9 {
			return metricsstore.ErrInvalidMetric
		}
		switch m.CallerKind {
		case "runner", "browser", "mcp", "ci", "anonymous", "other":
		default:
			return metricsstore.ErrInvalidMetric
		}
		m.At = m.At.UTC().Truncate(time.Minute)
	}
	slices.SortFunc(pending, func(a, b metricsstore.Metric) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Route, b.Route), cmp.Compare(a.CallerKind, b.CallerKind), cmp.Compare(a.CallerID, b.CallerID), cmp.Compare(a.StatusClass, b.StatusClass))
	})
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		minuteCutoff := now.Add(-metricsstore.MinuteRetention).UTC().Truncate(time.Minute)
		hourCutoff := now.Add(-metricsstore.HourRetention).UTC().Truncate(time.Hour)
		var previous time.Time
		for _, m := range pending {
			if m.At.Before(minuteCutoff) || m.At.Equal(previous) {
				continue
			}
			if err := s.queries.LockRequestMetricWindow(ctx, pgquery.LockRequestMetricWindowParams{
				OrganizationID: organization,
				Minute:         m.At,
			}); err != nil {
				return err
			}
			previous = m.At
		}
		for _, m := range pending {
			if m.At.Before(minuteCutoff) {
				continue
			}
			kind, caller, err := s.metricCaller(ctx, m)
			if err != nil {
				return err
			}
			if err := s.queries.MergeRequestMinute(ctx, pgquery.MergeRequestMinuteParams{
				OrganizationID: organization,
				Minute:         m.At,
				Route:          m.Route,
				CallerKind:     kind,
				CallerID:       caller,
				StatusClass:    m.StatusClass,
				Count:          m.Count,
				SumMs:          m.Sum,
				MaxMs:          m.Max,
				Histogram:      m.Histogram[:],
			}); err != nil {
				return err
			}
		}
		slices.SortFunc(pending, func(a, b metricsstore.Metric) int {
			return cmp.Or(a.At.Truncate(time.Hour).Compare(b.At.Truncate(time.Hour)), cmp.Compare(a.Route, b.Route), cmp.Compare(a.StatusClass, b.StatusClass))
		})
		for _, m := range pending {
			hour := m.At.Truncate(time.Hour)
			if hour.Before(hourCutoff) {
				continue
			}
			if err := s.queries.MergeRequestHour(ctx, pgquery.MergeRequestHourParams{
				OrganizationID: organization,
				Hour:           hour,
				Route:          m.Route,
				StatusClass:    m.StatusClass,
				Count:          m.Count,
				SumMs:          m.Sum,
				MaxMs:          m.Max,
				Histogram:      m.Histogram[:],
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *session) metricCaller(ctx context.Context, m metricsstore.Metric) (string, string, error) {
	if m.CallerKind == metricsstore.OverflowKind && m.CallerID == metricsstore.OverflowCaller {
		return m.CallerKind, m.CallerID, nil
	}
	exists, err := s.queries.RequestMetricCallerExists(ctx, pgquery.RequestMetricCallerExistsParams{
		OrganizationID: s.organization,
		Minute:         m.At,
		CallerKind:     m.CallerKind,
		CallerID:       m.CallerID,
	})
	if err != nil || exists {
		return m.CallerKind, m.CallerID, err
	}
	reserved, err := s.queries.ReserveRequestMetricCaller(ctx, pgquery.ReserveRequestMetricCallerParams{
		OrganizationID: s.organization,
		Minute:         m.At,
	})
	if err != nil {
		return "", "", err
	}
	if reserved == 0 {
		return metricsstore.OverflowKind, metricsstore.OverflowCaller, nil
	}
	err = s.queries.InsertRequestMetricCaller(ctx, pgquery.InsertRequestMetricCallerParams{
		OrganizationID: s.organization,
		Minute:         m.At,
		CallerKind:     m.CallerKind,
		CallerID:       m.CallerID,
	})
	return m.CallerKind, m.CallerID, err
}

func (r *Repository) RequestMetrics(ctx context.Context, organization tracker.OrganizationID, page metricsstore.Page) ([]metricsstore.Metric, error) {
	if err := checkPage(page.Limit); err != nil {
		return nil, err
	}
	if !page.From.Before(page.To) {
		return nil, metricsstore.ErrInvalidMetric
	}
	after := metricsstore.Metric{At: page.From}
	if page.After != nil {
		after = *page.After
	}
	var records []metricsstore.Metric
	err := r.transaction(ctx, organization, corestore.Serving, true, func(s *session) error {
		switch page.Grain {
		case metricsstore.Minute:
			rows, err := s.queries.ListRequestMinutes(ctx, pgquery.ListRequestMinutesParams{
				OrganizationID:   organization,
				FromTime:         page.From,
				ToTime:           page.To,
				AfterMinute:      after.At,
				AfterRoute:       after.Route,
				AfterCallerKind:  after.CallerKind,
				AfterCallerID:    after.CallerID,
				AfterStatusClass: after.StatusClass,
				PageLimit:        int32(page.Limit),
			})
			if err != nil {
				return err
			}
			for _, row := range rows {
				records = append(records, metricFromMinute(row))
			}
		case metricsstore.Hour:
			rows, err := s.queries.ListRequestHours(ctx, pgquery.ListRequestHoursParams{
				OrganizationID:   organization,
				FromTime:         page.From,
				ToTime:           page.To,
				AfterHour:        after.At,
				AfterRoute:       after.Route,
				AfterStatusClass: after.StatusClass,
				PageLimit:        int32(page.Limit),
			})
			if err != nil {
				return err
			}
			for _, row := range rows {
				records = append(records, metricsstore.Metric{At: row.Hour, Route: row.Route, StatusClass: row.StatusClass, Latency: metricLatency(row.Count, row.SumMs, row.MaxMs, row.Histogram)})
			}
		case metricsstore.Day:
			rows, err := s.queries.ListRequestDays(ctx, pgquery.ListRequestDaysParams{
				OrganizationID:   organization,
				FromTime:         page.From,
				ToTime:           page.To,
				AfterDay:         after.At,
				AfterRoute:       after.Route,
				AfterCallerKind:  after.CallerKind,
				AfterStatusClass: after.StatusClass,
				PageLimit:        int32(page.Limit),
			})
			if err != nil {
				return err
			}
			for _, row := range rows {
				records = append(records, metricsstore.Metric{At: row.Day, Route: row.Route, CallerKind: row.CallerKind, StatusClass: row.StatusClass, Latency: metricsstore.Latency{Count: row.Count, Sum: row.SumMs, Max: row.MaxMs}})
			}
		default:
			return metricsstore.ErrInvalidMetric
		}
		return nil
	})
	return records, err
}

func metricLatency(count int64, sum, maximum float64, histogram []int64) metricsstore.Latency {
	l := metricsstore.Latency{Count: count, Sum: sum, Max: maximum}
	copy(l.Histogram[:], histogram)
	return l
}

func metricFromMinute(row pgquery.RequestMinute) metricsstore.Metric {
	return metricsstore.Metric{At: row.Minute, Route: row.Route, CallerKind: row.CallerKind, CallerID: row.CallerID, StatusClass: row.StatusClass, Latency: metricLatency(row.Count, row.SumMs, row.MaxMs, row.Histogram)}
}

func (r *Repository) RetainRequestMetrics(ctx context.Context, organization tracker.OrganizationID, now time.Time) error {
	minuteCutoff := now.Add(-metricsstore.MinuteRetention).UTC().Truncate(time.Minute)
	dayCutoff := now.Add(-metricsstore.DayRetention).UTC().Truncate(24 * time.Hour)
	for range metricsstore.RetentionLimit {
		more := false
		err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			row, err := s.queries.TakeExpiredRequestMinute(ctx, pgquery.TakeExpiredRequestMinuteParams{
				OrganizationID: organization,
				Cutoff:         minuteCutoff,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			more = true
			day := row.Minute.UTC().Truncate(24 * time.Hour)
			if day.Before(dayCutoff) {
				return nil
			}
			return s.queries.MergeRequestDay(ctx, pgquery.MergeRequestDayParams{
				OrganizationID: organization,
				Day:            day,
				Route:          row.Route,
				CallerKind:     row.CallerKind,
				StatusClass:    row.StatusClass,
				Count:          row.Count,
				SumMs:          row.SumMs,
				MaxMs:          row.MaxMs,
			})
		})
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	for _, expire := range []func(*session) error{
		func(s *session) error {
			_, err := s.queries.ExpireRequestHours(ctx, pgquery.ExpireRequestHoursParams{
				OrganizationID: organization,
				Cutoff:         now.Add(-metricsstore.HourRetention).UTC().Truncate(time.Hour),
			})
			return err
		},
		func(s *session) error {
			_, err := s.queries.ExpireRequestDays(ctx, pgquery.ExpireRequestDaysParams{
				OrganizationID: organization,
				Cutoff:         dayCutoff,
			})
			return err
		},
	} {
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error { return expire(s) }); err != nil {
			return err
		}
	}
	for range metricsstore.RetentionLimit {
		var expired int64
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			var err error
			expired, err = s.queries.ExpireRequestMetricWindows(ctx, pgquery.ExpireRequestMetricWindowsParams{
				OrganizationID: organization,
				Cutoff:         minuteCutoff,
			})
			return err
		}); err != nil {
			return err
		}
		if expired == 0 {
			break
		}
	}

	return nil
}

func (r *Repository) RequestMetricTotals(ctx context.Context, organization tracker.OrganizationID, from, to time.Time) ([]metricsstore.Metric, []metricsstore.Metric, error) {
	if !from.Before(to) || to.Sub(from) > time.Hour {
		return nil, nil, metricsstore.ErrInvalidMetric
	}
	callers, routes := []metricsstore.Metric{}, []metricsstore.Metric{}
	err := r.transaction(ctx, organization, corestore.Serving, true, func(s *session) error {
		rows, err := s.queries.RequestMetricTotals(ctx, pgquery.RequestMetricTotalsParams{OrganizationID: organization, FromTime: from, ToTime: to})
		if err != nil {
			return err
		}
		for _, row := range rows {
			metric := metricsstore.Metric{Route: row.Route, CallerKind: row.CallerKind, CallerID: row.CallerID, Latency: metricLatency(row.Count, row.SumMs, row.MaxMs, row.Histogram)}
			if row.CallerGroup {
				callers = append(callers, metric)
			} else {
				routes = append(routes, metric)
			}
		}
		return nil
	})
	return callers, routes, err
}

func (r *Repository) RequestMetricHistory(ctx context.Context, organization tracker.OrganizationID, from, to time.Time, bucket time.Duration) ([]metricsstore.Metric, error) {
	if !from.Before(to) || from.Before(to.Add(-metricsstore.HourRetention).UTC().Truncate(time.Hour)) || bucket < time.Hour || bucket%time.Hour != 0 || (to.Sub(from)+bucket-1)/bucket > metricsstore.BucketLimit {
		return nil, metricsstore.ErrInvalidMetric
	}
	records := []metricsstore.Metric{}
	err := r.transaction(ctx, organization, corestore.Serving, true, func(s *session) error {
		rows, err := s.queries.RequestMetricRouteHistory(ctx, pgquery.RequestMetricRouteHistoryParams{OrganizationID: organization, FromTime: from, ToTime: to, BucketSeconds: int64(bucket / time.Second)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			records = append(records, metricsstore.Metric{At: row.BucketTime.UTC(), Route: row.Route, StatusClass: row.StatusClass, Latency: metricLatency(row.Count, row.SumMs, row.MaxMs, row.Histogram)})
		}
		return nil
	})
	return records, err
}
