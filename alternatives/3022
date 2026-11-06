package hubserver

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type postgresReportClassEvent struct {
	class reportClass
	item  string
	at    time.Time
	cost  float64
}

func postgresReportClasses(events []postgresReportClassEvent) []reportClassTotal {
	acc := newReportClassAccumulator()
	for _, event := range events {
		acc.add(event.class, event.item, event.cost)
	}
	out := acc.result()
	for i := range out {
		items := map[string]int{}
		for _, event := range events {
			if event.class.Name != out[i].Class || event.class.Kind != out[i].Kind {
				continue
			}
			items[event.item]++
			if out[i].FirstSeen == nil || event.at.Before(*out[i].FirstSeen) {
				at := event.at
				out[i].FirstSeen = &at
			}
			if out[i].LastSeen == nil || event.at.After(*out[i].LastSeen) {
				at := event.at
				out[i].LastSeen = &at
			}
		}
		out[i].WorkItems = reportClassTopItems(items)
		out[i].itemCounts = items
	}
	return out
}

func postgresReportRemoveUnavailable(values []string, remove ...string) []string {
	return slices.DeleteFunc(values, func(value string) bool { return slices.Contains(remove, value) })
}

func postgresReportCompleted(timelines []analyticsResidenceTimeline, from, to time.Time) []analyticsResidenceTimeline {
	out := []analyticsResidenceTimeline{}
	for _, timeline := range timelines {
		if timeline.state != "Done" || len(timeline.done) == 0 {
			continue
		}
		at := timeline.done[len(timeline.done)-1]
		if !at.Before(from) && at.Before(to) {
			out = append(out, timeline)
		}
	}
	return out
}

func postgresReportTiming(target *reportsCompletion, source reportsCompletion) {
	target.System, target.Lead, target.Working, target.Lanes, target.Partial = source.System, source.Lead, source.Working, source.Lanes, source.Partial
}

