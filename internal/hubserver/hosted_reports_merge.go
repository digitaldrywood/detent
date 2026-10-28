package hubserver

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

// mergeHostedReports combines the reports of several projects over one
// window into organization totals. Counts and spend add up; durations and
// percentages are computed again from the combined samples and counts.
func mergeHostedReports(reports []hostedReportsReport, w operatortool.AnalyticsWindow) hostedReportsReport {
	out := hostedReportsReport{
		Pipeline:    pipelineReport{Stages: []pipelineStage{}},
		Requests:    reports[0].Requests,
		Stages:      []reportsStage{},
		Spend:       []reportsSpend{},
		Titles:      map[string]string{},
		Coverage:    []diagnosticsCoverage{},
		Unavailable: mergeReportsUnavailable(reports, func(r hostedReportsReport) []string { return r.Unavailable }),
	}
	analytics := make([]nativeAnalyticsProject, 0, len(reports))
	completions, previous, priors := make([]reportsCompletion, 0, len(reports)), make([]reportsCompletion, 0, len(reports)), make([]reportsPrior, 0, len(reports))
	priorFailed := true
	var failed int
	firstMissing, priorFirstMissing, priorCostMissing := false, false, false
	for _, r := range reports {
		analytics = append(analytics, r.Analytics)
		completions, previous, priors = append(completions, r.Completion), append(previous, r.Previous), append(priors, r.Prior)
		out.FailedIssues += r.FailedIssues
		out.Tokens += r.Tokens
		out.Input += r.Input
		out.Cached += r.Cached
		out.Cost += r.Cost
		out.Cancelled += r.Cancelled
		out.PriorRecorded = out.PriorRecorded || r.PriorRecorded
		out.firstTry.add(r.firstTry)
		out.priorFirst.add(r.priorFirst)
		firstMissing = firstMissing || r.firstTryMissing
		priorFirstMissing = priorFirstMissing || r.priorFirstMissing
		priorCostMissing = priorCostMissing || r.PriorRecorded && r.PriorCost == nil
		if r.PriorCost != nil {
			out.PriorCost = new(*r.PriorCost + valueOr(out.PriorCost))
		}
		if r.PriorFailed == nil {
			priorFailed = false
		} else {
			failed += *r.PriorFailed
		}
		out.Spend = append(out.Spend, r.Spend...)
		maps.Copy(out.Titles, r.Titles)
	}
	if priorFailed {
		out.PriorFailed = &failed
	}
	out.FirstTry, out.PriorFirst = out.firstTry.percent(), out.priorFirst.percent()
	if firstMissing {
		out.FirstTry, out.firstTryMissing = nil, true
		if !slices.Contains(out.Unavailable, "landed_first_try") {
			out.Unavailable = append(out.Unavailable, "landed_first_try")
		}
	}
	if priorFirstMissing {
		out.PriorFirst, out.priorFirstMissing = nil, true
	}
	if priorCostMissing {
		out.PriorCost = nil
	}
	out.Analytics = mergeReportsAnalytics(analytics, w)
	out.Completion, out.Previous = mergeReportsCompletion(completions), mergeReportsCompletion(previous)
	out.Prior = mergeReportsPrior(priors)
	out.Series = mergeReportsSeries(reports)
	out.Throughput = mergeReportsThroughput(reports)
	out.Runners = mergeReportsRunners(reports)
	out.Stages = mergeReportsStages(reports)
	out.OpenWork = mergeReportsOpenWork(reports)
	out.Rework = mergeReportClasses(reports, func(r hostedReportsReport) []reportClassTotal { return r.Rework })
	out.Failures = mergeReportClasses(reports, func(r hostedReportsReport) []reportClassTotal { return r.Failures })
	out.Coverage = mergeReportsCoverage(reports)
	slices.SortFunc(out.Spend, func(a, b reportsSpend) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), strings.Compare(a.WorkItemID, b.WorkItemID))
	})
	return out
}

