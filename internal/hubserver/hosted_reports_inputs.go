package hubserver

import (
	"context"
	"time"

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

type reportsInputs struct {
	states    map[string]tracker.NativeState
	done      []reportsDone
	priorDone []reportsDone
	timing    func(ids []string, to time.Time) (reportsCompletion, error)
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
func reportsTimelineResidence(in *reportsInputs, timelines []analyticsResidenceTimeline, issues []analyticsResidenceIssue, states map[string]tracker.NativeState, w operatortool.AnalyticsWindow, clipped bool) {
	in.states, in.clipped = states, clipped
	in.lanes = func(from, to time.Time) map[string]float64 {
		summary, _ := summarizeAnalyticsResidence(timelines, states, from, to)
		return reportsLanesExcept(summary)
	}
	in.open = nil
	for _, timeline := range timelines {
		in.open = append(in.open, reportsOpenIssue{id: timeline.issue.id, state: timeline.state, entered: timeline.entered, started: timeline.started, partial: timeline.issue.partial})
	}
	in.priorOpen = nil
	for _, issue := range issues {
		if !issue.created.Before(w.From) {
			continue
		}
		earlier := issue
		earlier.events = nil
		for _, event := range issue.events {
			if event.at.Before(w.From) {
				earlier.events = append(earlier.events, event)
			}
		}
		timeline := buildAnalyticsResidenceTimeline(earlier, states, w.From)
		in.priorOpen = append(in.priorOpen, reportsOpenIssue{id: issue.id, state: timeline.state, entered: timeline.entered, started: timeline.started, partial: timeline.issue.partial})
	}
}

func readReportsResidence(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, in *reportsInputs) error {
	history := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.To, Bucket: w.Bucket}

	states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, q, scope, history)
	if err != nil {
		return err
	}
	timelines := make([]analyticsResidenceTimeline, 0, len(issues))
	for _, issue := range issues {
		timelines = append(timelines, buildAnalyticsResidenceTimeline(issue, states, w.To))
	}
	reportsTimelineResidence(in, timelines, issues, states, w, clipped)
	return nil

}
