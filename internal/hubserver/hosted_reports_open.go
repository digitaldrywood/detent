package hubserver

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	reportsStuckAfter   = 24 * time.Hour
	reportsOpenWorkRows = 50
)

// reportsOpenWork counts open issues in the lanes Detent works (system) and
// the lanes people hold, never Backlog or Triage, over the whole population.
type reportsOpenWork struct {
	Count         int                           `json:"count"`
	Held          int                           `json:"held"`
	Stuck         int                           `json:"stuck"`
	PreviousStuck *int                          `json:"previous_stuck,omitempty"`
	Items         []operatortool.AnalyticsAging `json:"items"`
}

type reportsPrior struct {
	ReworkPercent         *float64           `json:"rework_percent"`
	InfrastructureReturns *int               `json:"infrastructure_returns,omitempty"`
	LaneSeconds           map[string]float64 `json:"lane_seconds"`
}

func reportsOpenAt(timeline analyticsResidenceTimeline, states map[string]tracker.NativeState, at time.Time) (string, bool) {
	if timeline.issue.partial || states[timeline.state].Terminal || !timeline.entered.Before(at) {
		return "", false
	}
	group := analyticsResidenceGroup(timeline.state, states)
	return group, group == "system" || group == "held"
}

func reportsOpenWorkFor(timelines []analyticsResidenceTimeline, issues []analyticsResidenceIssue, states map[string]tracker.NativeState, lanes []operatortool.AnalyticsLaneDuration, w operatortool.AnalyticsWindow, clipped bool) reportsOpenWork {
	out := reportsOpenWork{Items: []operatortool.AnalyticsAging{}}
	p90 := map[string]float64{}
	for _, lane := range lanes {
		p90[lane.Lane] = lane.P90Seconds / 3600
	}
	for _, timeline := range timelines {
		group, open := reportsOpenAt(timeline, states, w.To)
		if !open {
			continue
		}
		if group == "held" {
			out.Held++
		} else {
			out.Count++
		}
		if w.To.Sub(timeline.entered) > reportsStuckAfter {
			out.Stuck++
		}
		item := operatortool.AnalyticsAging{WorkItemID: timeline.issue.id, Lane: timeline.state, EnteredAt: timeline.entered, Hours: w.To.Sub(timeline.entered).Hours()}
		if hours, ok := p90[timeline.state]; ok {
			item.LaneP90Hours = &hours
		}
		if timeline.started != nil {
			since := w.To.Sub(*timeline.started).Hours()
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
			return 0
		}
	})
	out.Items = out.Items[:min(len(out.Items), reportsOpenWorkRows)]
	if clipped {
		return out
	}
	previous := 0
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
		if _, open := reportsOpenAt(timeline, states, w.From); open && w.From.Sub(timeline.entered) > reportsStuckAfter {
			previous++
		}
	}
	out.PreviousStuck = &previous
	return out
}

func (s *Service) reportsPriorWindow(ctx context.Context, scope nativeScope, timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, w operatortool.AnalyticsWindow) (reportsPrior, error) {
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	out := reportsPrior{LaneSeconds: reportsLaneSeconds(timelines, states, prior)}
	quality, err := readAnalyticsQuality(ctx, s.database.reader, scope, operatortool.AnalyticsRequest{ProjectID: string(scope.project), Limit: maxAnalyticsPopulation}, prior)
	if err != nil {
		return out, err
	}
	out.ReworkPercent = quality.ReworkPercent
	if !quality.Partial {
		returns := quality.InfraReturns
		out.InfrastructureReturns = &returns
	}
	return out, nil
}

// reportsLaneSeconds totals residence per lane over a window, the same
// population as the lane table's Total and Share.
func reportsLaneSeconds(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, w operatortool.AnalyticsWindow) map[string]float64 {
	out := map[string]float64{}
	residence, _ := summarizeAnalyticsResidence(timelines, states, w.From, w.To)
	for _, lane := range residence.Lanes {
		if lane.Group != "excluded" {
			out[lane.Lane] = lane.Seconds
		}
	}
	return out
}
