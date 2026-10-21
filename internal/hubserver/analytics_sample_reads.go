package hubserver

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func mergeHistogramDurations(durations ...operatortool.AnalyticsDuration) operatortool.AnalyticsDuration {
	out := operatortool.AnalyticsDuration{}
	bins := map[int]operatortool.AnalyticsDurationBucket{}
	add := func(b operatortool.AnalyticsDurationBucket) {
		key := int(math.Ceil(math.Log2(b.Maximum+1) * 16))
		old, exists := bins[key]
		if !exists {
			bins[key] = b
			return
		}
		old.Count += b.Count
		old.Minimum = min(old.Minimum, b.Minimum)
		old.Maximum = max(old.Maximum, b.Maximum)
		bins[key] = old
	}
	for _, d := range durations {
		out.Count += d.Count
		out.Seconds += d.Seconds
		for _, b := range d.Histogram {
			add(b)
		}
		for _, v := range d.Samples {
			add(operatortool.AnalyticsDurationBucket{Count: 1, Minimum: v, Maximum: v})
		}
	}
	for _, b := range bins {
		out.Histogram = append(out.Histogram, b)
	}
	slices.SortFunc(out.Histogram, func(a, b operatortool.AnalyticsDurationBucket) int {
		if a.Minimum < b.Minimum {
			return -1
		}
		if a.Minimum > b.Minimum {
			return 1
		}
		return 0
	})
	valueAt := func(rank int) float64 {
		for _, b := range out.Histogram {
			if rank < b.Count {
				if b.Count == 1 {
					return (b.Minimum + b.Maximum) / 2
				}
				return b.Minimum + (b.Maximum-b.Minimum)*float64(rank)/float64(b.Count-1)
			}
			rank -= b.Count
		}
		return 0
	}
	quantile := func(q float64) float64 {
		rank := q * float64(max(0, out.Count-1))
		low := int(math.Floor(rank))
		high := int(math.Ceil(rank))
		lower := valueAt(low)
		return lower + (valueAt(high)-lower)*(rank-float64(low))
	}
	out.P50Seconds, out.P90Seconds = quantile(.5), quantile(.9)
	return out
}

func readAnalyticsResidenceHours(ctx context.Context, q nativeQueryer, scope nativeScope, from, to time.Time) (operatortool.AnalyticsResidenceSummary, int, bool, error) {
	out := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	rows, err := q.QueryContext(ctx, `SELECT hour,metric,dimension,count,total,minimum,maximum,unavailable FROM analytics_sample_hours WHERE organization_id=? AND project_id=? AND metric LIKE 'residence_%' AND hour<? AND hour+3600>? ORDER BY metric,dimension,bucket`, scope.organization, scope.project, to.Unix(), from.Unix())
	if err != nil {
		return out, 0, false, err
	}
	defer rows.Close()
	durations := map[string][]operatortool.AnalyticsDuration{}
	causes := map[string]operatortool.AnalyticsReworkCause{}
	observed, found := 0, false
	for rows.Next() {
		var hour int64
		var metric, dimension string
		var count, unavailable int
		var total, minimum, maximum float64
		if err := rows.Scan(&hour, &metric, &dimension, &count, &total, &minimum, &maximum, &unavailable); err != nil {
			return out, 0, false, err
		}
		found = true
		out.Partial = out.Partial || unavailable > 0
		start := time.Unix(hour, 0).UTC()
		fraction := minTime(to, start.Add(time.Hour)).Sub(maxTime(from, start)).Seconds() / 3600
		out.Partial = out.Partial || fraction < 1
		if metric == "residence_coverage" {
			observed += count
			continue
		}
		if metric == "residence_cause" {
			var key [2]string
			if err := json.Unmarshal([]byte(dimension), &key); err != nil {
				return out, 0, false, err
			}
			cause := causes[dimension]
			cause.FromState, cause.ReasonDetail = key[0], key[1]
			cause.Count += count
			causes[dimension] = cause
			continue
		}
		d := operatortool.AnalyticsDuration{Count: count, Seconds: total * fraction, Histogram: []operatortool.AnalyticsDurationBucket{{Count: count, Minimum: minimum, Maximum: maximum}}}
		durations[metric+":"+dimension] = append(durations[metric+":"+dimension], d)
	}
	for key, values := range durations {
		d := mergeHistogramDurations(values...)
		switch key {
		case "residence_system:":
			out.SystemTotal = d
		case "residence_held:":
			out.HeldTotal = d
		case "residence_queue:":
			out.QueueTotal = d
		case "residence_lead:":
			out.LeadTotal = d
		default:
			if lane, ok := strings.CutPrefix(key, "residence_lane:"); ok {
				group := "system"
				if lane == "Blocked" || lane == "Human Review" {
					group = "held"
				}
				out.Lanes = append(out.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: group, AnalyticsDuration: d})
			}
		}
	}
	for _, cause := range causes {
		out.ReworkCauses = append(out.ReworkCauses, cause)
	}
	slices.SortFunc(out.Lanes, func(a, b operatortool.AnalyticsLaneDuration) int { return strings.Compare(a.Lane, b.Lane) })
	return out, observed, found, rows.Err()
}

