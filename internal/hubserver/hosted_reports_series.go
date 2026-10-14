package hubserver

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const reportsOtherStage = "other"

type reportsSeriesBucket struct {
	From                 time.Time                      `json:"from"`
	To                   time.Time                      `json:"to"`
	Done                 int                            `json:"done"`
	PriorDone            int                            `json:"prior_done"`
	Failed               int                            `json:"failed"`
	FailedInfrastructure int                            `json:"failed_infrastructure"`
	FailedIssue          int                            `json:"failed_issue"`
	Cycle                operatortool.AnalyticsDuration `json:"cycle"`
	Lead                 operatortool.AnalyticsDuration `json:"lead"`
	LaneSeconds          map[string]float64             `json:"lane_seconds"`
	StageCost            map[string]float64             `json:"stage_cost"`
	NotShippedCost       float64                        `json:"not_shipped_cost"`
	ReworkClasses        map[string]int                 `json:"rework_classes"`
}

// reportsWindowStart aligns a window of whole days to UTC midnight, so its
// last day is today so far; shorter windows keep their rolling start.
func reportsWindowStart(to time.Time, hours int) time.Time {
	if hours < 7*24 {
		return to.Add(-time.Duration(hours) * time.Hour)
	}
	days := time.Duration(hours/24) * 24 * time.Hour
	midnight := to.Truncate(24 * time.Hour)
	if midnight.Equal(to) {
		return to.Add(-days)
	}
	return midnight.Add(24*time.Hour - days)
}

type reportsSeriesSpan struct{ from, to time.Time }

// reportsSeriesSpans returns the display buckets: UTC calendar days for a
// window of seven days or more, clipped to the window, otherwise hours.
func reportsSeriesSpans(w operatortool.AnalyticsWindow) []reportsSeriesSpan {
	step, start := time.Hour, w.From
	if w.To.Sub(w.From) >= 6*24*time.Hour {
		step, start = 24*time.Hour, w.From.Truncate(24*time.Hour)
	}
	spans := []reportsSeriesSpan{}
	for from := start; from.Before(w.To); from = from.Add(step) {
		spans = append(spans, reportsSeriesSpan{maxTime(from, w.From), minTime(from.Add(step), w.To)})
	}
	return spans
}

func reportsSeriesIndex(spans []reportsSeriesSpan) func(time.Time) int {
	return func(at time.Time) int {
		i, found := slices.BinarySearchFunc(spans, at, func(span reportsSeriesSpan, at time.Time) int {
			switch {
			case at.Before(span.from):
				return 1
			case !at.Before(span.to):
				return -1
			default:
				return 0
			}
		})
		if !found {
			return -1
		}
		return i
	}
}

func reportsLastDone(timeline analyticsResidenceTimeline, from, to time.Time) (time.Time, bool) {
	var last time.Time
	found := false
	for _, done := range timeline.done {
		if !done.Before(from) && done.Before(to) && (!found || done.After(last)) {
			last, found = done, true
		}
	}
	return last, found
}

func reportsShipped(timelines []analyticsResidenceTimeline, from, to time.Time) map[string]bool {
	shipped := map[string]bool{}
	for _, timeline := range timelines {
		if _, ok := reportsLastDone(timeline, from, to); ok {
			shipped[timeline.issue.id] = true
		}
	}
	return shipped
}