func applyPostgresReportHistory(out *hostedReportsReport, history []reportstore.ProjectHistory, facts []reportstore.OverviewFact, w, prior operatortool.AnalyticsWindow) {
	analytics := []nativeAnalyticsProject{}
	current, previous := []reportsCompletion{}, []reportsCompletion{}
	openReports := []hostedReportsReport{}
	reworks := []postgresReportClassEvent{}
	priorInfra := 0
	priorReworked := map[string]bool{}
	bucketCycles := make([][]operatortool.AnalyticsDuration, len(out.Series))
	bucketLeads := make([][]operatortool.AnalyticsDuration, len(out.Series))
	for i := range out.Series {
		out.Series[i].LaneSeconds = map[string]float64{}
		out.Series[i].ReworkClasses = map[string]int{}
	}
	for _, project := range history {
		states := map[string]tracker.NativeState{}
		for _, state := range project.States {
			states[state.Name] = state
		}
		issues := []analyticsResidenceIssue{}
		timelines, priorTimelines := []analyticsResidenceTimeline{}, []analyticsResidenceTimeline{}
		eventCount := 0
		partial := false
		for _, item := range project.Issues {
			issue := analyticsResidenceIssue{id: string(item.WorkItemID), created: item.CreatedAt, initial: item.InitialState}
			for _, event := range item.Events {
				issue.events = append(issue.events, analyticsResidenceEvent{at: event.At, kind: event.Type, data: tracker.CollaborationData{FromState: event.FromState, ToState: event.ToState, ReasonDetail: event.Class.Name}})
				if event.ToState == "Rework" && !event.At.Before(w.From) && event.At.Before(w.To) {
					reworks = append(reworks, postgresReportClassEvent{class: event.Class, item: issue.id, at: event.At})
				}
				if event.ToState == "Rework" && !event.At.Before(prior.From) && event.At.Before(prior.To) {
					if event.Class.Kind == reportKindInfrastructure {
						priorInfra++
					} else {
						priorReworked[issue.id] = true
					}
				}
			}
			eventCount += len(issue.events)
			issues = append(issues, issue)
			timeline := buildAnalyticsResidenceTimeline(issue, states, w.To)
			partial = partial || timeline.issue.partial
			timelines = append(timelines, timeline)
			if issue.created.Before(prior.To) {
				earlier := issue
				earlier.events = slices.DeleteFunc(slices.Clone(issue.events), func(event analyticsResidenceEvent) bool { return !event.at.Before(prior.To) })
				priorTimelines = append(priorTimelines, buildAnalyticsResidenceTimeline(earlier, states, prior.To))
			}
		}
		summary, items := summarizeAnalyticsResidence(timelines, states, w.From, w.To)
		summary.Partial = partial
		residence := operatortool.AnalyticsResidence{AnalyticsResidenceSummary: summary, Source: "postgres_typed_workflow_history", Coverage: "bounded ordered workflow transitions; residence clipped to window; aging at window end", IssuesObserved: len(issues), EventsObserved: eventCount, Issues: operatortool.ReadPage[operatortool.AnalyticsIssueResidence]{Items: items}, Aging: operatortool.ReadPage[operatortool.AnalyticsAging]{Items: []operatortool.AnalyticsAging{}}}
		var inputs reportsInputs
		reportsTimelineResidence(&inputs, timelines, issues, states, w, partial)
		open := reportsOpenWorkFor(inputs, summary.Lanes, w)
		residence.Aging.Items = open.Items
		analytics = append(analytics, nativeAnalyticsProject{ProjectID: string(project.ProjectID), Source: residence.Source, LaneResidence: residence})
		openReports = append(openReports, hostedReportsReport{OpenWork: open})
		current = append(current, reportsCompletions(postgresReportCompleted(timelines, w.From, w.To), states, w.To))
		previous = append(previous, reportsCompletions(postgresReportCompleted(priorTimelines, prior.From, prior.To), states, prior.To))
		priorSummary, _ := summarizeAnalyticsResidence(timelines, states, prior.From, prior.To)
		for lane, seconds := range reportsLanesExcept(priorSummary) {
			out.Prior.LaneSeconds[lane] += seconds
		}
		for i, bucket := range out.Series {
			summary, _ := summarizeAnalyticsResidence(timelines, states, bucket.From, bucket.To)
			for lane, seconds := range reportsLanesExcept(summary) {
				out.Series[i].LaneSeconds[lane] += seconds
			}
			timing := reportsCompletions(postgresReportCompleted(timelines, bucket.From, bucket.To), states, bucket.To)
			bucketCycles[i] = append(bucketCycles[i], timing.System)
			bucketLeads[i] = append(bucketLeads[i], timing.Lead)
		}
	}
	if len(analytics) == 1 {
		out.Analytics.LaneResidence = analytics[0].LaneResidence
	} else if len(analytics) > 1 {
		out.Analytics.LaneResidence = mergeReportsAnalytics(analytics, w).LaneResidence
	}
	out.OpenWork = mergeReportsOpenWork(openReports)
	postgresReportTiming(&out.Completion, mergeReportsCompletion(current))
	postgresReportTiming(&out.Previous, mergeReportsCompletion(previous))
	out.Rework = postgresReportClasses(reworks)
	out.Analytics.Quality.returns = nil
	issueReturns := map[string]bool{}
	out.Analytics.Quality.InfraReturns = 0
	out.Analytics.Quality.Causes = map[string]int{}
	for _, event := range reworks {
		out.Analytics.Quality.returns = append(out.Analytics.Quality.returns, qualityReturn{item: event.item, at: event.at, class: event.class})
		out.Analytics.Quality.Causes[event.class.Name]++
		if event.class.Kind == reportKindInfrastructure {
			out.Analytics.Quality.InfraReturns++
		} else {
			issueReturns[event.item] = true
		}
	}
	out.Analytics.Quality.ReworkedItems = len(issueReturns)
	if out.Analytics.Quality.WorkedItems > 0 {
		out.Analytics.Quality.ReworkPercent = new(100 * float64(len(issueReturns)) / float64(out.Analytics.Quality.WorkedItems))
	}
	out.Prior.InfrastructureReturns = &priorInfra
	priorWorked := 0
	for _, fact := range facts {
		if fact.PreviousWorked {
			priorWorked++
		}
	}
	if priorWorked > 0 {
		out.Prior.ReworkPercent = new(100 * float64(len(priorReworked)) / float64(priorWorked))
	}
	index := reportsSeriesIndex(reportsSeriesSpans(w))
	for _, event := range reworks {
		if i := index(event.at); i >= 0 {
			out.Series[i].ReworkClasses[event.class.Name]++
		}
	}
	for i := range out.Series {
		out.Series[i].Cycle = mergeDurations(bucketCycles[i]...)
		out.Series[i].Lead = mergeDurations(bucketLeads[i]...)
	}
	out.Unavailable = postgresReportRemoveUnavailable(out.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "rework_timeline_unavailable", "current_open_work_age_partial", "current_open_work_state_unavailable")
	out.Analytics.Unavailable = postgresReportRemoveUnavailable(out.Analytics.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "rework_timeline_unavailable")
	out.Analytics.Quality.Unavailable = postgresReportRemoveUnavailable(out.Analytics.Quality.Unavailable, "rework_timeline_unavailable")
}