func valueOr[T any](value *T) T {
	var zero T
	if value == nil {
		return zero
	}
	return *value
}

func (r *reportsRatio) add(other reportsRatio) {
	r.part += other.part
	r.total += other.total
}

func (r reportsRatio) percent() *float64 {
	if r.total == 0 {
		return nil
	}
	return new(100 * float64(r.part) / float64(r.total))
}

// mergeReportsUnavailable keeps a source unavailable when no project
// recorded it, and a population incomplete when any project's was.
func mergeReportsUnavailable[T any](items []T, keys func(T) []string) []string {
	counts := map[string]int{}
	for _, item := range items {
		seen := map[string]bool{}
		for _, key := range keys(item) {
			if !seen[key] {
				seen[key] = true
				counts[key]++
			}
		}
	}
	out := []string{}
	for key, count := range counts {
		if count == len(items) || strings.Contains(key, "complete") || strings.Contains(key, "partial") {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

func mergeDurations(durations ...operatortool.AnalyticsDuration) operatortool.AnalyticsDuration {
	var samples []float64
	for _, d := range durations {
		if len(d.Histogram) > 0 {
			return mergeHistogramDurations(durations...)
		}
		samples = append(samples, d.Samples...)
	}
	return sampledDuration(samples)
}

func mergeLaneDurations(groups ...[]operatortool.AnalyticsLaneDuration) []operatortool.AnalyticsLaneDuration {
	byLane := map[string][]operatortool.AnalyticsDuration{}
	group := map[string]string{}
	for _, lanes := range groups {
		for _, lane := range lanes {
			byLane[lane.Lane] = append(byLane[lane.Lane], lane.AnalyticsDuration)
			group[lane.Lane] = lane.Group
		}
	}
	out := []operatortool.AnalyticsLaneDuration{}
	for lane, durations := range byLane {
		out = append(out, operatortool.AnalyticsLaneDuration{Lane: lane, Group: group[lane], AnalyticsDuration: mergeDurations(durations...)})
	}
	slices.SortFunc(out, func(a, b operatortool.AnalyticsLaneDuration) int { return strings.Compare(a.Lane, b.Lane) })
	return out
}

func mergeReportsCompletion(items []reportsCompletion) reportsCompletion {
	out := reportsCompletion{}
	var lanes [][]operatortool.AnalyticsLaneDuration
	var system, lead, working []operatortool.AnalyticsDuration
	for _, c := range items {
		out.Done += c.Done
		out.Cancelled += c.Cancelled
		out.Cost += c.Cost
		out.Partial = out.Partial || c.Partial
		out.perIssue = append(out.perIssue, c.perIssue...)
		lanes = append(lanes, c.Lanes)
		system, lead, working = append(system, c.System), append(lead, c.Lead), append(working, c.Working)
	}
	out.Lanes = mergeLaneDurations(lanes...)
	out.System, out.Lead, out.Working = mergeDurations(system...), mergeDurations(lead...), mergeDurations(working...)
	if len(out.perIssue) > 0 {
		spread := analyticsDuration(slices.Clone(out.perIssue))
		out.CostP50, out.CostP90 = &spread.P50Seconds, &spread.P90Seconds
	}
	return out
}

func mergeReportsPrior(items []reportsPrior) reportsPrior {
	out := reportsPrior{LaneSeconds: map[string]float64{}}
	returns, complete := 0, true
	for _, p := range items {
		out.rework.add(p.rework)
		if p.InfrastructureReturns == nil {
			complete = false
		} else {
			returns += *p.InfrastructureReturns
		}
		for lane, seconds := range p.LaneSeconds {
			out.LaneSeconds[lane] += seconds
		}
	}
	if complete {
		out.InfrastructureReturns = &returns
	}
	if slices.ContainsFunc(items, func(p reportsPrior) bool { return p.ReworkPercent == nil && p.rework.total > 0 }) {
		return out
	}
	out.ReworkPercent = out.rework.percent()
	return out
}

func mergeReportsSeries(reports []hostedReportsReport) []reportsSeriesBucket {
	out := slices.Clone(reports[0].Series)
	for i := range out {
		buckets := make([]reportsSeriesBucket, 0, len(reports))
		for _, r := range reports {
			if i < len(r.Series) {
				buckets = append(buckets, r.Series[i])
			}
		}
		merged := reportsSeriesBucket{From: out[i].From, To: out[i].To, LaneSeconds: map[string]float64{}, StageCost: map[string]float64{}, ReworkClasses: map[string]int{}}
		var cycle, lead []operatortool.AnalyticsDuration
		for _, b := range buckets {
			merged.Done += b.Done
			merged.PriorDone += b.PriorDone
			merged.Failed += b.Failed
			merged.FailedInfrastructure += b.FailedInfrastructure
			merged.FailedIssue += b.FailedIssue
			merged.NotShippedCost += b.NotShippedCost
			cycle, lead = append(cycle, b.Cycle), append(lead, b.Lead)
			for lane, seconds := range b.LaneSeconds {
				merged.LaneSeconds[lane] += seconds
			}
			for stage, cost := range b.StageCost {
				merged.StageCost[stage] += cost
			}
			for class, count := range b.ReworkClasses {
				merged.ReworkClasses[class] += count
			}
		}
		merged.Cycle, merged.Lead = mergeDurations(cycle...), mergeDurations(lead...)
		out[i] = merged
	}
	if out == nil {
		return []reportsSeriesBucket{}
	}
	return out
}

func mergeReportsThroughput(reports []hostedReportsReport) []reportsBucket {
	out := slices.Clone(reports[0].Throughput)
	for i := range out {
		for _, r := range reports[1:] {
			if i < len(r.Throughput) {
				out[i].Done += r.Throughput[i].Done
				out[i].Failed += r.Throughput[i].Failed
			}
		}
	}
	if out == nil {
		return []reportsBucket{}
	}
	return out
}

func mergeReportsRunners(reports []hostedReportsReport) []reportsRunner {
	byID := map[string]*reportsRunner{}
	durations := map[string][]operatortool.AnalyticsDuration{}
	for _, r := range reports {
		for _, runner := range r.Runners {
			merged := byID[runner.RunnerID]
			if merged == nil {
				merged = &reportsRunner{RunnerID: runner.RunnerID, Name: runner.Name}
				byID[runner.RunnerID] = merged
			}
			merged.Sessions += runner.Sessions
			merged.Succeeded += runner.Succeeded
			merged.Cost += runner.Cost
			durations[runner.RunnerID] = append(durations[runner.RunnerID], runner.Duration)
		}
	}
	out := make([]reportsRunner, 0, len(byID))
	for id, runner := range byID {
		runner.Duration = mergeDurations(durations[id]...)
		out = append(out, *runner)
	}
	slices.SortFunc(out, func(a, b reportsRunner) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), strings.Compare(a.RunnerID, b.RunnerID))
	})
	return out
}

