package reports

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func PostgresReportOpenWork(facts []reportstore.OpenFact, at time.Time, unavailable []string) (ReportsOpenWork, []string) {
	out := ReportsOpenWork{Items: []operatortool.AnalyticsAging{}}
	for _, fact := range facts {
		group := analyticsResidenceGroup(fact.State, map[string]tracker.NativeState{fact.State: {Name: fact.State, Terminal: fact.Terminal}})
		if group != "system" && group != "held" {
			if fact.State == "" {
				unavailable = append(unavailable, "current_open_work_state_unavailable")
			}
			continue
		}
		if group == "held" {
			out.Held++
		} else {
			out.Count++
		}
		if fact.EnteredAt == nil {
			unavailable = append(unavailable, "current_open_work_age_partial")
			continue
		}
		hours := at.Sub(*fact.EnteredAt).Hours()
		if hours > reportsStuckAfter.Hours() {
			out.Stuck++
		}
		item := operatortool.AnalyticsAging{WorkItemID: string(fact.WorkItemID), Lane: fact.State, EnteredAt: *fact.EnteredAt, Hours: hours}
		if fact.FirstWorkedAt != nil {
			since := at.Sub(*fact.FirstWorkedAt).Hours()
			item.SinceTodoHours = &since
		}
		out.Items = append(out.Items, item)
	}
	slices.SortFunc(out.Items, func(a, b operatortool.AnalyticsAging) int {
		switch {
		case a.Hours > b.Hours:
			return -1
		case a.Hours < b.Hours:
			return 1
		default:
			return strings.Compare(a.WorkItemID, b.WorkItemID)
		}
	})
	out.Items = out.Items[:min(len(out.Items), reportsOpenWorkRows)]
	return out, slices.Compact(slices.Sorted(slices.Values(unavailable)))
}

