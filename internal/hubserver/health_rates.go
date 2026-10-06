package hubserver

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Seven days follow the project's workload; twenty samples keep sparse history quiet.
// The 1h/6h windows reject brief rate spikes, with p90 allowing usual busy periods.
// Five matching failures and three recoveries indicate repetition; the 0.3 retry
// ratio and 20%/10% conflict floors bound sustained wasted effort. A 3x lane p90
// tolerates slow work, clamped to 2h/12h to avoid both noise and indefinite stalls.
// Five times median spend plus $25 isolates material outliers; quarter-throughput
// detects a large drop against the same weekday hours rather than a quiet shift.
const (
	healthBaselineWindow     = 7 * 24 * time.Hour
	healthShortWindow        = time.Hour
	healthLongWindow         = 6 * time.Hour
	healthBaselineSamples    = 20
	healthBaselineQuantile   = 0.9
	healthMedianQuantile     = 0.5
	healthRetryCount         = 5
	healthRetryRatio         = 0.3
	healthRecoveryCount      = 3
	healthRecoveryWindow     = 30 * time.Minute
	healthStallMultiplier    = 3
	healthStallMinimum       = 2 * time.Hour
	healthStallMaximum       = 12 * time.Hour
	healthConflictShort      = 0.2
	healthConflictLong       = 0.1
	healthLandingMinimum     = 5
	healthCostMultiplier     = 5
	healthCostMinimum        = 25
	healthThroughputFraction = 0.25
)

type healthRateAttempt struct {
	ID, Item, Status, Signature, Recovery, LandingKey string
	At, Heartbeat                                     time.Time
	Cost                                              float64
	Landing, Conflict, Landed                         bool
	LandingAt                                         time.Time
}

type healthItemCost struct {
	Cost  float64
	Known bool
}

type healthRateProject struct {
	ID               string
	Attempts         []healthRateAttempt
	Timelines        []analyticsResidenceTimeline
	Costs            map[string]healthItemCost
	BaselineCosts    map[string]healthItemCost
	Runners          []healthRunner
	Candidates       int
	ResidencePartial bool
}

type healthQueueEvidence struct {
	QueueDepth  int     `json:"queue_depth"`
	ShortP50    float64 `json:"todo_p50_seconds_1h"`
	LongP50     float64 `json:"todo_p50_seconds_6h"`
	BaselineP90 float64 `json:"baseline_p90_seconds"`
}

type healthBaselineUnavailable struct {
	Project string `json:"project_id"`
	Signal  string `json:"signal"`
	Lane    string `json:"lane,omitempty"`
}

type healthRateBaseline struct {
	RetryCounts, RetryShort, RetryLong, Recoveries []float64
	ConflictShort, ConflictLong                    []float64
	Lanes                                          map[string][]float64
	Costs                                          []float64
	ThroughputSamples                              int
}

func healthInWindow(at, from, to time.Time) bool {
	return !at.Before(from) && at.Before(to)
}

func healthQuantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := q * float64(len(sorted)-1)
	lower := int(index)
	return sorted[lower] + (sorted[min(lower+1, len(sorted)-1)]-sorted[lower])*(index-float64(lower))
}

func healthRecoveryReason(text string) string {
	_, reason, found := strings.Cut(text, "native checkpoint requires recovery: ")
	if !found {
		return ""
	}
	return normalizeFailureSignature(reason)
}

func healthAttemptBuckets(attempts []healthRateAttempt, from, to time.Time, width time.Duration) (counts, ratios, recoveries []float64) {
	type bucket struct {
		failures, successes, recovery int
		signatures                    map[string]int
	}
	buckets := map[string]*bucket{}
	for _, a := range attempts {
		if !healthInWindow(a.At, from, to) || a.Status == "running" || a.Status == "cancelled" {
			continue
		}
		key := a.Item + "\x00" + a.At.Truncate(width).Format(time.RFC3339)
		b := buckets[key]
		if b == nil {
			b = &bucket{signatures: map[string]int{}}
			buckets[key] = b
		}
		if a.Recovery != "" {
			b.recovery++
		} else if a.Signature != "" {
			b.failures++
			b.signatures[a.Signature]++
		} else if a.Status == "succeeded" {
			b.successes++
		}
	}
	for _, b := range buckets {
		count := 0
		for _, n := range b.signatures {
			count = max(count, n)
		}
		counts = append(counts, float64(count))
		ratios = append(ratios, float64(b.failures)/float64(max(1, b.successes)))
		recoveries = append(recoveries, float64(b.recovery))
	}
	return counts, ratios, recoveries
}

