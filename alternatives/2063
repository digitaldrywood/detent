package hubserver

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	requestHourRetention    = 90 * 24 * time.Hour
	routeLatencyRouteLimit  = 20
	routeLatencyBucketLimit = 240
)

type requestHourKey struct {
	Hour   int64
	Route  string
	Status int
}

type routeLatencyBucket struct {
	From        time.Time `json:"from"`
	Route       string    `json:"route"`
	StatusClass string    `json:"status_class"`
	requestLatency
}

type routeLatencyHistory struct {
	From    time.Time            `json:"from"`
	To      time.Time            `json:"to"`
	Bucket  string               `json:"bucket"`
	Buckets []routeLatencyBucket `json:"buckets"`
}

// writeRequestHours folds one flush of minute observations into the hourly
// per-route history, inside the metrics store's own transaction.
func writeRequestHours(ctx context.Context, tx *sql.Tx, now time.Time, pending map[requestMetricKey]requestLatency) error {
	cutoff := now.Add(-requestHourRetention).Unix() / 3600 * 3600
	if _, err := tx.ExecContext(ctx, "DELETE FROM request_hours WHERE hour < ?", cutoff); err != nil {
		return fmt.Errorf("prune request hours: %w", err)
	}
	hours := make(map[requestHourKey]requestLatency)
	for key, l := range pending {
		hour := requestHourKey{key.Minute / 3600 * 3600, key.Route, key.Status}
		if hour.Hour < cutoff {
			continue
		}
		current := hours[hour]
		current.merge(l)
		hours[hour] = current
	}
	for key, l := range hours {
		var previous requestLatency
		var histogram string
		err := tx.QueryRowContext(ctx, "SELECT count,sum_ms,max_ms,histogram FROM request_hours WHERE hour=? AND route=? AND status_class=?", key.Hour, key.Route, key.Status).Scan(&previous.Count, &previous.Sum, &previous.Max, &histogram)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read request hour: %w", err)
		}
		if err == nil {
			if err := json.Unmarshal([]byte(histogram), &previous.Histogram); err != nil {
				return fmt.Errorf("decode request hour: %w", err)
			}
			l.merge(previous)
		}
		l.percentiles()
		raw, err := json.Marshal(l.Histogram)
		if err != nil {
			return fmt.Errorf("encode request hour: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO request_hours(hour,route,status_class,count,sum_ms,p50_ms,p95_ms,max_ms,histogram) VALUES(?,?,?,?,?,?,?,?,?)", key.Hour, key.Route, key.Status, l.Count, l.Sum, l.P50, l.P95, l.Max, string(raw)); err != nil {
			return fmt.Errorf("write request hour: %w", err)
		}
	}
	return nil
}

// history returns per-route latency for [from, to) in buckets of at least an
// hour, limited to the busiest routes in the window.
func (m *tenantRequestMetrics) history(ctx context.Context, from, to time.Time, bucket time.Duration) (routeLatencyHistory, error) {
	start := from.UTC().Truncate(time.Hour)
	bucket = max(time.Hour, bucket.Truncate(time.Hour), (to.Sub(start)/routeLatencyBucketLimit + time.Hour - 1).Truncate(time.Hour))
	out := routeLatencyHistory{From: start, To: to.UTC(), Bucket: bucket.String(), Buckets: []routeLatencyBucket{}}
	rows, err := m.db.QueryContext(ctx, `WITH top AS (SELECT route FROM request_hours WHERE hour>=:from AND hour<:to GROUP BY route ORDER BY sum(count) DESC, route LIMIT :limit)
SELECT h.hour,h.route,h.status_class,h.count,h.sum_ms,h.max_ms,h.histogram FROM request_hours h JOIN top ON top.route=h.route WHERE h.hour>=:from AND h.hour<:to`,
		sql.Named("from", start.Unix()), sql.Named("to", to.Unix()), sql.Named("limit", routeLatencyRouteLimit))
	if err != nil {
		return out, fmt.Errorf("query request hours: %w", err)
	}
	defer rows.Close()
	buckets := make(map[requestHourKey]requestLatency)
	for rows.Next() {
		var key requestHourKey
		var histogram string
		var l requestLatency
		if err := rows.Scan(&key.Hour, &key.Route, &key.Status, &l.Count, &l.Sum, &l.Max, &histogram); err != nil {
			return out, fmt.Errorf("scan request hour: %w", err)
		}
		if err := json.Unmarshal([]byte(histogram), &l.Histogram); err != nil {
			return out, fmt.Errorf("decode request hour: %w", err)
		}
		key.Hour = start.Add((time.Unix(key.Hour, 0).Sub(start) / bucket) * bucket).Unix()
		current := buckets[key]
		current.merge(l)
		buckets[key] = current
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("read request hours: %w", err)
	}
	for key, l := range buckets {
		l.percentiles()
		out.Buckets = append(out.Buckets, routeLatencyBucket{From: time.Unix(key.Hour, 0).UTC(), Route: key.Route, StatusClass: fmt.Sprintf("%dxx", key.Status), requestLatency: l})
	}
	slices.SortFunc(out.Buckets, func(a, b routeLatencyBucket) int {
		return cmp.Or(a.From.Compare(b.From), cmp.Compare(a.Route, b.Route), cmp.Compare(a.StatusClass, b.StatusClass))
	})
	return out, nil
}

func (s *Service) adminRouteLatency(ctx context.Context, from, to time.Time, bucket time.Duration) (routeLatencyHistory, bool, error) {
	if !s.requestMetricsVisible(ctx) {
		return routeLatencyHistory{}, false, nil
	}
	history, err := s.requestMetrics.history(ctx, from, to, bucket)
	return history, true, err
}

type requestMetricsResponse struct {
	requestMetricsReport
	RouteLatency *routeLatencyHistory `json:"route_latency,omitempty"`
}

func (s *Service) hostedRequestMetrics(c echo.Context) error {
	ctx := c.Request().Context()
	if !s.requestMetricsVisible(ctx) {
		return s.hostedJSONError(c, http.StatusForbidden, "Staff access is required to read request metrics")
	}
	var window time.Duration
	if raw := c.QueryParam("window"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Hour || parsed > requestHourRetention {
			return s.hostedJSONError(c, http.StatusBadRequest, "window must be a duration between 1h and 2160h")
		}
		window = parsed
	}
	now := s.config.now().UTC()
	report, err := s.requestMetrics.report(ctx, now)
	response := requestMetricsResponse{requestMetricsReport: report}
	if err == nil && window > 0 {
		var history routeLatencyHistory
		history, err = s.requestMetrics.history(ctx, now.Add(-window), now, time.Hour)
		response.RouteLatency = &history
	}
	if err != nil {
		s.config.Logger.Warn("read request metrics", "error", err)
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Request metrics are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, response)
}