func SummarizePostgresHostedReport(facts []reportstore.OverviewFact, attempts []reportstore.AttemptFact, costs map[reportstore.CostKey]float64, costAvailable, attemptFactsAvailable, priorRecorded bool, projects []string, w, prior operatortool.AnalyticsWindow, compare bool) HostedReportsReport {
	unavailable := []string{"lane_residence_and_unclaimed_waits_unavailable", "landed_first_try", "failure_classification", "rework_timeline_unavailable", "stage_usage", "runner_usage"}
	if !attemptFactsAvailable {
		unavailable = append(unavailable, "attempt_outcomes_unavailable")
	}
	if !costAvailable {
		unavailable = append(unavailable, "recorded_usage")
	}
	analyticsUnavailable := append([]string{}, unavailable...)
	analyticsUnavailable = append(analyticsUnavailable, "escape_classification", "infrastructure_return_classification")
	firstProject := ReportsAllProjects
	if len(projects) == 1 {
		firstProject = projects[0]
	}
	quality := AnalyticsQuality{AnalyticsQualityCounts: AnalyticsQualityCounts{Causes: map[string]int{}}, Source: "postgres_typed_work_item_facts", Coverage: "bounded issue and workflow facts; unrecorded evidence marked unavailable", Partial: true, Unavailable: []string{"escape_classification", "infrastructure_return_classification", "rework_timeline_unavailable"}, Buckets: []analyticsQualityBucket{}, Occurrences: operatortool.ReadPage[AnalyticsQualityEscape]{Items: []AnalyticsQualityEscape{}}}
	residence := operatortool.AnalyticsResidence{AnalyticsResidenceSummary: operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}, Partial: true}, Source: "postgres_typed_work_item_facts", Coverage: "lane residence and current aging are not available in typed PostgreSQL reports", Issues: operatortool.ReadPage[operatortool.AnalyticsIssueResidence]{Items: []operatortool.AnalyticsIssueResidence{}}, Aging: operatortool.ReadPage[operatortool.AnalyticsAging]{Items: []operatortool.AnalyticsAging{}}}
	projectAnalytics := NativeAnalyticsProject{ProjectID: firstProject, Source: "postgres_typed_work_item_facts", SourceAt: w.To, Window: w, Quality: quality, LaneResidence: residence, FailureSignatures: operatortool.ReadPage[NativeFailureSignature]{Items: []NativeFailureSignature{}}, Unavailable: analyticsUnavailable}
	spans := reportsSeriesSpans(w)
	priorSpans := make([]reportsSeriesSpan, len(spans))
	for i, span := range spans {
		priorSpans[i] = reportsSeriesSpan{From: span.From.Add(-w.To.Sub(w.From)), To: span.To.Add(-w.To.Sub(w.From))}
	}
	out := HostedReportsReport{
		Pipeline:   PipelineReport{Stages: []pipelineStage{}, Partial: true, Source: "pipeline receipts are unavailable in typed PostgreSQL report reads"},
		Analytics:  projectAnalytics,
		Completion: reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}, Partial: true},
		Previous:   reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}, Partial: true},
		Series:     []reportsSeriesBucket{}, Runners: []reportsRunner{}, OpenWork: ReportsOpenWork{Items: []operatortool.AnalyticsAging{}}, Prior: reportsPrior{LaneSeconds: map[string]float64{}},
		Stages: []reportdata.Stage{}, Spend: []reportdata.Spend{}, Throughput: []reportsBucket{}, Titles: map[string]string{}, Coverage: []diagnosticsCoverage{},
		Rework: []ReportClassTotal{}, Failures: []ReportClassTotal{}, Unavailable: unavailable, PriorRecorded: priorRecorded,
	}
	if costAvailable && compare && priorRecorded {
		out.PriorCost = new(float64)
	}
	cycleByBucket := make([][]float64, len(spans))
	priorIndex := reportsSeriesIndex(priorSpans)
	currentIndex := reportsSeriesIndex(spans)
	for _, span := range spans {
		out.Series = append(out.Series, reportsSeriesBucket{From: span.From, To: span.To, LaneSeconds: map[string]float64{}, StageCost: map[string]float64{}, ReworkClasses: map[string]int{}})
		out.Throughput = append(out.Throughput, reportsBucket{From: span.From, To: span.To})
	}
	currentCycles, previousCycles := []float64{}, []float64{}
	currentWorked, currentReworked, currentLanded := 0, 0, 0
	previousWorked, previousReworked := 0, 0
	failedIssues := map[string]bool{}
	for _, fact := range facts {
		id := string(fact.WorkItemID)
		out.Titles[id] = fact.Title
		currentKey := reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: w.From, To: w.To}
		previousKey := reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: prior.From, To: prior.To}
		currentCost, previousCost := costs[currentKey], costs[previousKey]
		if costAvailable {
			out.Cost += currentCost
			if out.PriorCost != nil {
				*out.PriorCost += previousCost
			}
		}
		if fact.Worked {
			currentWorked++
		}
		if fact.Reworked {
			currentReworked++
		}
		if fact.PreviousWorked {
			previousWorked++
		}
		if fact.PreviousReworked {
			previousReworked++
		}
		if fact.ChangesLanded {
			currentLanded++
		}
		out.FailedIssues = len(failedIssues)
		for i := range out.Series {
			out.Series[i].ReworkClasses["Not classified"] += boolInt(fact.Reworked && fact.CompletedAt != nil && !fact.CompletedAt.Before(spans[i].From) && fact.CompletedAt.Before(spans[i].To))
		}
		if fact.PreviousCompletedAt != nil {
			at := *fact.PreviousCompletedAt
			if !at.Before(prior.From) && at.Before(prior.To) {
				if fact.PreviousCompletedState == "Done" {
					out.Previous.Done++
					if fact.PreviousCycleSeconds != nil {
						previousCycles = append(previousCycles, *fact.PreviousCycleSeconds)
					}
					if i := priorIndex(at); i >= 0 {
						out.Series[i].PriorDone++
					}
					if costAvailable {
						out.Previous.Cost += previousCost
					}
				} else if fact.PreviousCompletedState == "Cancelled" {
					out.Previous.Cancelled++
				}
			}
		}
		if fact.CompletedAt == nil {
			continue
		}
		at := *fact.CompletedAt
		if !at.Before(w.From) && at.Before(w.To) {
			if fact.CompletedState == "Done" {
				out.Completion.Done++
				if fact.CycleSeconds != nil {
					currentCycles = append(currentCycles, *fact.CycleSeconds)
					if i := currentIndex(at); i >= 0 {
						cycleByBucket[i] = append(cycleByBucket[i], *fact.CycleSeconds)
						out.Series[i].Done++
						out.Throughput[i].Done++
					}
				}
				if costAvailable {
					out.Completion.Cost += currentCost
				}
			} else if fact.CompletedState == "Cancelled" {
				out.Completion.Cancelled++
				out.Cancelled++
			}
		}

	}
	for _, attempt := range attempts {
		id := string(attempt.WorkItemID)
		if attempt.Status != "failed" || attempt.OutcomeAt.Before(w.From) || !attempt.OutcomeAt.Before(w.To) {
			continue
		}
		out.FailedIssues += boolInt(!failedIssues[id])
		failedIssues[id] = true
		if i := currentIndex(attempt.OutcomeAt); i >= 0 {
			out.Series[i].Failed++
			out.Throughput[i].Failed++
		}
	}
	previousFailed := 0
	for _, attempt := range attempts {
		if attempt.Status == "failed" && !attempt.OutcomeAt.Before(prior.From) && attempt.OutcomeAt.Before(prior.To) {
			previousFailed++
		}
	}
	out.Completion.Lead = reportdata.SampledDuration(currentCycles)
	out.Previous.Lead = reportdata.SampledDuration(previousCycles)
	if costAvailable {
		for _, fact := range facts {
			shipped := fact.CompletedAt != nil && !fact.CompletedAt.Before(w.From) && fact.CompletedAt.Before(w.To) && fact.CompletedState == "Done"
			out.Spend = append(out.Spend, reportdata.Spend{WorkItemID: string(fact.WorkItemID), Title: fact.Title, Cost: costs[reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: w.From, To: w.To}], Shipped: shipped, State: fact.State})
		}
	}
	out.Analytics.Quality.WorkedItems = currentWorked
	out.Analytics.Quality.ReworkedItems = currentReworked
	out.Analytics.Quality.LandedVersions = currentLanded
	if currentReworked > 0 {
		workItems := make([]string, 0, currentReworked)
		for _, fact := range facts {
			if fact.Reworked {
				workItems = append(workItems, string(fact.WorkItemID))
			}
		}
		out.Rework = []ReportClassTotal{{Class: reportNotClassified.Name, Kind: reportNotClassified.Kind, Count: currentReworked, Issues: currentReworked, WorkItems: workItems}}
	}
	if currentWorked > 0 {
		out.Analytics.Quality.ReworkPercent = new(100 * float64(currentReworked) / float64(currentWorked))
	}
	if previousWorked > 0 {
		out.Prior.ReworkPercent = new(100 * float64(previousReworked) / float64(previousWorked))
	}
	out.Analytics.Quality.Buckets = make([]analyticsQualityBucket, len(out.Series))
	for i := range out.Series {
		out.Series[i].Cycle = reportdata.SampledDuration(cycleByBucket[i])
		out.Series[i].Lead = out.Series[i].Cycle
		out.Analytics.Quality.Buckets[i] = analyticsQualityBucket{From: out.Series[i].From, To: out.Series[i].To}
		out.Analytics.Quality.Buckets[i].AnalyticsQualityCounts = AnalyticsQualityCounts{Causes: map[string]int{}}
	}
	if compare {
		out.PriorFailed = &previousFailed
	}
	out.Tokens, out.Input, out.Cached = 0, 0, 0
	observed := len(facts)
	out.Coverage = []diagnosticsCoverage{{Source: "typed_work_item_facts", Observed: &observed}}
	return out
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func completeReportCosts(keys []reportstore.CostKey, costs map[reportstore.CostKey]float64) bool {
	if len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if _, ok := costs[key]; !ok {
			return false
		}
	}
	return true
}