func healthConflictBuckets(attempts []healthRateAttempt, from, to time.Time, width time.Duration) []float64 {
	type bucket struct{ total, conflicts int }
	buckets := map[time.Time]bucket{}
	for _, a := range attempts {
		if !a.Landing || !healthInWindow(a.LandingAt, from, to) {
			continue
		}
		key := a.LandingAt.Truncate(width)
		b := buckets[key]
		b.total++
		if a.Conflict {
			b.conflicts++
		}
		buckets[key] = b
	}
	values := []float64{}
	for _, b := range buckets {
		values = append(values, float64(b.conflicts)/float64(b.total))
	}
	return values
}

func buildHealthRateBaseline(now time.Time, p healthRateProject) healthRateBaseline {
	to := now.Add(-healthLongWindow)
	from := to.Add(-healthBaselineWindow)
	b := healthRateBaseline{Lanes: map[string][]float64{}}
	b.RetryCounts, b.RetryShort, _ = healthAttemptBuckets(p.Attempts, from, to, healthShortWindow)
	_, b.RetryLong, _ = healthAttemptBuckets(p.Attempts, from, to, healthLongWindow)
	_, _, b.Recoveries = healthAttemptBuckets(p.Attempts, from, to, healthRecoveryWindow)
	b.ConflictShort = healthConflictBuckets(p.Attempts, from, to, healthShortWindow)
	b.ConflictLong = healthConflictBuckets(p.Attempts, from, to, healthLongWindow)
	for _, a := range p.Attempts {
		if a.Landed && healthInWindow(a.LandingAt, now.Add(-healthBaselineWindow-healthLongWindow), now.Add(-healthBaselineWindow)) {
			b.ThroughputSamples++
		}
	}
	if p.ResidencePartial {
		return b
	}
	for _, t := range p.Timelines {
		if t.issue.partial {
			continue
		}
		for _, span := range t.spans {
			if span.to.Before(to) && !span.to.Before(from) && span.to.After(span.from) {
				b.Lanes[span.lane] = append(b.Lanes[span.lane], span.to.Sub(span.from).Seconds())
			}
		}
		for _, done := range t.done {
			if healthInWindow(done, from, to) {
				if cost := p.BaselineCosts[t.issue.id]; cost.Known {
					b.Costs = append(b.Costs, cost.Cost)
				}
				break
			}
		}
	}
	return b
}

func healthBaselineGaps(p healthRateProject, b healthRateBaseline) []healthBaselineUnavailable {
	out := []healthBaselineUnavailable{}
	add := func(signal, lane string, available bool) {
		if !available {
			out = append(out, healthBaselineUnavailable{p.ID, signal, lane})
		}
	}
	add("retry_storm", "", len(b.RetryCounts) >= healthBaselineSamples && len(b.RetryLong) >= healthBaselineSamples)
	add("recovery_loop", "", len(b.Recoveries) >= healthBaselineSamples)
	for _, lane := range []string{"In Progress", "Rework", "Merging"} {
		add("stalled_active_item", lane, len(b.Lanes[lane]) >= healthBaselineSamples)
	}
	add("conflict_burn", "", len(b.ConflictShort) >= healthBaselineSamples && len(b.ConflictLong) >= healthBaselineSamples)
	add("capacity_starvation", "Todo", len(b.Lanes["Todo"]) >= healthBaselineSamples)
	add("cost_anomaly", "", len(b.Costs) >= healthBaselineSamples)
	add("throughput_collapse", "", b.ThroughputSamples >= healthBaselineSamples)
	return out
}