func addResidenceHours(exact, hourly operatortool.AnalyticsResidenceSummary) operatortool.AnalyticsResidenceSummary {
	exact.Lanes = mergeLaneDurations(exact.Lanes, hourly.Lanes)
	exact.SystemTotal = mergeDurations(exact.SystemTotal, hourly.SystemTotal)
	exact.HeldTotal = mergeDurations(exact.HeldTotal, hourly.HeldTotal)
	exact.LeadTotal = mergeDurations(exact.LeadTotal, hourly.LeadTotal)
	exact.QueueTotal = mergeDurations(exact.QueueTotal, hourly.QueueTotal)
	exact.Partial = exact.Partial || hourly.Partial
	for _, cause := range hourly.ReworkCauses {
		index := slices.IndexFunc(exact.ReworkCauses, func(v operatortool.AnalyticsReworkCause) bool {
			return v.FromState == cause.FromState && v.ReasonDetail == cause.ReasonDetail
		})
		if index < 0 {
			exact.ReworkCauses = append(exact.ReworkCauses, cause)
		} else {
			exact.ReworkCauses[index].Count += cause.Count
		}
	}
	return exact
}

func addAnalyticsResidenceHours(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	hourly, observed, found, err := readAnalyticsResidenceHours(ctx, q, scope, out.Window.From, out.Window.To)
	if err != nil || !found {
		return err
	}
	if filter, ok := reportsScopeFrom(ctx); ok && filter.issues != "" {
		out.Partial, out.LaneResidence.Partial, out.QueueTime.Partial = true, true, true
		out.Unavailable = append(out.Unavailable, "older_hourly_issue_filter_unavailable")
		return nil
	}
	out.LaneResidence.AnalyticsResidenceSummary = addResidenceHours(out.LaneResidence.AnalyticsResidenceSummary, hourly)
	out.LaneResidence.Coverage += "; older_detail_hourly; counts_are_issue_hours; quantiles_log_histogram_16_bins_per_octave"
	out.LaneResidence.IssuesObserved = max(out.LaneResidence.IssuesObserved, observed)
	if !hourly.Partial {
		out.Unavailable = slices.DeleteFunc(out.Unavailable, func(key string) bool {
			return key == "lane_residence_and_unclaimed_waits_unavailable" || key == "queue_time"
		})
	}
	out.QueueTime.Seconds = out.LaneResidence.QueueTotal.Seconds
	out.QueueTime.Duration = out.LaneResidence.QueueTotal
	out.QueueTime.Coverage += "; older_detail_hourly"
	out.QueueTime.Partial = out.QueueTime.Partial || hourly.Partial
	out.Partial = out.Partial || hourly.Partial
	for i := range out.Digest {
		b := &out.Digest[i]
		h, _, _, err := readAnalyticsResidenceHours(ctx, q, scope, b.From, b.To)
		if err != nil {
			return err
		}
		b.LaneResidence = addResidenceHours(b.LaneResidence, h)
		b.QueueSeconds = b.LaneResidence.QueueTotal.Seconds
	}
	return nil
}

