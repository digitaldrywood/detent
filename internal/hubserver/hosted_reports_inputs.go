package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// reportsDone is a shipped issue and the moment it closed.
type reportsDone struct {
	id string
	at time.Time
}

// reportsOpenIssue is an issue's lane at one instant.
type reportsOpenIssue struct {
	id, state string
	entered   time.Time
	started   *time.Time
	partial   bool
}

// reportsInputs is what the tab series, open work and prior values read:
// the shipped population of the window and the one before it, cycle timing
// for any set of shipped issues, and lane residence from the hourly rollups
// or, while they are incomplete, from raw residence history.
type reportsInputs struct {
	states    map[string]tracker.NativeState
	done      []reportsDone
	priorDone []reportsDone
	timing    func(ids []string, to time.Time) (hubanalytics.Completion, error)
	open      []reportsOpenIssue
	priorOpen []reportsOpenIssue
	lanes     func(from, to time.Time) map[string]float64
	clipped   bool
}

func reportsDoneFrom(shipped []nativeShippedIssue) []reportsDone {
	out := make([]reportsDone, 0, len(shipped))
	for _, issue := range shipped {
		out = append(out, reportsDone{id: issue.ID, at: issue.ClosedAt})
	}
	return out
}

func (in reportsInputs) shipped() map[string]bool {
	out := map[string]bool{}
	for _, done := range in.done {
		out[done.id] = true
	}
	return out
}

func (in reportsInputs) lanesOf() map[string]string {
	out := map[string]string{}
	for _, issue := range in.open {
		out[issue.id] = issue.state
	}
	return out
}

func reportsLanesExcept(summary operatortool.AnalyticsResidenceSummary) map[string]float64 {
	out := map[string]float64{}
	for _, lane := range summary.Lanes {
		if lane.Group != "excluded" {
			out[lane.Lane] = lane.Seconds
		}
	}
	return out
}

// reportsTimelineResidence fills the residence inputs from raw timelines.
func reportsTimelineResidence(in *reportsInputs, timelines []hubanalytics.ResidenceTimeline, issues []hubanalytics.ResidenceIssue, states map[string]tracker.NativeState, w operatortool.AnalyticsWindow, clipped bool) {
	in.states, in.clipped = states, clipped
	in.lanes = func(from, to time.Time) map[string]float64 {
		summary, _ := hubanalytics.SummarizeResidence(timelines, states, from, to)
		return reportsLanesExcept(summary)
	}
	in.open = nil
	for _, timeline := range timelines {
		in.open = append(in.open, reportsOpenIssue{id: timeline.Issue.ID, state: timeline.State, entered: timeline.Entered, started: timeline.Started, partial: timeline.Issue.Partial})
	}
	in.priorOpen = nil
	for _, issue := range issues {
		if !issue.Created.Before(w.From) {
			continue
		}
		earlier := issue
		earlier.Events = nil
		for _, event := range issue.Events {
			if event.At.Before(w.From) {
				earlier.Events = append(earlier.Events, event)
			}
		}
		timeline := hubanalytics.BuildResidenceTimeline(earlier, states, w.From)
		in.priorOpen = append(in.priorOpen, reportsOpenIssue{id: issue.ID, state: timeline.State, entered: timeline.Entered, started: timeline.Started, partial: timeline.Issue.Partial})
	}
}

// readReportsResidence fills the residence inputs from the hourly rollups, or
// from raw residence history while the project's rollups are incomplete.
func readReportsResidence(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, in *reportsInputs) error {
	history := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.To, Bucket: w.Bucket}
	if _, projected := q.(analyticsRollupReader); !projected {
		states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, q, scope, history)
		if err != nil {
			return err
		}
		timelines := make([]hubanalytics.ResidenceTimeline, 0, len(issues))
		for _, issue := range issues {
			timelines = append(timelines, hubanalytics.BuildResidenceTimeline(issue, states, w.To))
		}
		reportsTimelineResidence(in, timelines, issues, states, w, clipped)
		return nil
	}
	points := []time.Time{history.From, w.From, w.To}
	for _, span := range reportsSeriesSpans(w) {
		points = append(points, span.from, span.to)
	}
	frames, err := readAnalyticsResidenceFrames(ctx, q, scope, history, points)
	if err != nil {
		return err
	}
	in.states, in.clipped = frames.States, frames.Clipped
	in.lanes = func(from, to time.Time) map[string]float64 {
		summary, _ := hubanalytics.SummarizeResidenceFrames(frames, from, to)
		return reportsLanesExcept(summary)
	}
	at := func(instant time.Time) []reportsOpenIssue {
		out := make([]reportsOpenIssue, 0, len(frames.IDs))
		for _, id := range frames.IDs {
			sample, ok := frames.Sample(instant, id)
			if !ok || sample.State == "" {
				continue
			}
			out = append(out, reportsOpenIssue{id: id, state: sample.State, entered: sample.Entered, started: sample.Started, partial: sample.Partial})
		}
		return out
	}
	in.open, in.priorOpen = at(w.To), at(w.From)
	return nil
}
