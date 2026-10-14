package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// reportsDone is an issue's last Done in a window, with its cycle and lead
// time when its history is complete.
type reportsDone struct {
	id           string
	at           time.Time
	system, lead float64
	timed        bool
}

// reportsOpenIssue is an issue's lane at one instant.
type reportsOpenIssue struct {
	id, state string
	entered   time.Time
	started   *time.Time
	partial   bool
}

// reportsInputs is what the tab series, open work and prior values read,
// built either from the hourly rollups or from raw residence history.
type reportsInputs struct {
	states    map[string]tracker.NativeState
	done      []reportsDone
	priorDone []reportsDone
	open      []reportsOpenIssue
	priorOpen []reportsOpenIssue
	lanes     func(from, to time.Time) map[string]float64
	clipped   bool
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

func reportsTimelineInputs(timelines []analyticsResidenceTimeline, issues []analyticsResidenceIssue, states map[string]tracker.NativeState, w operatortool.AnalyticsWindow, clipped bool) reportsInputs {
	span := w.To.Sub(w.From)
	in := reportsInputs{states: states, clipped: clipped}
	in.lanes = func(from, to time.Time) map[string]float64 {
		summary, _ := summarizeAnalyticsResidence(timelines, states, from, to)
		return reportsLanesExcept(summary)
	}
	doneAt := func(timeline analyticsResidenceTimeline, from, to time.Time) (reportsDone, bool) {
		at, ok := reportsLastDone(timeline, from, to)
		if !ok {
			return reportsDone{}, false
		}
		completion := reportsCompletions([]analyticsResidenceTimeline{timeline}, states, at, at.Add(time.Nanosecond))
		return reportsDone{id: timeline.issue.id, at: at, system: completion.System.Seconds, lead: completion.Lead.Seconds, timed: completion.System.Count == 1}, true
	}
	for _, timeline := range timelines {
		if done, ok := doneAt(timeline, w.From, w.To); ok {
			in.done = append(in.done, done)
		}
		if done, ok := doneAt(timeline, w.From.Add(-span), w.From); ok {
			in.priorDone = append(in.priorDone, done)
		}
		in.open = append(in.open, reportsOpenIssue{id: timeline.issue.id, state: timeline.state, entered: timeline.entered, started: timeline.started, partial: timeline.issue.partial})
	}
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
	return in
}

func readReportsRollupInputs(ctx context.Context, q nativeQueryer, scope nativeScope, w, history operatortool.AnalyticsWindow) (reportsInputs, error) {
	points := []time.Time{history.From, w.From, w.To}
	for _, span := range reportsSeriesSpans(w) {
		points = append(points, span.from, span.to)
	}
	frames, err := readAnalyticsResidenceFrames(ctx, q, scope, history, points)
	if err != nil {
		return reportsInputs{}, err
	}
	in := reportsInputs{states: frames.states, clipped: frames.clipped}
	in.lanes = func(from, to time.Time) map[string]float64 {
		summary, _ := summarizeResidenceFrames(frames, from, to)
		return reportsLanesExcept(summary)
	}
	at := func(instant time.Time) []reportsOpenIssue {
		out := make([]reportsOpenIssue, 0, len(frames.ids))
		for _, id := range frames.ids {
			sample, ok := frames.samples[instant.UnixNano()][id]
			if !ok || sample.State == "" {
				continue
			}
			out = append(out, reportsOpenIssue{id: id, state: sample.State, entered: sample.Entered, started: sample.Started, partial: sample.Partial})
		}
		return out
	}
	in.open, in.priorOpen = at(w.To), at(w.From)
	ids, err := json.Marshal(frames.ids)
	if err != nil {
		return in, err
	}
	latest := frames.samples[w.To.UnixNano()]
	read := func(from, to time.Time) ([]reportsDone, error) {
		rows, err := q.QueryContext(ctx, `SELECT (SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='completion' AND json_extract(data_json,'$.ID')=i.value AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<? ORDER BY json_extract(data_json,'$.AtNS') DESC,value DESC LIMIT 1) FROM json_each(?) i`, scope.organization, scope.project, from.UnixNano(), to.UnixNano(), string(ids))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []reportsDone
		for rows.Next() {
			var raw sql.NullString
			if err := rows.Scan(&raw); err != nil {
				return nil, err
			}
			if !raw.Valid {
				continue
			}
			var sample analyticsResidenceSample
			if err := json.Unmarshal([]byte(raw.String), &sample); err != nil {
				return nil, err
			}
			if sample.Events > latest[sample.ID].Events {
				continue
			}
			done := reportsDone{id: sample.ID, at: sample.At}
			if !sample.Partial && !latest[sample.ID].Partial && sample.Started != nil {
				done.system = residenceSampleItem(sample, frames.states, sample.At).SystemSeconds
				done.lead = sample.At.Sub(sample.Created).Seconds()
				done.timed = true
			}
			out = append(out, done)
		}
		return out, rows.Err()
	}
	if in.done, err = read(w.From, w.To); err != nil {
		return in, err
	}
	in.priorDone, err = read(history.From, w.From)
	return in, err
}
