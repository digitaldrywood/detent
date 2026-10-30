package hubserver

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// mergeHostedReports combines the reports of several projects over one
// window into organization totals. Counts and spend add up; durations and
// percentages are computed again from the combined samples and counts.
func mergeHostedReports(reports []hostedReportsReport, w operatortool.AnalyticsWindow) hostedReportsReport {
	out := hostedReportsReport{
		Pipeline:    hubanalytics.PipelineReport{Stages: []hubanalytics.PipelineStage{}},
		Requests:    reports[0].Requests,
		Stages:      []reportsStage{},
		Spend:       []reportsSpend{},
		Titles:      map[string]string{},
		Coverage:    []diagnosticsCoverage{},
		Unavailable: mergeReportsUnavailable(reports, func(r hostedReportsReport) []string { return r.Unavailable }),
	}
	analytics := make([]hubanalytics.Project, 0, len(reports))
	completions, previous, priors := make([]hubanalytics.Completion, 0, len(reports)), make([]hubanalytics.Completion, 0, len(reports)), make([]reportsPrior, 0, len(reports))
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

func mergeReportsCompletion(items []hubanalytics.Completion) hubanalytics.Completion {
	out := hubanalytics.Completion{}
	var lanes [][]operatortool.AnalyticsLaneDuration
	var system, lead, working []operatortool.AnalyticsDuration
	for _, c := range items {
		out.Done += c.Done
		out.Cancelled += c.Cancelled
		out.Cost += c.Cost
		out.Partial = out.Partial || c.Partial
		out.PerIssue = append(out.PerIssue, c.PerIssue...)
		lanes = append(lanes, c.Lanes)
		system, lead, working = append(system, c.System), append(lead, c.Lead), append(working, c.Working)
	}
	out.Lanes = hubanalytics.MergeLaneDurations(lanes...)
	out.System, out.Lead, out.Working = hubanalytics.MergeDurations(system...), hubanalytics.MergeDurations(lead...), hubanalytics.MergeDurations(working...)
	if len(out.PerIssue) > 0 {
		spread := hubanalytics.Duration(slices.Clone(out.PerIssue))
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
		merged.Cycle, merged.Lead = hubanalytics.MergeDurations(cycle...), hubanalytics.MergeDurations(lead...)
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
		runner.Duration = hubanalytics.MergeDurations(durations[id]...)
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
		stage.Duration = hubanalytics.MergeDurations(durations[key]...)
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

func mergeReportsAnalytics(projects []hubanalytics.Project, w operatortool.AnalyticsWindow) hubanalytics.Project {
	out := hubanalytics.Project{
		ProjectID:   reportsAllProjects,
		Source:      projects[0].Source,
		Window:      w,
		Digest:      []hubanalytics.Bucket{},
		Efficiency:  []hubanalytics.Phase{},
		SkipReasons: []hubanalytics.Skip{},
		Unavailable: mergeReportsUnavailable(projects, func(p hubanalytics.Project) []string { return p.Unavailable }),
		Pipeline:    hubanalytics.PipelineReport{Stages: []hubanalytics.PipelineStage{}},
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
	signatures := []hubanalytics.FailureSignature{}
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
	residence.Lanes = hubanalytics.MergeLaneDurations(lanes...)
	residence.SystemTotal, residence.HeldTotal, residence.LeadTotal, residence.QueueTotal = hubanalytics.MergeDurations(system...), hubanalytics.MergeDurations(held...), hubanalytics.MergeDurations(lead...), hubanalytics.MergeDurations(queue...)
	residence.Aging.Items = reportsAgingTop(residence.Aging.Items, hubanalytics.MaxPopulation)
	residence.Issues.Items = []operatortool.AnalyticsIssueResidence{}
	residence.ReworkCauses = []operatortool.AnalyticsReworkCause{}
	for key, count := range causes {
		residence.ReworkCauses = append(residence.ReworkCauses, operatortool.AnalyticsReworkCause{FromState: key[0], ReasonDetail: key[1], Count: count})
	}
	slices.SortFunc(residence.ReworkCauses, func(a, b operatortool.AnalyticsReworkCause) int {
		return cmp.Or(strings.Compare(a.FromState, b.FromState), strings.Compare(a.ReasonDetail, b.ReasonDetail))
	})
	slices.SortStableFunc(signatures, func(a, b hubanalytics.FailureSignature) int { return cmp.Compare(b.Count, a.Count) })
	out.FailureSignatures.Items = signatures
	out.Landings.Items = []nativeAnalyticsLanding{}
	out.Attempts.Items = []hubanalytics.Attempt{}
	return out
}

func mergeReportsQuality(projects []hubanalytics.Project) hubanalytics.Quality {
	out := hubanalytics.Quality{Source: projects[0].Quality.Source, Coverage: projects[0].Quality.Coverage, Unavailable: mergeReportsUnavailable(projects, func(p hubanalytics.Project) []string { return p.Quality.Unavailable })}
	out.QualityCounts = mergeQualityCounts(func(yield func(hubanalytics.QualityCounts) bool) {
		for _, p := range projects {
			if !yield(p.Quality.QualityCounts) {
				return
			}
		}
	})
	for _, p := range projects {
		out.Partial = out.Partial || p.Quality.Partial
		out.Occurrences.Items = append(out.Occurrences.Items, p.Quality.Occurrences.Items...)
	}
	buckets := projects[0].Quality.Buckets
	out.Buckets = make([]hubanalytics.QualityBucket, 0, len(buckets))
	for i, bucket := range buckets {
		merged := hubanalytics.QualityBucket{From: bucket.From, To: bucket.To}
		merged.QualityCounts = mergeQualityCounts(func(yield func(hubanalytics.QualityCounts) bool) {
			for _, p := range projects {
				if i < len(p.Quality.Buckets) && !yield(p.Quality.Buckets[i].QualityCounts) {
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
		out.Occurrences.Items = []hubanalytics.QualityEscape{}
	}
	return out
}

func mergeQualityCounts(counts func(func(hubanalytics.QualityCounts) bool)) hubanalytics.QualityCounts {
	out := hubanalytics.QualityCounts{Causes: map[string]int{}}
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
	report.Previous = hubanalytics.Completion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	report.Prior = reportsPrior{LaneSeconds: map[string]float64{}}
	report.PriorCost, report.PriorFirst, report.PriorFailed, report.OpenWork.PreviousStuck = nil, nil, nil, nil
	report.PriorRecorded = false
	for i := range report.Series {
		report.Series[i].PriorDone = 0
	}
	return report
}