func MarkPostgresReportCoverage(out *HostedReportsReport, history []reportstore.ProjectHistory, attempts []reportstore.AttemptFact) {
	quality := &out.Analytics.Quality
	quality.Partial = true
	quality.Coverage = "observed typed facts; historical escape and landing completeness is unknown"
	quality.Unavailable = append(quality.Unavailable, "escape_classification")
	quality.EscapePercent = nil
	out.Analytics.Unavailable = append(out.Analytics.Unavailable, "escape_classification")
	out.FirstTry, out.PriorFirst = nil, nil
	out.Unavailable = append(out.Unavailable, "landed_first_try")
	for _, attempt := range attempts {
		if !attempt.ProjectionKnown {
			out.Unavailable = append(out.Unavailable, "attempt_outcomes_unavailable", "failure_classification")
			out.Analytics.Unavailable = append(out.Analytics.Unavailable, "attempt_outcomes_unavailable", "failure_classification")
			out.PriorFailed = nil
			break
		}
	}
	historyIncomplete, initialStateMissing := false, false
	for _, project := range history {
		for _, issue := range project.Issues {
			if issue.InitialState == "" {
				historyIncomplete = true
				initialStateMissing = true
			}
			for _, event := range issue.Events {
				historyIncomplete = historyIncomplete || event.ToState == "Rework" && event.Class.Name == ""
			}
		}
	}
	if initialStateMissing {
		out.Unavailable = append(out.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "completion_complete_population")
		out.Analytics.Unavailable = append(out.Analytics.Unavailable, "lane_residence_and_unclaimed_waits_unavailable")
	}
	if historyIncomplete {
		out.Unavailable = append(out.Unavailable, "rework_timeline_unavailable")
		out.Analytics.Unavailable = append(out.Analytics.Unavailable, "rework_timeline_unavailable", "infrastructure_return_classification")
		quality.Unavailable = append(quality.Unavailable, "rework_timeline_unavailable", "infrastructure_return_classification")
		quality.ReworkPercent = nil
		out.Prior.ReworkPercent, out.Prior.InfrastructureReturns = nil, nil
	}
}