func evaluateHealthRates(now time.Time, projects []healthRateProject) []healthFinding {
	out := []healthFinding{}
	capacity := map[string]int{}
	for _, p := range projects {
		b := buildHealthRateBaseline(now, p)
		out = append(out, healthRetryFindings(now, p, b)...)
		out = append(out, healthRecoveryFindings(now, p, b)...)
		out = append(out, healthStallFindings(now, p, b)...)
		out = append(out, healthCostFindings(p, b)...)
		if f, active := healthConflictFinding(now, p, b); active {
			out = append(out, f)
		}
		for _, f := range healthCapacityFindings(now, p, b) {
			if index, ok := capacity[f.Fingerprint]; ok {
				existing := &out[index]
				existing.Projects = append(existing.Projects, p.ID)
				existing.Evidence.Queues[p.ID] = f.Evidence.Queues[p.ID]
				existing.Evidence.Counts["queue_depth"] += f.Evidence.Counts["queue_depth"]
			} else {
				capacity[f.Fingerprint] = len(out)
				out = append(out, f)
			}
		}
		if f, active := healthThroughputFinding(now, p, b); active {
			out = append(out, f)
		}
	}
	return out
}

func healthRetryFindings(now time.Time, p healthRateProject, b healthRateBaseline) []healthFinding {
	if len(b.RetryCounts) < healthBaselineSamples || len(b.RetryLong) < healthBaselineSamples {
		return nil
	}
	byItem := map[string][]healthRateAttempt{}
	for _, a := range p.Attempts {
		if healthInWindow(a.At, now.Add(-healthLongWindow), now) {
			byItem[a.Item] = append(byItem[a.Item], a)
		}
	}
	out := []healthFinding{}
	for item, attempts := range byItem {
		failures, successes := [2]int{}, [2]int{}
		signatures := map[string]int{}
		evidence := healthEvidence{Values: map[string]float64{}}
		for _, a := range attempts {
			if a.Recovery != "" {
				continue
			}
			if a.Signature != "" {
				failures[1]++
				evidence.AttemptIDs = append(evidence.AttemptIDs, a.ID)
				evidence.Values["wasted_cost_usd"] += a.Cost
				evidence.Signatures = append(evidence.Signatures, a.Signature)
				if healthInWindow(a.At, now.Add(-healthShortWindow), now) {
					failures[0]++
					signatures[a.Signature]++
				}
			} else if a.Status == "succeeded" {
				successes[1]++
				if healthInWindow(a.At, now.Add(-healthShortWindow), now) {
					successes[0]++
				}
			}
		}
		repeated := false
		for _, n := range signatures {
			repeated = repeated || n >= healthRetryCount && float64(n) > healthQuantile(b.RetryCounts, healthBaselineQuantile)
		}
		short, long := float64(failures[0])/float64(max(1, successes[0])), float64(failures[1])/float64(max(1, successes[1]))
		if !repeated && !(successes[0] > 0 && successes[1] > 0 && short > max(healthRetryRatio, healthQuantile(b.RetryShort, healthBaselineQuantile)) && long > max(healthRetryRatio, healthQuantile(b.RetryLong, healthBaselineQuantile))) {
			continue
		}
		slices.Sort(evidence.Signatures)
		evidence.Signatures = slices.Compact(evidence.Signatures)
		evidence.Values["retry_ratio_1h"], evidence.Values["retry_ratio_6h"] = short, long
		evidence.Counts = map[string]int{"failures_1h": failures[0], "failures_6h": failures[1]}
		out = append(out, newHealthFinding("retry_storm", "instance", "work_item", item, "Repeated failed attempts are wasting work on this item.", "The runner operator inspects the repeated failure signatures and attempts.", []string{p.ID}, evidence))
	}
	return out
}