func applyPostgresReportLandings(out *hostedReportsReport, facts []reportstore.LandingFact, w, prior operatortool.AnalyticsWindow, compare bool) {
	for _, window := range []struct {
		from, to time.Time
		current  bool
	}{{w.From, w.To, true}, {prior.From, prior.To, false}} {
		seen := map[tracker.NativeWorkItemID]bool{}
		first, total := 0, 0
		complete := true
		for _, fact := range facts {
			if fact.At.Before(window.from) || !fact.At.Before(window.to) || seen[fact.WorkItemID] {
				continue
			}
			seen[fact.WorkItemID] = true
			complete = complete && fact.Complete
			total++
			if fact.Attempts == 1 {
				first++
			}
		}
		var percent *float64
		if complete && total > 0 {
			percent = new(100 * float64(first) / float64(total))
		}
		if window.current {
			out.FirstTry = percent
			if complete {
				out.Unavailable = postgresReportRemoveUnavailable(out.Unavailable, "landed_first_try")
			}
		} else if compare {
			out.PriorFirst = percent
		}
	}
}

func applyPostgresReportFailures(out *hostedReportsReport, attempts []reportstore.AttemptFact, w, prior operatortool.AnalyticsWindow, compare bool) {
	failures := []postgresReportClassEvent{}
	issues := map[string]bool{}
	previous := 0
	index := reportsSeriesIndex(reportsSeriesSpans(w))
	for i := range out.Series {
		out.Series[i].Failed, out.Series[i].FailedInfrastructure, out.Series[i].FailedIssue = 0, 0, 0
		out.Throughput[i].Failed = 0
	}
	for _, attempt := range attempts {
		if !attempt.Failed {
			continue
		}
		if !attempt.OutcomeAt.Before(prior.From) && attempt.OutcomeAt.Before(prior.To) {
			previous++
		}
		if attempt.OutcomeAt.Before(w.From) || !attempt.OutcomeAt.Before(w.To) {
			continue
		}
		class := reportClass{Name: attempt.FailureClass, Kind: attempt.FailureKind}
		failures = append(failures, postgresReportClassEvent{class: class, item: string(attempt.WorkItemID), at: attempt.OutcomeAt})
		issues[string(attempt.WorkItemID)] = true
		if i := index(attempt.OutcomeAt); i >= 0 {
			out.Series[i].Failed++
			out.Throughput[i].Failed++
			if class.Kind == reportKindInfrastructure {
				out.Series[i].FailedInfrastructure++
			}
			if class.Kind == reportKindIssue {
				out.Series[i].FailedIssue++
			}
		}
	}
	out.Failures = postgresReportClasses(failures)
	out.FailedIssues = len(issues)
	if compare {
		out.PriorFailed = &previous
	}
	out.Unavailable = postgresReportRemoveUnavailable(out.Unavailable, "failure_classification")
	out.Analytics.Unavailable = postgresReportRemoveUnavailable(out.Analytics.Unavailable, "failure_classification")
}
