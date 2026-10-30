package hubserver

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func projectNativeAnalyticsActivity(out *hubanalytics.Project, a *hubanalytics.Attempt, raw string) {
	var p workflowmetrics.ActivityProfile
	if raw == "null" {
		a.ActivityUnavailable = "missing_receipt"
	} else if json.Unmarshal([]byte(raw), &p) != nil || !hubanalytics.ValidActivity(p) {
		a.ActivityUnavailable = "malformed_receipt"
	} else if !a.PublicActivity {
		p = workflowmetrics.PublicActivityProfile(p)
		if !hubanalytics.ValidActivity(p) {
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
	out.Activity.OmissionCoverage = "cumulative_selected_attempt_receipts; hourly_timing_when_retained; omission_counts_not_hourly_attributed"
	addNativeAnalyticsActivity(&out.Activity.ActivityTiming, p, out.Window.From, out.Window.To)
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

func addNativeAnalyticsActivity(out *hubanalytics.ActivityTiming, p workflowmetrics.ActivityProfile, from, to time.Time) {
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
	b, unallocated := p.BreakdownBetween(start, finish)
	out.UnallocatedSeconds += unallocated
	out.Partial = out.Partial || unallocated > 0
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

func nativeAnalyticsAttemptPage(attempts []hubanalytics.Attempt, offset, limit int) operatortool.ReadPage[hubanalytics.Attempt] {
	page := operatortool.OffsetPage(attempts, offset, limit)
	page.Items = slices.Clone(page.Items)
	for i := range page.Items {
		page.Items[i].Pipeline = nil
	}
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
