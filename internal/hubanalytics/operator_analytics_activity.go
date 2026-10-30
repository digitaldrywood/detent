package hubanalytics

import (
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type ActivityTiming struct {
	ProfilesObserved    int                               `json:"profiles_observed"`
	ProfilesUnavailable int                               `json:"profiles_unavailable"`
	Timing              workflowmetrics.ActivityBreakdown `json:"timing"`
	UnallocatedSeconds  float64                           `json:"unallocated_seconds"`
	Partial             bool                              `json:"partial"`
}

type nativeAnalyticsActivity struct {
	ActivityTiming
	DroppedEvents     uint64 `json:"dropped_events"`
	UnpairedEvents    uint64 `json:"unpaired_events"`
	DetailOmitted     uint64 `json:"detail_omitted"`
	ProjectionOmitted uint64 `json:"projection_omitted"`
	OmissionCoverage  string `json:"omission_coverage"`
}

func ValidActivity(p workflowmetrics.ActivityProfile) bool {
	end := p.AsOf
	if !p.FinishedAt.IsZero() {
		end = p.FinishedAt
	}
	if p.Schema != 1 || p.StartedAt.IsZero() || p.AsOf.Before(p.StartedAt) || end.Before(p.StartedAt) || end.After(p.AsOf) || len(p.Spans) > 1024 || len(p.Sources) > 64 {
		return false
	}
	ids := make(map[string]bool, len(p.Spans))
	for _, span := range p.Spans {
		if span.ID == "" || ids[span.ID] || span.StartedAt.IsZero() || !span.FinishedAt.IsZero() && span.FinishedAt.Before(span.StartedAt) {
			return false
		}
		ids[span.ID] = true
	}
	if p.Summary == nil {
		return p.DetailOmitted == 0 && p.ProjectionOmitted == 0
	}
	s := p.Summary
	if s.Through.Before(p.StartedAt) || s.Through.After(end) || s.DetailFrom.Before(p.StartedAt) || s.DetailFrom.After(s.Through) {
		return false
	}
	if len(s.Hourly) > workflowmetrics.ActivityHourLimit || !validBreakdown(s.Breakdown, s.Through.Sub(p.StartedAt).Seconds()) {
		return false
	}
	previous := p.StartedAt
	var elapsed, observed, unknown, concurrent float64
	kinds := make(map[string]float64)
	for _, hour := range s.Hourly {
		if hour.From.Before(previous) || !hour.To.After(hour.From) || hour.To.After(s.Through) || hour.To.After(hour.From.UTC().Truncate(time.Hour).Add(time.Hour)) || !validBreakdown(hour.Breakdown, hour.To.Sub(hour.From).Seconds()) {
			return false
		}
		previous = hour.To
		elapsed += hour.Breakdown.ElapsedSeconds
		observed += hour.Breakdown.ObservedSeconds
		unknown += hour.Breakdown.UnknownSeconds
		concurrent += hour.Breakdown.ConcurrentSeconds
		for kind, seconds := range hour.Breakdown.ByKind {
			kinds[kind] += seconds
		}
	}
	if elapsed > s.Breakdown.ElapsedSeconds+0.001 || observed > s.Breakdown.ObservedSeconds+0.001 || unknown > s.Breakdown.UnknownSeconds+0.001 || concurrent > s.Breakdown.ConcurrentSeconds+0.001 {
		return false
	}
	for kind, seconds := range kinds {
		if seconds > s.Breakdown.ByKind[kind]+0.001 {
			return false
		}
	}
	return true
}

func validBreakdown(b workflowmetrics.ActivityBreakdown, elapsed float64) bool {
	for _, seconds := range []float64{b.ElapsedSeconds, b.ObservedSeconds, b.UnknownSeconds, b.ConcurrentSeconds} {
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
			return false
		}
	}
	sum := 0.0
	for _, seconds := range b.ByKind {
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
			return false
		}
		sum += seconds
	}
	return math.Abs(b.ElapsedSeconds-elapsed) < 0.001 && math.Abs(b.ElapsedSeconds-b.ObservedSeconds-b.UnknownSeconds) < 0.001 && b.ConcurrentSeconds <= b.ObservedSeconds && math.Abs(sum-b.ElapsedSeconds) < 0.001 && math.Abs(b.ByKind["unobserved"]-b.UnknownSeconds) < 0.001 && math.Abs(b.ByKind["concurrent"]-b.ConcurrentSeconds) < 0.001
}
