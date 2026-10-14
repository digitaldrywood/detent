package hubserver

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reportsSeriesBucket struct {
	From                 time.Time                      `json:"from"`
	To                   time.Time                      `json:"to"`
	Done                 int                            `json:"done"`
	PriorDone            int                            `json:"prior_done"`
	Failed               int                            `json:"failed"`
	Cycle                operatortool.AnalyticsDuration `json:"cycle"`
	Lead                 operatortool.AnalyticsDuration `json:"lead"`
	LaneSeconds          map[string]float64             `json:"lane_seconds"`
	StageCost            map[string]float64             `json:"stage_cost"`
	NotShippedCost       float64                        `json:"not_shipped_cost"`
	ReworkClasses        map[string]int                 `json:"rework_classes"`
	FailedInfrastructure int                            `json:"failed_infrastructure"`
}

func reportsSeriesStep(w operatortool.AnalyticsWindow) time.Duration {
	if w.To.Sub(w.From) >= 7*24*time.Hour {
		return 24 * time.Hour
	}
	return time.Hour
}

func reportsShipped(timelines []analyticsResidenceTimeline, from, to time.Time) map[string]bool {
	shipped := map[string]bool{}
	for _, timeline := range timelines {
		for _, done := range timeline.done {
			if !done.Before(from) && done.Before(to) {
				shipped[timeline.issue.id] = true
				break
			}
		}
	}
	return shipped
}

func reportsSeries(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, attempts []nativeAnalyticsAttempt, usage map[string][]usageRow, shipped map[string]bool, w operatortool.AnalyticsWindow) []reportsSeriesBucket {
	step := reportsSeriesStep(w)
	span := w.To.Sub(w.From)
	out := []reportsSeriesBucket{}
	for from := w.From; from.Before(w.To); from = from.Add(step) {
		to := minTime(from.Add(step), w.To)
		completion := reportsCompletions(timelines, states, from, to)
		residence, _ := summarizeAnalyticsResidence(timelines, states, from, to)
		bucket := reportsSeriesBucket{
			From: from, To: to,
			Done:          completion.Done,
			PriorDone:     reportsCompletions(timelines, states, from.Add(-span), to.Add(-span)).Done,
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
	index := reportsSeriesIndex(w)
	for _, a := range attempts {
		if a.Status == "failed" {
			if i := index(a.ObservedAt); i >= 0 {
				out[i].Failed++
			}
		}
		stage := a.Identity.Role
		if stage == "" {
			stage = "Unavailable"
		}
		for _, u := range usage[a.AttemptID] {
			i := index(u.Period)
			if i < 0 {
				continue
			}
			out[i].StageCost[stage] += u.Cost
			if !shipped[a.WorkItemID] {
				out[i].NotShippedCost += u.Cost
			}
		}
	}
	return out
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

func reportsSeriesIndex(w operatortool.AnalyticsWindow) func(time.Time) int {
	step := reportsSeriesStep(w)
	return func(at time.Time) int {
		if at.Before(w.From) || !at.Before(w.To) {
			return -1
		}
		return int(at.Sub(w.From) / step)
	}
}

func reportsSeriesClasses(series []reportsSeriesBucket, returns []qualityReturn, failures []nativeFailure, w operatortool.AnalyticsWindow) {
	index := reportsSeriesIndex(w)
	for _, r := range returns {
		if i := index(r.at); i >= 0 && i < len(series) {
			series[i].ReworkClasses[r.class.Name]++
		}
	}
	for _, f := range failures {
		if i := index(f.at); i >= 0 && i < len(series) && classifyFailure(f.example).Kind == reportKindInfrastructure {
			series[i].FailedInfrastructure++
		}
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
