package hubanalytics

import (
	"slices"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ResidenceIssue struct {
	ID      string           `json:"-"`
	Created time.Time        `json:"-"`
	Initial string           `json:"-"`
	Events  []ResidenceEvent `json:"-"`
	Partial bool             `json:"-"`
}

type ResidenceEvent struct {
	At   time.Time                 `json:"-"`
	Kind string                    `json:"-"`
	Data tracker.CollaborationData `json:"-"`
}

type ResidenceSpan struct {
	Lane    string    `json:"-"`
	From    time.Time `json:"-"`
	To      time.Time `json:"-"`
	queueTo time.Time
}

type ResidenceTimeline struct {
	Issue   ResidenceIssue  `json:"-"`
	Started *time.Time      `json:"-"`
	Spans   []ResidenceSpan `json:"-"`
	State   string          `json:"-"`
	Entered time.Time       `json:"-"`
	Done    []time.Time     `json:"-"`
	rework  []ResidenceEvent
}

func BuildResidenceTimeline(issue ResidenceIssue, states map[string]tracker.NativeState, to time.Time) ResidenceTimeline {
	timeline := ResidenceTimeline{Issue: issue, State: issue.Initial, Entered: issue.Created}
	if issue.Initial == "" {
		timeline.Issue.Partial = true
	}
	if states[issue.Initial].Dispatchable && ResidenceGroup(issue.Initial, states) == "system" {
		at := issue.Created
		timeline.Started = &at
	}
	queueTo := to
	last := issue.Created
	for _, event := range issue.Events {
		if event.At.Before(last) {
			timeline.Issue.Partial = true
			continue
		}
		last = event.At
		if event.Kind == "scheduler.decision" {
			if timeline.State == "Rework" && queueTo.After(event.At) {
				queueTo = event.At
			}
			continue
		}
		if event.Data.FromState != timeline.State || event.Data.ToState == "" {
			timeline.Issue.Partial = true
		} else if event.Data.ToState != timeline.State && timeline.Started != nil {
			timeline.Spans = append(timeline.Spans, ResidenceSpan{Lane: timeline.State, From: maxTime(timeline.Entered, *timeline.Started), To: event.At, queueTo: minTime(queueTo, event.At)})
		}
		if event.Data.ToState == "" || event.Data.ToState == timeline.State {
			continue
		}
		timeline.State, timeline.Entered = event.Data.ToState, event.At
		queueTo = to
		if timeline.Started == nil && states[timeline.State].Dispatchable && ResidenceGroup(timeline.State, states) == "system" {
			at := event.At
			timeline.Started = &at
		}
		if timeline.State == "Done" {
			timeline.Done = append(timeline.Done, event.At)
		}
		if timeline.State == "Rework" {
			timeline.rework = append(timeline.rework, event)
		}
	}
	if timeline.Started != nil && !timeline.Issue.Partial {
		timeline.Spans = append(timeline.Spans, ResidenceSpan{Lane: timeline.State, From: maxTime(timeline.Entered, *timeline.Started), To: to, queueTo: queueTo})
	}
	return timeline
}

func ResidenceGroup(lane string, states map[string]tracker.NativeState) string {
	if lane == "Backlog" || lane == "Triage" || lane == "Done" || lane == "Cancelled" || states[lane].Terminal {
		return "excluded"
	}
	if lane == "Blocked" || lane == "Human Review" {
		return "held"
	}
	return "system"
}

func SummarizeResidence(timelines []ResidenceTimeline, states map[string]tracker.NativeState, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	var system, held, lead, queue []float64
	causes := map[[2]string]operatortool.AnalyticsReworkCause{}
	for _, timeline := range timelines {
		item := operatortool.AnalyticsIssueResidence{WorkItemID: timeline.Issue.ID, SystemStartedAt: timeline.Started, LaneSeconds: map[string]float64{}, Partial: timeline.Issue.Partial}
		for _, span := range timeline.Spans {
			group := ResidenceGroup(span.Lane, states)
			seconds := minTime(span.To, to).Sub(maxTime(span.From, from)).Seconds()
			if seconds <= 0 || group == "excluded" {
				continue
			}
			item.LaneSeconds[span.Lane] += seconds
			if group == "held" {
				item.HeldSeconds += seconds
			} else {
				item.SystemSeconds += seconds
			}
			if span.Lane == "Todo" || span.Lane == "Rework" {
				end := span.To
				if span.Lane == "Rework" {
					end = span.queueTo
				}
				item.QueueSeconds += max(0, minTime(end, to).Sub(maxTime(span.From, from)).Seconds())
			}
		}
		for _, done := range timeline.Done {
			if !done.Before(from) && done.Before(to) {
				seconds := done.Sub(timeline.Issue.Created).Seconds()
				item.LeadSeconds = &seconds
			}
		}
		for lane, seconds := range item.LaneSeconds {
			lanes[lane] = append(lanes[lane], seconds)
		}
		if item.SystemSeconds > 0 {
			system = append(system, item.SystemSeconds)
		}
		if item.HeldSeconds > 0 {
			held = append(held, item.HeldSeconds)
		}
		if item.QueueSeconds > 0 {
			queue = append(queue, item.QueueSeconds)
		}
		if item.LeadSeconds != nil {
			lead = append(lead, *item.LeadSeconds)
		}
		for _, event := range timeline.rework {
			if event.At.Before(from) || !event.At.Before(to) {
				continue
			}
			key := [2]string{event.Data.FromState, event.Data.ReasonDetail}
			cause := causes[key]
			cause.FromState, cause.ReasonDetail = event.Data.FromState, event.Data.ReasonDetail
			cause.Count++
			causes[key] = cause
		}
		items = append(items, item)
	}
	for lane, values := range lanes {
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: ResidenceGroup(lane, states), AnalyticsDuration: SampledDuration(values)})
	}
	sort.Slice(summary.Lanes, func(i, j int) bool { return summary.Lanes[i].Lane < summary.Lanes[j].Lane })
	for _, cause := range causes {
		summary.ReworkCauses = append(summary.ReworkCauses, cause)
	}
	sort.Slice(summary.ReworkCauses, func(i, j int) bool {
		a, b := summary.ReworkCauses[i], summary.ReworkCauses[j]
		if a.FromState != b.FromState {
			return a.FromState < b.FromState
		}
		return a.ReasonDetail < b.ReasonDetail
	})
	summary.SystemTotal, summary.HeldTotal = SampledDuration(system), SampledDuration(held)
	summary.LeadTotal, summary.QueueTotal = SampledDuration(lead), SampledDuration(queue)
	return summary, items
}

// sampledDuration is analyticsDuration keeping its sorted samples, so that
// durations read for several projects combine exactly.
func SampledDuration(values []float64) operatortool.AnalyticsDuration {
	out := Duration(values)
	if len(values) > 0 {
		out.Samples = slices.Clone(values)
	}
	return out
}

func Duration(values []float64) operatortool.AnalyticsDuration {
	out := operatortool.AnalyticsDuration{Count: len(values)}
	if len(values) == 0 {
		return out
	}
	sort.Float64s(values)
	for _, seconds := range values {
		out.Seconds += seconds
	}
	quantile := func(q float64) float64 {
		index := q * float64(len(values)-1)
		lower := int(index)
		upper := min(lower+1, len(values)-1)
		return values[lower] + (values[upper]-values[lower])*(index-float64(lower))
	}
	out.P50Seconds, out.P90Seconds = quantile(0.5), quantile(0.9)
	return out
}