// reportsSeries counts each shipped issue once, on its last Done in the window,
// and splits every usage row in the window by stage, so the buckets sum to
// the Shipped and Total spend headlines.
func reportsSeries(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, attempts []nativeAnalyticsAttempt, usage map[string][]usageRow, shipped map[string]bool, w operatortool.AnalyticsWindow) []reportsSeriesBucket {
	spans := reportsSeriesSpans(w)
	index := reportsSeriesIndex(spans)
	span := w.To.Sub(w.From)
	shift := make([]reportsSeriesSpan, len(spans))
	for i, s := range spans {
		shift[i] = reportsSeriesSpan{s.from.Add(-span), s.to.Add(-span)}
	}
	priorIndex := reportsSeriesIndex(shift)
	finished := make([][]analyticsResidenceTimeline, len(spans))
	prior := make([]int, len(spans))
	for _, timeline := range timelines {
		if done, ok := reportsLastDone(timeline, w.From, w.To); ok {
			if i := index(done); i >= 0 {
				finished[i] = append(finished[i], timeline)
			}
		}
		if done, ok := reportsLastDone(timeline, w.From.Add(-span), w.From); ok {
			if i := priorIndex(done); i >= 0 {
				prior[i]++
			}
		}
	}
	out := make([]reportsSeriesBucket, 0, len(spans))
	for i, s := range spans {
		completion := reportsCompletions(finished[i], states, s.from, s.to)
		residence, _ := summarizeAnalyticsResidence(timelines, states, s.from, s.to)
		bucket := reportsSeriesBucket{
			From: s.from, To: s.to,
			Done:          len(finished[i]),
			PriorDone:     prior[i],
			Cycle:         completion.System,
			Lead:          completion.Lead,
			LaneSeconds:   map[string]float64{},
			StageCost:     map[string]float64{},
			ReworkClasses: map[string]int{},
		}
		for _, lane := range residence.Lanes {
			if lane.Group != "excluded" {
				bucket.LaneSeconds[lane.Lane] = lane.Seconds
			}
		}
		out = append(out, bucket)
	}
	byAttempt := map[string]nativeAnalyticsAttempt{}
	for _, a := range attempts {
		byAttempt[a.AttemptID] = a
	}
	for attemptID, rows := range usage {
		a, known := byAttempt[attemptID]
		stage := reportsOtherStage
		if known && a.Identity.Role != "" {
			stage = a.Identity.Role
		}
		for _, u := range rows {
			i := index(u.Period)
			if i < 0 {
				continue
			}
			if known && !shipped[a.WorkItemID] {
				out[i].NotShippedCost += u.Cost
			} else {
				out[i].StageCost[stage] += u.Cost
			}
		}
	}
	return out
}

func reportsSeriesClasses(series []reportsSeriesBucket, returns []qualityReturn, failures []nativeFailure, w operatortool.AnalyticsWindow) {
	index := reportsSeriesIndex(reportsSeriesSpans(w))
	for _, r := range returns {
		if i := index(r.at); i >= 0 && i < len(series) {
			series[i].ReworkClasses[r.class.Name]++
		}
	}
	for _, f := range failures {
		i := index(f.at)
		if i < 0 || i >= len(series) {
			continue
		}
		series[i].Failed++
		switch classifyFailure(f.example).Kind {
		case reportKindInfrastructure:
			series[i].FailedInfrastructure++
		case reportKindIssue:
			series[i].FailedIssue++
		}
	}
}

func reportsSpendOutcomes(spend []reportsSpend, attempts []nativeAnalyticsAttempt, timelines []analyticsResidenceTimeline, shipped map[string]bool) {
	counts := map[string]int{}
	for _, a := range attempts {
		counts[a.WorkItemID]++
	}
	lanes := map[string]string{}
	for _, timeline := range timelines {
		lanes[timeline.issue.id] = timeline.state
	}
	for i := range spend {
		id := spend[i].WorkItemID
		spend[i].Attempts = counts[id]
		spend[i].Shipped = shipped[id]
		spend[i].State = lanes[id]
	}
}

const reportClassTopIssues = 3

func reportClassDetails(rework, failures []reportClassTotal, returns []qualityReturn, failed []nativeFailure) {
	type event struct {
		class, item string
		at          time.Time
	}
	fill := func(totals []reportClassTotal, events []event) {
		for i := range totals {
			counts := map[string]int{}
			for _, e := range events {
				if e.class != totals[i].Class {
					continue
				}
				counts[e.item]++
				at := e.at
				if totals[i].FirstSeen == nil || at.Before(*totals[i].FirstSeen) {
					totals[i].FirstSeen = &at
				}
				if totals[i].LastSeen == nil || at.After(*totals[i].LastSeen) {
					totals[i].LastSeen = &at
				}
			}
			items := make([]string, 0, len(counts))
			for item := range counts {
				if item != "" {
					items = append(items, item)
				}
			}
			slices.SortFunc(items, func(a, b string) int {
				if counts[a] != counts[b] {
					return counts[b] - counts[a]
				}
				return strings.Compare(a, b)
			})
			totals[i].WorkItems = items[:min(len(items), reportClassTopIssues)]
		}
	}
	reworkEvents := make([]event, 0, len(returns))
	for _, r := range returns {
		reworkEvents = append(reworkEvents, event{r.class.Name, r.item, r.at})
	}
	failureEvents := make([]event, 0, len(failed))
	for _, f := range failed {
		failureEvents = append(failureEvents, event{classifyFailure(f.example).Name, f.workItem, f.at})
	}
	fill(rework, reworkEvents)
	fill(failures, failureEvents)
}
