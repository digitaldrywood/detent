package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	requestHourRetention    = 90 * 24 * time.Hour
	routeLatencyRouteLimit  = 20
	routeLatencyBucketLimit = 240
)

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

// history returns per-route latency for [from, to) in buckets of at least an
// hour, limited to the busiest routes in the window.
func (m *tenantRequestMetrics) history(ctx context.Context, from, to time.Time, bucket time.Duration) (routeLatencyHistory, error) {
	start := from.UTC().Truncate(time.Hour)
	bucket = max(time.Hour, bucket.Truncate(time.Hour), (to.Sub(start)/routeLatencyBucketLimit + time.Hour - 1).Truncate(time.Hour))
	out := routeLatencyHistory{From: start, To: to.UTC(), Bucket: bucket.String(), Buckets: []routeLatencyBucket{}}

	return m.repositoryHistory(ctx, out, bucket)
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
