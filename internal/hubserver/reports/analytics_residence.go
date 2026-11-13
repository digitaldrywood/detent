package reports

import (
	"context"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AnalyticsResidenceIssue struct {
	Id      string                    `json:"-"`
	Created time.Time                 `json:"-"`
	Initial string                    `json:"-"`
	Events  []AnalyticsResidenceEvent `json:"-"`
	Partial bool                      `json:"-"`
}

type AnalyticsResidenceEvent struct {
	At   time.Time                 `json:"-"`
	Kind string                    `json:"-"`
	Data tracker.CollaborationData `json:"-"`
}

type analyticsResidenceSpan struct {
	Lane    string    `json:"-"`
	From    time.Time `json:"-"`
	To      time.Time `json:"-"`
	queueTo time.Time
}

type AnalyticsResidenceTimeline struct {
	Issue   AnalyticsResidenceIssue  `json:"-"`
	Started *time.Time               `json:"-"`
	Spans   []analyticsResidenceSpan `json:"-"`
	State   string                   `json:"-"`
	Entered time.Time                `json:"-"`
	Done    []time.Time              `json:"-"`
	rework  []AnalyticsResidenceEvent
}

func ProjectAnalyticsResidencePopulation(states map[string]tracker.NativeState, issues []AnalyticsResidenceIssue, clipped bool, r operatortool.AnalyticsRequest, out *NativeAnalyticsProject) error {
	residence := operatortool.AnalyticsResidence{
		Source:         "native_workflow_transitions_and_issue_creation",
		Coverage:       "per_issue; residence_clipped_to_window; system_excludes_intake_terminal_and_held; held_after_first_dispatch; lead_created_to_done_at_completion; aging_at_window_end; quantiles_linear_interpolation",
		IssuesObserved: len(issues),
	}
	timelines := make([]AnalyticsResidenceTimeline, 0, len(issues))
	available := false
	for _, issue := range issues {
		available = available || issue.Initial != ""
		residence.EventsObserved += len(issue.Events)
		for _, event := range issue.Events {
			if event.At.After(out.SourceAt) {
				out.SourceAt = event.At
			}
		}
		if issue.Created.After(out.SourceAt) {
			out.SourceAt = issue.Created
		}
		timeline := BuildAnalyticsResidenceTimeline(issue, states, out.Window.To)
		clipped = clipped || timeline.Issue.Partial
		timelines = append(timelines, timeline)
		out.PipelineTimings = append(out.PipelineTimings, residencePipelineTimings(timeline, out.AllAttempts)...)
	}
	var items []operatortool.AnalyticsIssueResidence
	residence.AnalyticsResidenceSummary, items = summarizeAnalyticsResidence(timelines, states, out.Window.From, out.Window.To)
	residence.Partial = clipped
	residence.Issues = operatortool.OffsetPage(items, r.RowOffset, r.Limit)
	aging := []operatortool.AnalyticsAging{}
	for _, timeline := range timelines {
		if timeline.Issue.Partial || states[timeline.State].Terminal || !timeline.Entered.Before(out.Window.To) {
			continue
		}
		item := operatortool.AnalyticsAging{WorkItemID: timeline.Issue.Id, Lane: timeline.State, EnteredAt: timeline.Entered, Hours: out.Window.To.Sub(timeline.Entered).Hours()}
		if timeline.Started != nil {
			since := out.Window.To.Sub(*timeline.Started).Hours()
			item.SinceTodoHours = &since
		}
		for _, lane := range residence.Lanes {
			if lane.Lane == item.Lane {
				hours := lane.P90Seconds / 3600
				item.LaneP90Hours = &hours
				break
			}
		}
		aging = append(aging, item)
	}
	residence.Aging = operatortool.OffsetPage(aging, r.RowOffset, r.Limit)
	out.LaneResidence = residence
	out.Partial = out.Partial || clipped
	for i := range out.Digest {
		bucket := &out.Digest[i]
		bucket.LaneResidence, _ = summarizeAnalyticsResidence(timelines, states, bucket.From, bucket.To)
		bucket.LaneResidence.Partial = clipped
		bucket.QueueSeconds = bucket.LaneResidence.QueueTotal.Seconds
	}
	out.QueueTime.Source = residence.Source
	out.QueueTime.Coverage = "todo_residence_and_rework_before_claim; includes_unclaimed_waits; clipped_to_window"
	out.QueueTime.Seconds = residence.QueueTotal.Seconds
	out.QueueTime.Duration = residence.QueueTotal
	out.QueueTime.Partial = clipped
	if !available {
		out.QueueTime.Partial = true
		out.Unavailable = append(out.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "queue_time")
	} else {
		if clipped {
			out.Unavailable = append(out.Unavailable, "lane_residence_complete_population", "queue_time_complete_population")
		}
	}
	return nil
}

type AnalyticsLiveIssuesKey struct{}

func WithAnalyticsLiveIssues(ctx context.Context) context.Context {
	return context.WithValue(ctx, AnalyticsLiveIssuesKey{}, true)
}

type analyticsResidenceFold struct {
	Issue   AnalyticsResidenceIssue `json:"-"`
	states  map[string]tracker.NativeState
	To      time.Time `json:"-"`
	queueTo time.Time
	last    time.Time
	Started *time.Time `json:"-"`
	State   string     `json:"-"`
	Entered time.Time  `json:"-"`
}

type analyticsResidenceDelta struct {
	span    analyticsResidenceSpan
	hasSpan bool
	Done    bool `json:"-"`
	rework  bool
}

func newAnalyticsResidenceFold(issue AnalyticsResidenceIssue, states map[string]tracker.NativeState, to time.Time) analyticsResidenceFold {
	fold := analyticsResidenceFold{Issue: issue, states: states, To: to, queueTo: to, last: issue.Created, State: issue.Initial, Entered: issue.Created}
	if issue.Initial == "" {
		fold.Issue.Partial = true
	}
	if states[issue.Initial].Dispatchable && analyticsResidenceGroup(issue.Initial, states) == "system" {
		at := issue.Created
		fold.Started = &at
	}
	return fold
}

func (fold *analyticsResidenceFold) append(event AnalyticsResidenceEvent) analyticsResidenceDelta {
	delta := analyticsResidenceDelta{}
	if event.At.Before(fold.last) {
		fold.Issue.Partial = true
		return delta
	}
	fold.last = event.At
	if event.Kind == "scheduler.decision" {
		if fold.State == "Rework" && fold.queueTo.After(event.At) {
			fold.queueTo = event.At
		}
		return delta
	}
	if event.Data.FromState != fold.State || event.Data.ToState == "" {
		fold.Issue.Partial = true
	} else if event.Data.ToState != fold.State && fold.Started != nil {
		delta.span = analyticsResidenceSpan{Lane: fold.State, From: hubapi.MaxTime(fold.Entered, *fold.Started), To: event.At, queueTo: hubapi.MinTime(fold.queueTo, event.At)}
		delta.hasSpan = true
	}
	if event.Data.ToState == "" || event.Data.ToState == fold.State {
		return delta
	}
	fold.State, fold.Entered = event.Data.ToState, event.At
	fold.queueTo = fold.To
	if fold.Started == nil && fold.states[fold.State].Dispatchable && analyticsResidenceGroup(fold.State, fold.states) == "system" {
		at := event.At
		fold.Started = &at
	}
	delta.Done = fold.State == "Done"
	delta.rework = fold.State == "Rework"
	return delta
}

func (fold *analyticsResidenceFold) finish() (analyticsResidenceSpan, bool) {
	if fold.Started == nil || fold.Issue.Partial {
		return analyticsResidenceSpan{}, false
	}
	return analyticsResidenceSpan{Lane: fold.State, From: hubapi.MaxTime(fold.Entered, *fold.Started), To: fold.To, queueTo: fold.queueTo}, true
}

func BuildAnalyticsResidenceTimeline(issue AnalyticsResidenceIssue, states map[string]tracker.NativeState, to time.Time) AnalyticsResidenceTimeline {
	fold := newAnalyticsResidenceFold(issue, states, to)
	timeline := AnalyticsResidenceTimeline{}
	for _, event := range issue.Events {
		delta := fold.append(event)
		if delta.hasSpan {
			timeline.Spans = append(timeline.Spans, delta.span)
		}
		if delta.Done {
			timeline.Done = append(timeline.Done, event.At)
		}
		if delta.rework {
			timeline.rework = append(timeline.rework, event)
		}
	}
	if span, ok := fold.finish(); ok {
		timeline.Spans = append(timeline.Spans, span)
	}
	timeline.Issue, timeline.Started, timeline.State, timeline.Entered = fold.Issue, fold.Started, fold.State, fold.Entered
	return timeline
}

func analyticsResidenceGroup(lane string, states map[string]tracker.NativeState) string {
	if lane == "Backlog" || lane == "Triage" || lane == "Done" || lane == "Cancelled" || states[lane].Terminal {
		return "excluded"
	}
	if lane == "Blocked" || lane == "Human Review" {
		return "held"
	}
	return "system"
}

func summarizeAnalyticsResidence(timelines []AnalyticsResidenceTimeline, states map[string]tracker.NativeState, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	var system, held, lead, queue []float64
	causes := map[[2]string]operatortool.AnalyticsReworkCause{}
	for _, timeline := range timelines {
		item := operatortool.AnalyticsIssueResidence{WorkItemID: timeline.Issue.Id, SystemStartedAt: timeline.Started, LaneSeconds: map[string]float64{}, Partial: timeline.Issue.Partial}
		for _, span := range timeline.Spans {
			group := analyticsResidenceGroup(span.Lane, states)
			seconds := hubapi.MinTime(span.To, to).Sub(hubapi.MaxTime(span.From, from)).Seconds()
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
				item.QueueSeconds += max(0, hubapi.MinTime(end, to).Sub(hubapi.MaxTime(span.From, from)).Seconds())
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
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, states), AnalyticsDuration: reportdata.SampledDuration(values)})
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
	summary.SystemTotal, summary.HeldTotal = reportdata.SampledDuration(system), reportdata.SampledDuration(held)
	summary.LeadTotal, summary.QueueTotal = reportdata.SampledDuration(lead), reportdata.SampledDuration(queue)
	return summary, items
}
