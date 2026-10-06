package hubserver

import (
	"encoding/json"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type nativeAnalyticsActivityTiming struct {
	ProfilesObserved    int                               `json:"profiles_observed"`
	ProfilesUnavailable int                               `json:"profiles_unavailable"`
	Timing              workflowmetrics.ActivityBreakdown `json:"timing"`
	UnallocatedSeconds  float64                           `json:"unallocated_seconds"`
	Partial             bool                              `json:"partial"`
}

type nativeAnalyticsActivity struct {
	nativeAnalyticsActivityTiming
	DroppedEvents     uint64 `json:"dropped_events"`
	UnpairedEvents    uint64 `json:"unpaired_events"`
	DetailOmitted     uint64 `json:"detail_omitted"`
	ProjectionOmitted uint64 `json:"projection_omitted"`
	OmissionCoverage  string `json:"omission_coverage"`
}

func projectNativeAnalyticsActivity(out *nativeAnalyticsProject, a *nativeAnalyticsAttempt, raw string) {
	var p workflowmetrics.ActivityProfile
	if raw == "null" {
		a.ActivityUnavailable = "missing_receipt"
	} else if json.Unmarshal([]byte(raw), &p) != nil || !validAnalyticsActivity(p) {
		a.ActivityUnavailable = "malformed_receipt"
	} else {
		p = workflowmetrics.PublicActivityProfile(p)
		if !validAnalyticsActivity(p) {
			a.ActivityUnavailable = "malformed_receipt"
		}
	}
	if a.ActivityUnavailable != "" {
		out.Activity.ProfilesUnavailable++
		out.Activity.Partial = true
		for i := range out.Digest {
			bucket := &out.Digest[i]
			if a.StartedAt.Before(bucket.To) && a.ObservedAt.After(bucket.From) {
				bucket.Activity.ProfilesUnavailable++
				bucket.Activity.Partial = true
			}
		}
		return
	}
	out.Activity.OmissionCoverage = "cumulative_selected_attempt_receipts; hourly_attribution_unavailable"
	addNativeAnalyticsActivity(&out.Activity.nativeAnalyticsActivityTiming, p, out.Window.From, out.Window.To)
	out.Activity.DroppedEvents += p.Dropped
	out.Activity.UnpairedEvents += p.Unpaired
	out.Activity.DetailOmitted += p.DetailOmitted
	out.Activity.ProjectionOmitted += p.ProjectionOmitted
	for i := range out.Digest {
		bucket := &out.Digest[i]
		addNativeAnalyticsActivity(&bucket.Activity, p, bucket.From, bucket.To)
	}
	if p.AsOf.After(out.SourceAt) {
		out.SourceAt = p.AsOf
	}
	runtime := tracker.NativeRuntimeObservation{Activity: &p}
	if summary := runtime.WithoutActivitySpans(); summary != nil {
		a.Activity = summary.Activity
	}
}

func addNativeAnalyticsActivity(out *nativeAnalyticsActivityTiming, p workflowmetrics.ActivityProfile, from, to time.Time) {
	end := p.AsOf
	if !p.FinishedAt.IsZero() {
		end = p.FinishedAt
	}
	start, finish := maxTime(p.StartedAt, from), minTime(end, to)
	if !finish.After(start) {
		return
	}
	out.ProfilesObserved++
	out.Partial = out.Partial || p.Coverage != "complete" || p.Dropped > 0 || p.Unpaired > 0 || p.DetailOmitted > 0 || p.ProjectionOmitted > 0
	var b workflowmetrics.ActivityBreakdown
	if !start.After(p.StartedAt) && !finish.Before(end) {
		b = p.Breakdown()
	} else {
		if p.Summary != nil && start.Before(p.Summary.DetailFrom) {
			detailStart := minTime(finish, p.Summary.DetailFrom)
			out.UnallocatedSeconds += detailStart.Sub(start).Seconds()
			out.Partial = true
			start = detailStart
		}
		p.StartedAt, p.AsOf, p.FinishedAt, p.Summary = start, finish, time.Time{}, nil
		b = p.Breakdown()
	}
	out.Timing.ElapsedSeconds += b.ElapsedSeconds
	out.Timing.ObservedSeconds += b.ObservedSeconds
	out.Timing.UnknownSeconds += b.UnknownSeconds
	out.Timing.ConcurrentSeconds += b.ConcurrentSeconds
	if out.Timing.ByKind == nil {
		out.Timing.ByKind = make(map[string]float64)
	}
	for kind, seconds := range b.ByKind {
		out.Timing.ByKind[kind] += seconds
	}
}

func validAnalyticsActivity(p workflowmetrics.ActivityProfile) bool {
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
	b := s.Breakdown
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
	return math.Abs(b.ElapsedSeconds-s.Through.Sub(p.StartedAt).Seconds()) < 0.001 && math.Abs(b.ElapsedSeconds-b.ObservedSeconds-b.UnknownSeconds) < 0.001 && b.ConcurrentSeconds <= b.ObservedSeconds && math.Abs(sum-b.ElapsedSeconds) < 0.001 && math.Abs(b.ByKind["unobserved"]-b.UnknownSeconds) < 0.001 && math.Abs(b.ByKind["concurrent"]-b.ConcurrentSeconds) < 0.001
}

func nativeAnalyticsAttemptPage(attempts []nativeAnalyticsAttempt, offset, limit int) operatortool.ReadPage[nativeAnalyticsAttempt] {
	page := operatortool.OffsetPage(attempts, offset, limit)
	size := 2
	for i, item := range page.Items {
		raw, err := json.Marshal(item)
		if err != nil {
			return page
		}
		if i > 0 && size+len(raw)+1 > operatortool.WorkListPageBytes {
			page.Items = page.Items[:i]
			next := offset + i
			page.NextOffset = &next
			break
		}
		size += len(raw) + 1
	}

	return page
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
