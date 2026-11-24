package metricsstore

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	MinuteRetention = 14 * 24 * time.Hour
	HourRetention   = 90 * 24 * time.Hour
	DayRetention    = 400 * 24 * time.Hour
	CallerLimit     = 200
	MergeLimit      = 200
	RetentionLimit  = 200
	RouteLimit      = 20
	BucketLimit     = 240
	OverflowKind    = "other"
	OverflowCaller  = ""
)

var ErrInvalidMetric = errors.New("invalid request metric")

type Latency struct {
	Count     int64
	Sum       float64
	Max       float64
	Histogram [64]int64
}

func (l Latency) Percentiles() (float64, float64) {
	var total int64
	var p50, p95 float64
	for i, n := range l.Histogram {
		previous := total
		total += n
		if target := l.Count/2 + l.Count%2; previous < target && total >= target {
			p50 = min(l.Max, float64(uint64(1)<<i)/1000)
		}
		target := l.Count/100*95 + (l.Count%100*95+99)/100
		if previous < target && total >= target {
			p95 = min(l.Max, float64(uint64(1)<<i)/1000)
		}
	}
	return p50, p95
}

func (l Latency) Valid() bool {
	if l.Count <= 0 || l.Sum < 0 || l.Max < 0 || math.IsNaN(l.Sum) || math.IsNaN(l.Max) || math.IsInf(l.Sum, 0) || math.IsInf(l.Max, 0) {
		return false
	}
	var total int64
	for _, n := range l.Histogram {
		if n < 0 || n > l.Count-total {
			return false
		}
		total += n
	}
	return total == l.Count
}

type Metric struct {
	At          time.Time
	Route       string
	CallerKind  string
	CallerID    string
	StatusClass int64
	Latency
}

type Grain string

const (
	Minute Grain = "minute"
	Hour   Grain = "hour"
	Day    Grain = "day"
)

type Page struct {
	Grain Grain
	From  time.Time
	To    time.Time
	After *Metric
	Limit int
}

type Repository interface {
	MergeRequestMetrics(context.Context, tracker.OrganizationID, time.Time, []Metric) error
	RequestMetrics(context.Context, tracker.OrganizationID, Page) ([]Metric, error)
	RetainRequestMetrics(context.Context, tracker.OrganizationID, time.Time) error
	RequestMetricTotals(context.Context, tracker.OrganizationID, time.Time, time.Time) ([]Metric, []Metric, error)
	RequestMetricHistory(context.Context, tracker.OrganizationID, time.Time, time.Time, time.Duration) ([]Metric, error)
}

func PrepareMerge(records []Metric) ([]Metric, error) {
	if len(records) > MergeLimit {
		return nil, ErrInvalidMetric
	}
	pending := slices.Clone(records)
	for i := range pending {
		m := &pending[i]
		if !m.Valid() || m.At.IsZero() || len(m.Route) == 0 || len(m.Route) > 1024 || len(m.CallerID) > 512 || m.StatusClass < 0 || m.StatusClass > 9 {
			return nil, ErrInvalidMetric
		}
		switch m.CallerKind {
		case "runner", "browser", "mcp", "ci", "anonymous", "other":
		default:
			return nil, ErrInvalidMetric
		}
		m.At = m.At.UTC().Truncate(time.Minute)
	}
	slices.SortFunc(pending, func(a, b Metric) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Route, b.Route), cmp.Compare(a.CallerKind, b.CallerKind), cmp.Compare(a.CallerID, b.CallerID), cmp.Compare(a.StatusClass, b.StatusClass))
	})
	return pending, nil
}
