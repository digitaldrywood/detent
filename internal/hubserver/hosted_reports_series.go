package hubserver

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
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

// reportsSeries counts each shipped issue once, on the day it closed, and
// splits every usage row in the window by stage, so the buckets sum to the
// Shipped and Total spend headlines.
func reportsSeries(in reportsInputs, attempts []hubanalytics.Attempt, usage map[string][]usageRow, w operatortool.AnalyticsWindow) ([]reportsSeriesBucket, error) {
	spans := reportsSeriesSpans(w)
	index := reportsSeriesIndex(spans)
	span := w.To.Sub(w.From)
	shift := make([]reportsSeriesSpan, len(spans))
	for i, s := range spans {
		shift[i] = reportsSeriesSpan{s.from.Add(-span), s.to.Add(-span)}
	}
	priorIndex := reportsSeriesIndex(shift)
	out := make([]reportsSeriesBucket, 0, len(spans))
	closed := make([][]string, len(spans))
	for _, s := range spans {
		out = append(out, reportsSeriesBucket{From: s.from, To: s.to, LaneSeconds: in.lanes(s.from, s.to), StageCost: map[string]float64{}, ReworkClasses: map[string]int{}})
	}
	for _, done := range in.done {
		if i := index(done.at); i >= 0 {
			out[i].Done++
			closed[i] = append(closed[i], done.id)
		}
	}
	for _, done := range in.priorDone {
		if i := priorIndex(done.at); i >= 0 {
			out[i].PriorDone++
		}
	}
	for i, ids := range closed {
		if len(ids) == 0 {
			continue
		}
		timing, err := in.timing(ids, spans[i].to)
		if err != nil {
			return nil, err
		}
		out[i].Cycle, out[i].Lead = timing.System, timing.Lead
	}
	shipped := in.shipped()
	byAttempt := map[string]hubanalytics.Attempt{}
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
	return out, nil
}

func reportsSeriesClasses(series []reportsSeriesBucket, returns []hubanalytics.QualityReturn, failures []hubanalytics.Failure, w operatortool.AnalyticsWindow) {
	index := reportsSeriesIndex(reportsSeriesSpans(w))
	for _, r := range returns {
		if i := index(r.At); i >= 0 && i < len(series) {
			series[i].ReworkClasses[r.Class.Name]++
		}
	}
	for _, f := range failures {
		i := index(f.At)
		if i < 0 || i >= len(series) {
			continue
		}
		series[i].Failed++
		switch classifyFailure(f.Example).Kind {
		case reportKindInfrastructure:
			series[i].FailedInfrastructure++
		case reportKindIssue:
			series[i].FailedIssue++
		}
	}
}

func reportsSpendOutcomes(spend []reportsSpend, attempts []hubanalytics.Attempt, in reportsInputs) {
	counts := map[string]int{}
	for _, a := range attempts {
		counts[a.WorkItemID]++
	}
	lanes, shipped := in.lanesOf(), in.shipped()
	for i := range spend {
		id := spend[i].WorkItemID
		spend[i].Attempts = counts[id]
		spend[i].Shipped = shipped[id]
		spend[i].State = lanes[id]
	}
}

const reportClassTopIssues = 3

// reportClassTopItems is the issues a class hit most often, most first.
func reportClassTopItems(counts map[string]int) []string {
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
	return items[:min(len(items), reportClassTopIssues)]
}

func reportClassDetails(rework, failures []reportClassTotal, returns []hubanalytics.QualityReturn, failed []hubanalytics.Failure) {
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
			totals[i].itemCounts = counts
			totals[i].WorkItems = reportClassTopItems(counts)
		}
	}
	reworkEvents := make([]event, 0, len(returns))
	for _, r := range returns {
		reworkEvents = append(reworkEvents, event{r.Class.Name, r.Item, r.At})
	}
	failureEvents := make([]event, 0, len(failed))
	for _, f := range failed {
		failureEvents = append(failureEvents, event{classifyFailure(f.Example).Name, f.WorkItem, f.At})
	}
	fill(rework, reworkEvents)
	fill(failures, failureEvents)
}