func mergeReportsStages(reports []hostedReportsReport) []reportsStage {
	byKey := map[[3]string]*reportsStage{}
	durations := map[[3]string][]operatortool.AnalyticsDuration{}
	disabled := map[string]bool{}
	for _, r := range reports {
		for _, stage := range r.Stages {
			if stage.Disabled {
				disabled[stage.Stage] = true
				continue
			}
			key := [3]string{stage.Stage, stage.Model, stage.Effort}
			merged := byKey[key]
			if merged == nil {
				merged = &reportsStage{Stage: stage.Stage, Model: stage.Model, Effort: stage.Effort}
				byKey[key] = merged
			}
			merged.Sessions += stage.Sessions
			merged.Succeeded += stage.Succeeded
			merged.Tokens += stage.Tokens
			merged.Input += stage.Input
			merged.Cached += stage.Cached
			merged.Cost += stage.Cost
			merged.Issues += stage.Issues
			merged.UsageObserved += stage.UsageObserved
			durations[key] = append(durations[key], stage.Duration)
		}
	}
	out := []reportsStage{}
	active := map[string]bool{}
	for key, stage := range byKey {
		stage.Duration = mergeDurations(durations[key]...)
		active[stage.Stage] = true
		out = append(out, *stage)
	}
	slices.SortFunc(out, func(a, b reportsStage) int {
		return cmp.Or(strings.Compare(a.Stage, b.Stage), strings.Compare(a.Model, b.Model), strings.Compare(a.Effort, b.Effort))
	})
	for _, stage := range slices.Sorted(maps.Keys(disabled)) {
		if !active[stage] {
			out = append(out, reportsStage{Stage: stage, Disabled: true})
		}
	}
	return out
}

