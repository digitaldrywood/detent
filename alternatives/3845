package requestmetrics

import (
	"context"
	"time"
)

const (
	requestHourRetention    = 90 * 24 * time.Hour
	routeLatencyRouteLimit  = 20
	routeLatencyBucketLimit = 240
)

type Bucket struct {
	From        time.Time `json:"from"`
	Route       string    `json:"route"`
	StatusClass string    `json:"status_class"`
	Latency
}

type History struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Bucket  string    `json:"bucket"`
	Buckets []Bucket  `json:"buckets"`
}

func (m *Service) History(ctx context.Context, from, to time.Time, bucket time.Duration) (History, error) {
	start := from.UTC().Truncate(time.Hour)
	bucket = max(time.Hour, bucket.Truncate(time.Hour), (to.Sub(start)/routeLatencyBucketLimit + time.Hour - 1).Truncate(time.Hour))
	out := History{From: start, To: to.UTC(), Bucket: bucket.String(), Buckets: []Bucket{}}

	return m.repositoryHistory(ctx, out, bucket)
}
