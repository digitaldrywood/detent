package residence

import (
	"maps"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Checkpoint(issue reportstore.HistoryIssue, states map[string]tracker.NativeState) reportstore.HistoryBase {
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	fold := NewFold(Issue{Created: issue.CreatedAt, Initial: issue.InitialState, Checkpoint: issue.Base}, states, end)
	result := reportstore.HistoryBase{LaneSeconds: map[string]float64{}}
	if issue.Base != nil {
		result.LaneSeconds = maps.Clone(issue.Base.LaneSeconds)
		result.SystemSeconds = issue.Base.SystemSeconds
	}
	for _, event := range issue.Events {
		delta := fold.Append(Event{At: event.At, Kind: event.Type, Data: tracker.CollaborationData{FromState: event.FromState, ToState: event.ToState}})
		if !delta.HasSpan || result.LaneSeconds == nil {
			continue
		}
		if _, known := states[delta.Span.Lane]; !known {
			result.LaneSeconds = nil
			continue
		}
		seconds := max(0, delta.Span.To.Sub(delta.Span.From).Seconds())
		result.LaneSeconds[delta.Span.Lane] += seconds
		if Group(delta.Span.Lane, states) == "system" {
			result.SystemSeconds += seconds
		}
	}
	result.Last, result.State, result.Entered = fold.last, fold.State, fold.Entered
	result.Started, result.Partial = fold.Started, fold.Issue.Partial
	if fold.queueTo.Before(end) {
		result.QueueCut = &fold.queueTo
	}
	return result
}