func mergeReportsOpenWork(reports []hostedReportsReport) reportsOpenWork {
	out := reportsOpenWork{Items: []operatortool.AnalyticsAging{}}
	previous, complete := 0, true
	for _, r := range reports {
		out.Count += r.OpenWork.Count
		out.Held += r.OpenWork.Held
		out.Stuck += r.OpenWork.Stuck
		out.Items = append(out.Items, r.OpenWork.Items...)
		if r.OpenWork.PreviousStuck == nil {
			complete = false
		} else {
			previous += *r.OpenWork.PreviousStuck
		}
	}
	if complete {
		out.PreviousStuck = &previous
	}
	out.Items = reportsAgingTop(out.Items, reportsOpenWorkRows)
	return out
}

func reportsAgingTop(items []operatortool.AnalyticsAging, limit int) []operatortool.AnalyticsAging {
	slices.SortFunc(items, func(a, b operatortool.AnalyticsAging) int {
		return cmp.Or(cmp.Compare(b.Hours, a.Hours), strings.Compare(a.WorkItemID, b.WorkItemID))
	})
	return items[:min(len(items), limit)]
}

func mergeReportClasses(reports []hostedReportsReport, classes func(hostedReportsReport) []reportClassTotal) []reportClassTotal {
	index := map[string]int{}
	out := []reportClassTotal{}
	for _, r := range reports {
		for _, class := range classes(r) {
			i, seen := index[class.Class]
			if !seen {
				i = len(out)
				index[class.Class] = i
				out = append(out, reportClassTotal{Class: class.Class, Kind: class.Kind})
			}
			merged := &out[i]
			merged.Count += class.Count
			merged.Issues += class.Issues
			merged.Cost += class.Cost
			if merged.itemCounts == nil {
				merged.itemCounts = map[string]int{}
			}
			for item, count := range class.itemCounts {
				merged.itemCounts[item] += count
			}
			if class.FirstSeen != nil && (merged.FirstSeen == nil || class.FirstSeen.Before(*merged.FirstSeen)) {
				merged.FirstSeen = class.FirstSeen
			}
			if class.LastSeen != nil && (merged.LastSeen == nil || class.LastSeen.After(*merged.LastSeen)) {
				merged.LastSeen = class.LastSeen
			}
			if merged.TrackingWorkItemID == "" {
				merged.TrackingWorkItemID = class.TrackingWorkItemID
			}
		}
	}
	for i := range out {
		out[i].WorkItems = reportClassTopItems(out[i].itemCounts)
	}
	slices.SortStableFunc(out, func(a, b reportClassTotal) int { return cmp.Compare(b.Count, a.Count) })
	return out
}

