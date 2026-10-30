package hubserver

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
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
	rework                reportsRatio
}

func reportsOpenAt(issue reportsOpenIssue, states map[string]tracker.NativeState, at time.Time) (string, bool) {
	if issue.partial || states[issue.state].Terminal || !issue.entered.Before(at) {
		return "", false
	}
	group := hubanalytics.ResidenceGroup(issue.state, states)
	return group, group == "system" || group == "held"
}

func reportsOpenWorkFor(in reportsInputs, lanes []operatortool.AnalyticsLaneDuration, w operatortool.AnalyticsWindow) reportsOpenWork {
	out := reportsOpenWork{Items: []operatortool.AnalyticsAging{}}
	p90 := map[string]float64{}
	for _, lane := range lanes {
		p90[lane.Lane] = lane.P90Seconds / 3600
	}
	for _, issue := range in.open {
		group, open := reportsOpenAt(issue, in.states, w.To)
		if !open {
			continue
		}
		if group == "held" {
			out.Held++
		} else {
			out.Count++
		}
		if w.To.Sub(issue.entered) > reportsStuckAfter {
			out.Stuck++
		}
		item := operatortool.AnalyticsAging{WorkItemID: issue.id, Lane: issue.state, EnteredAt: issue.entered, Hours: w.To.Sub(issue.entered).Hours()}
		if hours, ok := p90[issue.state]; ok {
			item.LaneP90Hours = &hours
		}
		if issue.started != nil {
			since := w.To.Sub(*issue.started).Hours()
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
	if in.clipped {
		return out
	}
	previous := 0
	for _, issue := range in.priorOpen {
		if _, open := reportsOpenAt(issue, in.states, w.From); open && w.From.Sub(issue.entered) > reportsStuckAfter {
			previous++
		}
	}
	out.PreviousStuck = &previous
	return out
}

// reportsPriorQuality reads the rework rate and infrastructure returns of the
// window before w through the same rollup reader as the analytics read.
func (s *Service) reportsPriorQuality(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, out *reportsPrior) error {
	q, err := analyticsProjectReader(ctx, analyticsRollupReader{s.database.reader}, scope)
	if err != nil {
		return err
	}
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	quality, err := readAnalyticsQuality(ctx, q, scope, operatortool.AnalyticsRequest{ProjectID: string(scope.project), Limit: hubanalytics.MaxPopulation}, prior)
	if err != nil {
		return err
	}
	out.ReworkPercent = quality.ReworkPercent
	out.rework = reportsRatio{quality.ReworkedItems, quality.WorkedItems}
	if !quality.Partial {
		returns := quality.InfraReturns
		out.InfrastructureReturns = &returns
	}
	return nil
}