func addAnalyticsCapacityHours(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	rows, err := q.QueryContext(ctx, `SELECT hour,metric,dimension,count,total,unavailable,last_ns FROM analytics_sample_hours WHERE organization_id=? AND project_id=? AND metric IN ('capacity_claim','capacity_wait') AND hour<?`, scope.organization, scope.project, out.Window.To.Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	queue := &out.ProviderCapacityWait
	for rows.Next() {
		var hour, last int64
		var metric, dimension string
		var count, unavailable int
		var total float64
		if err := rows.Scan(&hour, &metric, &dimension, &count, &total, &unavailable, &last); err != nil {
			return err
		}
		if filter, ok := reportsScopeFrom(ctx); ok && filter.issues != "" {
			if !found {
				out.Unavailable = append(out.Unavailable, "older_hourly_capacity_filter_unavailable")
			}
			found, out.Partial = true, true
			continue
		}
		at := time.Unix(hour, 0).UTC()
		if metric == "capacity_claim" {
			if !at.Add(time.Hour).After(out.Window.From) {
				continue
			}
			found = true
			queue.ClaimsObserved += count
			queue.IntervalsUnavailable += unavailable
			queue.IntervalsObserved += count - unavailable
			queue.SourceAt = maxTime(queue.SourceAt, time.Unix(0, last).UTC())
			if at.Before(out.Window.From) || at.Add(time.Hour).After(out.Window.To) {
				out.Partial = true
			}
		} else {
			claim, err := strconv.ParseInt(dimension, 10, 64)
			if err != nil {
				return err
			}
			if claim < out.Window.From.Truncate(time.Hour).Unix() || claim >= out.Window.To.Unix() {
				continue
			}
			overlap := func(from, to time.Time) float64 {
				return max(0, minTime(to, at.Add(time.Hour)).Sub(maxTime(from, at)).Seconds()) / 3600
			}
			queue.Seconds += total * overlap(out.Window.From, out.Window.To)
			for i := range out.Digest {
				b := &out.Digest[i]
				b.ProviderCapacityWaitSeconds += total * overlap(b.From, b.To)
			}
		}
	}
	if found {
		queue.Coverage += "; older_detail_hourly"
		out.SourceAt = maxTime(out.SourceAt, queue.SourceAt)
	}
	return rows.Err()
}

func readAnalyticsSlotHours(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) error {
	rows, err := q.QueryContext(ctx, `SELECT hour,value,data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='slots' AND hour<? AND hour+3600>?
UNION ALL SELECT hour,total,'' FROM analytics_sample_hours WHERE organization_id=? AND project_id=? AND metric='slots' AND hour<? AND hour+3600>?`, scope.organization, scope.project, out.To.Unix(), out.From.Unix(), scope.organization, scope.project, out.To.Unix(), out.From.Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hour int64
		var slots float64
		var raw string
		if err := rows.Scan(&hour, &slots, &raw); err != nil {
			return err
		}
		start := time.Unix(hour, 0).UTC()
		finish := start.Add(time.Hour)
		if raw != "" {
			var lease struct {
				Acquired string  `json:"acquired_at"`
				Released *string `json:"released_at"`
				Expires  string  `json:"expires_at"`
			}
			if err := json.Unmarshal([]byte(raw), &lease); err != nil {
				return err
			}
			acquired, err := parseTimeValue(lease.Acquired)
			if err != nil {
				return err
			}
			expires, err := parseTimeValue(lease.Expires)
			if err != nil {
				return err
			}
			start, finish = maxTime(start, acquired), minTime(finish, expires)
			if lease.Released != nil {
				released, err := parseTimeValue(*lease.Released)
				if err != nil {
					return err
				}
				finish = minTime(finish, released)
			}
		}
		for i := range out.Capacity {
			b := &out.Capacity[i]
			end := minTime(b.Hour.Add(time.Hour), out.To)
			overlap := max(0, minTime(end, finish).Sub(maxTime(maxTime(b.Hour, out.From), start)).Seconds())
			if raw != "" {
				b.Slots += overlap / end.Sub(b.Hour).Seconds()
			} else {
				b.Slots += slots * overlap / end.Sub(b.Hour).Seconds()
				out.Partial = out.Partial || overlap > 0 && overlap < 3600
			}
		}
	}
	return rows.Err()
}

type diagnosticsResidenceHour struct {
	transitions int
	todo        *int
	partial     bool
}

func readDiagnosticsResidenceHours(ctx context.Context, q nativeQueryer, scope nativeScope, from, to time.Time) (map[int64]diagnosticsResidenceHour, error) {
	rows, err := q.QueryContext(ctx, `SELECT hour,metric,sum(total),sum(unavailable) FROM analytics_sample_hours WHERE organization_id=? AND project_id=? AND metric IN ('residence_coverage','diagnostics_todo') AND hour<? AND hour+3600>? GROUP BY hour,metric`, scope.organization, scope.project, to.Unix(), from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hours := map[int64]diagnosticsResidenceHour{}
	filter, filtered := reportsScopeFrom(ctx)
	for rows.Next() {
		var hour int64
		var metric string
		var total float64
		var unavailable int
		if err := rows.Scan(&hour, &metric, &total, &unavailable); err != nil {
			return nil, err
		}
		h := hours[hour]
		h.partial = h.partial || unavailable > 0
		if filtered && filter.issues != "" {
			h.partial = true
		} else if metric == "residence_coverage" {
			h.transitions = int(total)
		} else {
			h.todo = new(int(total))
		}
		hours[hour] = h
	}
	return hours, rows.Err()
}