// mergeReportsCoverage adds each source's coverage across projects; a source
// a project did not report, or reported unobserved, is unobserved overall.
func mergeReportsCoverage(reports []hostedReportsReport) []diagnosticsCoverage {
	type total struct {
		name            string
		projects        int
		observed, total int
		unobserved      bool
		untotalled      bool
	}
	index := map[string]int{}
	totals := []total{}
	for _, r := range reports {
		for _, c := range r.Coverage {
			i, seen := index[c.Source]
			if !seen {
				i = len(totals)
				index[c.Source] = i
				totals = append(totals, total{name: c.Source})
			}
			t := &totals[i]
			t.projects++
			if c.Observed == nil {
				t.unobserved = true
			} else {
				t.observed += *c.Observed
			}
			if c.Total == nil {
				t.untotalled = true
			} else {
				t.total += *c.Total
			}
		}
	}
	out := make([]diagnosticsCoverage, 0, len(totals))
	for _, t := range totals {
		c := diagnosticsCoverage{Source: t.name}
		t.unobserved = t.unobserved || t.projects < len(reports)
		if !t.unobserved {
			c.Observed = new(t.observed)
		}
		if !t.untotalled && !t.unobserved {
			c.Total = new(t.total)
		}
		out = append(out, c)
	}
	return out
}

func mergeReportsAnalytics(projects []nativeAnalyticsProject, w operatortool.AnalyticsWindow) nativeAnalyticsProject {
	out := nativeAnalyticsProject{
		ProjectID:   reportsAllProjects,
		Source:      projects[0].Source,
		Window:      w,
		Digest:      []nativeAnalyticsBucket{},
		Efficiency:  []nativeAnalyticsPhase{},
		SkipReasons: []nativeAnalyticsSkip{},
		Unavailable: mergeReportsUnavailable(projects, func(p nativeAnalyticsProject) []string { return p.Unavailable }),
		Pipeline:    pipelineReport{Stages: []pipelineStage{}},
		Quality:     mergeReportsQuality(projects),
		LaneResidence: operatortool.AnalyticsResidence{
			Source:   projects[0].LaneResidence.Source,
			Coverage: projects[0].LaneResidence.Coverage,
		},
	}
	residence := &out.LaneResidence
	var lanes [][]operatortool.AnalyticsLaneDuration
	var system, held, lead, queue []operatortool.AnalyticsDuration
	causes := map[[2]string]int{}
	signatures := []nativeFailureSignature{}
	for _, p := range projects {
		out.Partial = out.Partial || p.Partial
		r := p.LaneResidence
		residence.Partial = residence.Partial || r.Partial
		residence.IssuesObserved += r.IssuesObserved
		residence.EventsObserved += r.EventsObserved
		residence.Aging.Items = append(residence.Aging.Items, r.Aging.Items...)
		lanes = append(lanes, r.Lanes)
		system, held, lead, queue = append(system, r.SystemTotal), append(held, r.HeldTotal), append(lead, r.LeadTotal), append(queue, r.QueueTotal)
		for _, cause := range r.ReworkCauses {
			causes[[2]string{cause.FromState, cause.ReasonDetail}] += cause.Count
		}
		signatures = append(signatures, p.FailureSignatures.Items...)
	}
	residence.Lanes = mergeLaneDurations(lanes...)
	residence.SystemTotal, residence.HeldTotal, residence.LeadTotal, residence.QueueTotal = mergeDurations(system...), mergeDurations(held...), mergeDurations(lead...), mergeDurations(queue...)
	residence.Aging.Items = reportsAgingTop(residence.Aging.Items, maxAnalyticsPopulation)
	residence.Issues.Items = []operatortool.AnalyticsIssueResidence{}
	residence.ReworkCauses = []operatortool.AnalyticsReworkCause{}
	for key, count := range causes {
		residence.ReworkCauses = append(residence.ReworkCauses, operatortool.AnalyticsReworkCause{FromState: key[0], ReasonDetail: key[1], Count: count})
	}
	slices.SortFunc(residence.ReworkCauses, func(a, b operatortool.AnalyticsReworkCause) int {
		return cmp.Or(strings.Compare(a.FromState, b.FromState), strings.Compare(a.ReasonDetail, b.ReasonDetail))
	})
	slices.SortStableFunc(signatures, func(a, b nativeFailureSignature) int { return cmp.Compare(b.Count, a.Count) })
	out.FailureSignatures.Items = signatures
	out.Landings.Items = []nativeAnalyticsLanding{}
	out.Attempts.Items = []nativeAnalyticsAttempt{}
	return out
}