func healthRecoveryFindings(now time.Time, p healthRateProject, b healthRateBaseline) []healthFinding {
	if len(b.Recoveries) < healthBaselineSamples {
		return nil
	}
	groups := map[string][]healthRateAttempt{}
	for _, a := range p.Attempts {
		if a.Recovery != "" && healthInWindow(a.At, now.Add(-healthRecoveryWindow), now) {
			groups[a.Item+"\x00"+a.Recovery] = append(groups[a.Item+"\x00"+a.Recovery], a)
		}
	}
	out := []healthFinding{}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		attempts := groups[key]
		if len(attempts) < healthRecoveryCount || float64(len(attempts)) <= healthQuantile(b.Recoveries, healthBaselineQuantile) {
			continue
		}
		evidence := healthEvidence{Reason: attempts[0].Recovery, Counts: map[string]int{"recoveries_30m": len(attempts)}}
		for _, a := range attempts {
			evidence.AttemptIDs = append(evidence.AttemptIDs, a.ID)
		}
		out = append(out, newHealthFinding("recovery_loop", "instance", "work_item", attempts[0].Item, "The same checkpoint recovery reason is recurring.", "The runner operator inspects the retained checkpoint and recovery reason.", []string{p.ID}, evidence))
	}
	return out
}

func healthStallFindings(now time.Time, p healthRateProject, b healthRateBaseline) []healthFinding {
	if p.ResidencePartial {
		return nil
	}
	out := []healthFinding{}
	for _, t := range p.Timelines {
		if t.issue.partial || !slices.Contains([]string{"In Progress", "Rework", "Merging"}, t.state) || len(b.Lanes[t.state]) < healthBaselineSamples {
			continue
		}
		last := t.entered
		attempt := ""
		for _, a := range p.Attempts {
			if a.Item == t.issue.id && !a.Heartbeat.After(now) && a.Heartbeat.After(last) {
				last, attempt = a.Heartbeat, a.ID
			}
		}
		threshold := min(healthStallMaximum.Seconds(), max(healthStallMinimum.Seconds(), healthStallMultiplier*healthQuantile(b.Lanes[t.state], healthBaselineQuantile)))
		if last.IsZero() || now.Sub(last).Seconds() <= threshold {
			continue
		}
		evidence := healthEvidence{Values: map[string]float64{"heartbeat_gap_seconds": now.Sub(last).Seconds(), "threshold_seconds": threshold}}
		if attempt != "" {
			evidence.AttemptIDs = []string{attempt}
		}
		out = append(out, newHealthFinding("stalled_active_item", "flow", "work_item", t.issue.id, fmt.Sprintf("The active item has no recent attempt heartbeat in %s.", t.state), "The project operator inspects the active attempt and lane residence.", []string{p.ID}, evidence))
	}
	return out
}

func healthConflictFinding(now time.Time, p healthRateProject, b healthRateBaseline) (healthFinding, bool) {
	total, conflicts := [2]int{}, [2]int{}
	evidence := healthEvidence{Values: map[string]float64{}}
	for _, a := range p.Attempts {
		if !a.Landing || !healthInWindow(a.LandingAt, now.Add(-healthLongWindow), now) {
			continue
		}
		total[1]++
		if a.Conflict {
			conflicts[1]++
			evidence.AttemptIDs = append(evidence.AttemptIDs, a.ID)
		}
		if healthInWindow(a.LandingAt, now.Add(-healthShortWindow), now) {
			total[0]++
			if a.Conflict {
				conflicts[0]++
			}
		}
	}
	short, long := float64(conflicts[0])/float64(max(1, total[0])), float64(conflicts[1])/float64(max(1, total[1]))
	evidence.Values["conflict_ratio_1h"], evidence.Values["conflict_ratio_6h"] = short, long
	evidence.Counts = map[string]int{"landings_1h": total[0], "landings_6h": total[1], "conflicts_1h": conflicts[0], "conflicts_6h": conflicts[1]}
	f := newHealthFinding("conflict_burn", "flow", "project", p.ID, "Landing conflicts exceed the project's usual rate in both windows.", "The project operator inspects conflicting changes and landing bases.", []string{p.ID}, evidence)
	f.Severity = "watch"
	return f, len(b.ConflictShort) >= healthBaselineSamples && len(b.ConflictLong) >= healthBaselineSamples && total[0] >= healthLandingMinimum && total[1] >= healthLandingMinimum && short > max(healthConflictShort, healthQuantile(b.ConflictShort, healthBaselineQuantile)) && long > max(healthConflictLong, healthQuantile(b.ConflictLong, healthBaselineQuantile))
}