func mergeReportsQuality(projects []nativeAnalyticsProject) analyticsQuality {
	out := analyticsQuality{Source: projects[0].Quality.Source, Coverage: projects[0].Quality.Coverage, Unavailable: mergeReportsUnavailable(projects, func(p nativeAnalyticsProject) []string { return p.Quality.Unavailable })}
	out.analyticsQualityCounts = mergeQualityCounts(func(yield func(analyticsQualityCounts) bool) {
		for _, p := range projects {
			if !yield(p.Quality.analyticsQualityCounts) {
				return
			}
		}
	})
	for _, p := range projects {
		out.Partial = out.Partial || p.Quality.Partial
		out.Occurrences.Items = append(out.Occurrences.Items, p.Quality.Occurrences.Items...)
	}
	buckets := projects[0].Quality.Buckets
	out.Buckets = make([]analyticsQualityBucket, 0, len(buckets))
	for i, bucket := range buckets {
		merged := analyticsQualityBucket{From: bucket.From, To: bucket.To}
		merged.analyticsQualityCounts = mergeQualityCounts(func(yield func(analyticsQualityCounts) bool) {
			for _, p := range projects {
				if i < len(p.Quality.Buckets) && !yield(p.Quality.Buckets[i].analyticsQualityCounts) {
					return
				}
			}
		})
		out.Buckets = append(out.Buckets, merged)
	}
	if out.Partial {
		out.ReworkPercent, out.EscapePercent = nil, nil
		for i := range out.Buckets {
			out.Buckets[i].ReworkPercent, out.Buckets[i].EscapePercent = nil, nil
		}
	}
	if out.Occurrences.Items == nil {
		out.Occurrences.Items = []analyticsQualityEscape{}
	}
	return out
}

func mergeQualityCounts(counts func(func(analyticsQualityCounts) bool)) analyticsQualityCounts {
	out := analyticsQualityCounts{Causes: map[string]int{}}
	for c := range counts {
		out.WorkedItems += c.WorkedItems
		out.ReworkedItems += c.ReworkedItems
		out.LandedVersions += c.LandedVersions
		out.EscapedVersions += c.EscapedVersions
		out.Escapes += c.Escapes
		out.Infrastructure += c.Infrastructure
		out.InfraReturns += c.InfraReturns
		out.Pending += c.Pending
		for cause, count := range c.Causes {
			out.Causes[cause] += count
		}
	}
	out.ReworkPercent = reportsRatio{out.ReworkedItems, out.WorkedItems}.percent()
	out.EscapePercent = reportsRatio{out.EscapedVersions, out.LandedVersions}.percent()
	return out
}

// withoutPriorPeriod drops the window before the report's, for a read that
// asked for no comparison.
func withoutPriorPeriod(report hostedReportsReport) hostedReportsReport {
	report.Previous = reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	report.Prior = reportsPrior{LaneSeconds: map[string]float64{}}
	report.PriorCost, report.PriorFirst, report.PriorFailed, report.OpenWork.PreviousStuck = nil, nil, nil, nil
	report.PriorRecorded = false
	for i := range report.Series {
		report.Series[i].PriorDone = 0
	}
	return report
}

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

type analyticsCapacitySample struct {
	ID        string
	At        time.Time
	AtNS      int64
	StartNS   int64
	Start     time.Time
	SecondsNS int64
	Valid     bool
}

type analyticsSkipSample struct {
	Source, Reason string
	At             time.Time
	AtNS           int64
}