func healthCapacityFindings(now time.Time, p healthRateProject, b healthRateBaseline) []healthFinding {
	if len(b.Lanes["Todo"]) < healthBaselineSamples || p.ResidencePartial || p.Candidates == 0 {
		return nil
	}
	waits := [2][]float64{}
	depth := 0
	for _, t := range p.Timelines {
		if t.issue.partial {
			continue
		}
		if t.state == "Todo" {
			depth++
		}
		for _, span := range t.spans {
			if span.lane != "Todo" {
				continue
			}
			for i, width := range []time.Duration{healthShortWindow, healthLongWindow} {
				if span.to.After(now.Add(-width)) && !span.from.After(now) {
					waits[i] = append(waits[i], minTime(span.to, now).Sub(span.from).Seconds())
				}
			}
		}
	}
	short, long, p90 := healthQuantile(waits[0], healthMedianQuantile), healthQuantile(waits[1], healthMedianQuantile), healthQuantile(b.Lanes["Todo"], healthBaselineQuantile)
	if short <= p90 || long <= p90 {
		return nil
	}
	out := []healthFinding{}
	for _, r := range p.Runners {
		if r.FreeSlots != 0 || r.Leases == 0 || now.Sub(r.Heartbeat) >= healthInstanceWindow {
			continue
		}
		f := newHealthFinding("capacity_starvation", "capacity", "runner", r.ID, "Todo wait exceeds the project's usual duration while the runner is full.", "The runner operator reviews slot occupancy and queued work.", []string{p.ID}, healthEvidence{Counts: map[string]int{"queue_depth": depth, "leases": r.Leases}, Queues: map[string]healthQueueEvidence{p.ID: {QueueDepth: depth, ShortP50: short, LongP50: long, BaselineP90: p90}}})
		f.Severity = "watch"
		out = append(out, f)
	}
	return out
}

func healthCostFindings(p healthRateProject, b healthRateBaseline) []healthFinding {
	if len(b.Costs) < healthBaselineSamples {
		return nil
	}
	median := healthQuantile(b.Costs, healthMedianQuantile)
	out := []healthFinding{}
	for _, t := range p.Timelines {
		cost := p.Costs[t.issue.id]
		if !cost.Known || cost.Cost <= healthCostMinimum || cost.Cost <= healthCostMultiplier*median {
			continue
		}
		f := newHealthFinding("cost_anomaly", "cost", "work_item", t.issue.id, "This item's spend exceeds five times the usual Done-item cost.", "The project operator reviews this item's usage and repeated attempts.", []string{p.ID}, healthEvidence{Values: map[string]float64{"cost_usd": cost.Cost, "baseline_median_cost_usd": median}})
		f.Severity = "watch"
		out = append(out, f)
	}
	return out
}

func healthThroughputFinding(now time.Time, p healthRateProject, b healthRateBaseline) (healthFinding, bool) {
	short, long, priorShort, priorLong := 0, 0, 0, 0
	for _, a := range p.Attempts {
		if !a.Landed {
			continue
		}
		if healthInWindow(a.LandingAt, now.Add(-healthLongWindow), now) {
			long++
		}
		if healthInWindow(a.LandingAt, now.Add(-healthShortWindow), now) {
			short++
		}
		if healthInWindow(a.LandingAt, now.Add(-healthBaselineWindow-healthLongWindow), now.Add(-healthBaselineWindow)) {
			priorLong++
		}
		if healthInWindow(a.LandingAt, now.Add(-healthBaselineWindow-healthShortWindow), now.Add(-healthBaselineWindow)) {
			priorShort++
		}
	}
	f := newHealthFinding("throughput_collapse", "flow", "project", p.ID, "Landings fell below a quarter of the same weekday-hour windows last week.", "The project operator inspects ready work and landing progress.", []string{p.ID}, healthEvidence{Counts: map[string]int{"landings_1h": short, "landings_6h": long, "last_week_1h": priorShort, "last_week_6h": priorLong, "ready_work": p.Candidates}})
	f.Severity = "watch"
	return f, b.ThroughputSamples >= healthBaselineSamples && p.Candidates > 0 && priorShort > 0 && float64(short) < healthThroughputFraction*float64(priorShort) && float64(long) < healthThroughputFraction*float64(priorLong)
}
