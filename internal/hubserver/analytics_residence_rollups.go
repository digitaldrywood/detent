package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsResidenceSample struct {
	ID          string
	Created     time.Time
	At          time.Time
	AtNS        int64
	Initial     string
	State       string
	Entered     time.Time
	Started     *time.Time
	Partial     bool
	Events      int
	Transitions int
	Lanes       map[string]float64
	Queue       float64
	QueueTo     *time.Time
	LastDone    *time.Time
	MergeClaim  *time.Time
	Sequence    int64
	LanesNS     map[string]int64
	QueueNS     int64
	Causes      map[string]operatortool.AnalyticsReworkCause
}

func analyticsResidenceStates(ctx context.Context, q nativeQueryer, scope nativeScope) (map[string]tracker.NativeState, error) {
	rows, err := q.QueryContext(ctx, `SELECT detent_state,terminal,dispatchable FROM workflow_states WHERE project_id=? LIMIT 50`, scope.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]tracker.NativeState{}
	for rows.Next() {
		var state tracker.NativeState
		if err := rows.Scan(&state.Name, &state.Terminal, &state.Dispatchable); err != nil {
			return nil, err
		}
		states[state.Name] = state
	}
	return states, rows.Err()
}

func writeAnalyticsResidenceSample(ctx context.Context, tx *sql.Tx, scope nativeScope, metric, dimension string, sample analyticsResidenceSample) error {
	sample.AtNS = sample.At.UnixNano()
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups VALUES(?,?,?,?,?,?,?) ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=excluded.value,data_json=excluded.data_json`, scope.organization, scope.project, sample.At.Truncate(time.Hour).Unix(), metric, dimension, sample.Events, string(raw))
	return err
}

func refreshAnalyticsResidence(ctx context.Context, tx *sql.Tx, scope nativeScope, id string, after int64) error {
	states, err := analyticsResidenceStates(ctx, tx, scope)
	if err != nil {
		return err
	}
	var created, initial string
	err = tx.QueryRowContext(ctx, `SELECT i.created_at,coalesce((SELECT json_extract(data_json,'$.from_state') FROM collaboration_events WHERE organization_id=i.organization_id AND project_id=i.project_id AND work_item_id=i.native_id AND type='workflow.transitioned' ORDER BY sequence LIMIT 1),CASE WHEN EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=i.organization_id AND work_item_id=i.native_id AND type='issue.created') THEN ws.detent_state END,'') FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, scope.organization, scope.project, id).Scan(&created, &initial)
	if err != nil {
		return err
	}
	start, err := parseTimeValue(created)
	if err != nil {
		return err
	}
	sample := analyticsResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&previous)
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return err
	}
	if !fresh {
		if err := json.Unmarshal([]byte(previous), &sample); err != nil {
			return err
		}
		fresh = sample.Initial != initial
	}
	if fresh {
		after = 0
		sample = analyticsResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
		if states[initial].Dispatchable && analyticsResidenceGroup(initial, states) == "system" {
			sample.Started = &start
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry') AND json_extract(data_json,'$.ID')=?`, scope.organization, scope.project, id); err != nil {
			return err
		}
	}
	write := func(sequence int64) error {
		if err := writeAnalyticsResidenceSample(ctx, tx, scope, "residence_hour", id, sample); err != nil {
			return err
		}
		return writeAnalyticsResidenceSample(ctx, tx, scope, "residence_edge", id+":"+strconv.FormatInt(sequence, 10), sample)
	}
	if fresh {
		if err := write(0); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,type,recorded_at,data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND json_extract(data_json,'$.decision.source')='native_claim' AND json_extract(data_json,'$.decision.outcome')='claimed')) ORDER BY sequence`, scope.organization, scope.project, id, max(sample.Sequence, after))
	if err != nil {
		return err
	}
	defer rows.Close()
	type event struct {
		sequence int64
		analyticsResidenceEvent
	}
	var events []event
	for rows.Next() {
		var e event
		var at, raw string
		if err := rows.Scan(&e.sequence, &e.kind, &at, &raw); err != nil {
			return err
		}
		e.at, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &e.data); err != nil {
			return err
		}
		events = append(events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	mergeClaim := sample.MergeClaim
	writeMerge := func(to time.Time) error {
		if sample.Started == nil {
			return nil
		}
		raw, err := json.Marshal(analyticsMergeSample{FromNS: maxTime(sample.Entered, *sample.Started).UnixNano(), ID: id, From: maxTime(sample.Entered, *sample.Started), To: to, Claim: mergeClaim})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups VALUES(?,?,?,'residence_merge',?,1,?) ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET data_json=excluded.data_json`, scope.organization, scope.project, sample.Entered.Truncate(time.Hour).Unix(), id+":"+strconv.FormatInt(sample.Entered.UnixNano(), 10), string(raw))
		return err
	}
	for _, e := range events {
		sample.Sequence = e.sequence
		sample.Events++
		if e.kind == "workflow.transitioned" {
			sample.Transitions++
			if e.data.ToState == "Merging" {
				at := sample.At
				sample.At = e.at
				if err := writeAnalyticsResidenceSample(ctx, tx, scope, "merge_entry", id+":"+strconv.FormatInt(e.sequence, 10), sample); err != nil {
					return err
				}
				sample.At = at
			}
		}
		if e.at.Before(sample.At) {
			sample.Partial = true
			if err := write(e.sequence); err != nil {
				return err
			}
			continue
		}
		sample.At = e.at
		if e.kind == "scheduler.decision" {
			if sample.State == "Rework" && sample.QueueTo == nil {
				at := e.at
				sample.QueueTo = &at
			}
			if sample.State == "Merging" && mergeClaim == nil {
				at := e.at
				mergeClaim = &at
			}
		} else {
			if e.data.FromState != sample.State || e.data.ToState == "" {
				sample.Partial = true
			} else if e.data.ToState != sample.State && sample.Started != nil {
				from := maxTime(sample.Entered, *sample.Started)
				if analyticsResidenceGroup(sample.State, states) != "excluded" {
					sample.Lanes[sample.State] += max(0, e.at.Sub(from).Seconds())
					sample.LanesNS[sample.State] += max(int64(0), e.at.Sub(from).Nanoseconds())
				}
				if sample.State == "Todo" || sample.State == "Rework" {
					to := e.at
					if sample.QueueTo != nil {
						to = minTime(to, *sample.QueueTo)
					}
					sample.Queue += max(0, to.Sub(from).Seconds())
					sample.QueueNS += max(int64(0), to.Sub(from).Nanoseconds())
				}
				if sample.State == "Merging" {
					if err := writeMerge(e.at); err != nil {
						return err
					}
				}
			}
			if e.data.ToState != "" && e.data.ToState != sample.State {
				sample.State, sample.Entered = e.data.ToState, e.at
				sample.QueueTo = nil
				mergeClaim = nil
				if sample.Started == nil && states[sample.State].Dispatchable && analyticsResidenceGroup(sample.State, states) == "system" {
					at := e.at
					sample.Started = &at
				}
				if sample.State == "Done" {
					at := e.at
					sample.LastDone = &at
					if err := writeAnalyticsResidenceSample(ctx, tx, scope, "completion", id+":"+strconv.FormatInt(e.sequence, 10), sample); err != nil {
						return err
					}
				}
				if sample.State == "Rework" {
					key := e.data.FromState + "\x00" + e.data.ReasonDetail
					cause := sample.Causes[key]
					cause.FromState, cause.ReasonDetail = e.data.FromState, e.data.ReasonDetail
					cause.Count++
					sample.Causes[key] = cause
				}
			}
		}
		sample.MergeClaim = mergeClaim
		if err := write(e.sequence); err != nil {
			return err
		}
	}
	if sample.State == "Merging" && !sample.Partial {
		if err := writeMerge(time.Time{}); err != nil {
			return err
		}
	}
	return nil
}

type analyticsMergeSample struct {
	FromNS int64

	ID       string
	From, To time.Time
	Claim    *time.Time
}

func residenceSampleItem(sample analyticsResidenceSample, states map[string]tracker.NativeState, at time.Time) operatortool.AnalyticsIssueResidence {
	item := operatortool.AnalyticsIssueResidence{WorkItemID: sample.ID, SystemStartedAt: sample.Started, LaneSeconds: map[string]float64{}, QueueSeconds: sample.Queue, Partial: sample.Partial}
	for lane, seconds := range sample.Lanes {
		item.LaneSeconds[lane] = seconds
	}
	if sample.Started != nil && !sample.Partial {
		from := maxTime(sample.Entered, *sample.Started)
		if analyticsResidenceGroup(sample.State, states) != "excluded" {
			item.LaneSeconds[sample.State] += max(0, at.Sub(from).Seconds())
		}
		if sample.State == "Todo" || sample.State == "Rework" {
			to := at
			if sample.QueueTo != nil {
				to = minTime(to, *sample.QueueTo)
			}
			item.QueueSeconds += max(0, to.Sub(from).Seconds())
		}
	}
	for lane, seconds := range item.LaneSeconds {
		if analyticsResidenceGroup(lane, states) == "held" {
			item.HeldSeconds += seconds
		} else {
			item.SystemSeconds += seconds
		}
	}
	return item
}

type analyticsResidenceFrames struct {
	states  map[string]tracker.NativeState
	ids     []string
	samples map[int64]map[string]analyticsResidenceSample
	clipped bool
}

func readAnalyticsResidenceFrames(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, points []time.Time) (analyticsResidenceFrames, error) {
	out := analyticsResidenceFrames{samples: map[int64]map[string]analyticsResidenceSample{}}
	var err error
	out.states, err = analyticsResidenceStates(ctx, q, scope)
	if err != nil {
		return out, err
	}
	rows, err := queryAnalyticsPopulation(ctx, q, "residence_issues", `SELECT native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND julianday(i.created_at)<julianday(?) AND (coalesce(ws.terminal,0)=0 OR julianday(i.updated_at)>=julianday(?)) ORDER BY native_id LIMIT ? OFFSET ?`, scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.clipped = analyticsPopulationOffset(ctx, "residence_issues") > 0 || analyticsPopulationOffset(ctx, "residence_events") > 0
	for rows.Next() {
		if len(out.ids) == maxAnalyticsPopulation {
			out.clipped = true
			break
		}
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out.ids = append(out.ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	ids, err := json.Marshal(out.ids)
	if err != nil {
		return out, err
	}
	type point struct{ NS, Hour int64 }
	var times []point
	seen := map[int64]bool{}
	for _, at := range points {
		if !seen[at.UnixNano()] {
			times = append(times, point{at.UnixNano(), at.Truncate(time.Hour).Unix()})
			seen[at.UnixNano()] = true
		}
		out.samples[at.UnixNano()] = map[string]analyticsResidenceSample{}
	}
	encoded, err := json.Marshal(times)
	if err != nil {
		return out, err
	}
	result, err := q.QueryContext(ctx, `SELECT json_extract(p.value,'$.NS'),coalesce(
 (SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND hour=json_extract(p.value,'$.Hour') AND json_extract(data_json,'$.ID')=i.value AND json_extract(data_json,'$.AtNS')<json_extract(p.value,'$.NS') ORDER BY json_extract(data_json,'$.AtNS') DESC,value DESC LIMIT 1),
 (SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=i.value AND hour<json_extract(p.value,'$.Hour') ORDER BY hour DESC LIMIT 1),'null')
 FROM json_each(?) i CROSS JOIN json_each(?) p`, scope.organization, scope.project, scope.organization, scope.project, string(ids), string(encoded))
	if err != nil {
		return out, err
	}
	defer result.Close()
	for result.Next() {
		var ns int64
		var raw string
		if err := result.Scan(&ns, &raw); err != nil {
			return out, err
		}
		if raw == "null" {
			continue
		}
		var sample analyticsResidenceSample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			return out, err
		}
		samples := out.samples[ns]
		if samples == nil {
			samples = map[string]analyticsResidenceSample{}
			out.samples[ns] = samples
		}
		samples[sample.ID] = sample
	}
	if err := errors.Join(result.Err(), result.Close()); err != nil {
		return out, err
	}
	remaining := maxAnalyticsPopulation
	for _, id := range out.ids {
		end := out.samples[w.To.UnixNano()][id]
		allowed := min(remaining, end.Events)
		remaining -= allowed
		if end.Events <= allowed {
			continue
		}
		out.clipped = true
		if page := analyticsPagingFromContext(ctx); page != nil {
			page.next["residence_events"] = "residence_events:1000:" + strconv.Itoa(analyticsPopulationOffset(ctx, "residence_issues"))
		}
		var raw string
		if err := q.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND json_extract(data_json,'$.ID')=? AND value=? ORDER BY json_extract(data_json,'$.AtNS'),json_extract(data_json,'$.Sequence') LIMIT 1`, scope.organization, scope.project, id, allowed).Scan(&raw); err != nil {
			return out, err
		}
		var cutoff analyticsResidenceSample
		if err := json.Unmarshal([]byte(raw), &cutoff); err != nil {
			return out, err
		}
		for ns, samples := range out.samples {
			sample, exists := samples[id]
			if !exists {
				continue
			}
			if sample.Events > allowed {
				sample = cutoff
			}
			sample.Partial = true
			samples[id] = sample
			out.samples[ns] = samples
		}
	}
	return out, nil
}

func summarizeResidenceFrames(frames analyticsResidenceFrames, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	causes := map[string]operatortool.AnalyticsReworkCause{}
	var system, held, queue, lead []float64
	for _, id := range frames.ids {
		end := frames.samples[to.UnixNano()][id]
		begin := frames.samples[from.UnixNano()][id]
		item := residenceSampleItem(end, frames.states, to)
		item.WorkItemID = id
		endLanes, endQueue := residenceSampleNanoseconds(end, frames.states, to)
		beginLanes, beginQueue := residenceSampleNanoseconds(begin, frames.states, from)
		item.SystemSeconds = 0
		item.HeldSeconds = 0
		item.QueueSeconds = float64(max(int64(0), endQueue-beginQueue)) / 1e9
		for lane := range item.LaneSeconds {
			seconds := float64(endLanes[lane]-beginLanes[lane]) / 1e9
			if seconds <= 0 {
				delete(item.LaneSeconds, lane)
				continue
			}
			item.LaneSeconds[lane] = seconds
			lanes[lane] = append(lanes[lane], seconds)
			if analyticsResidenceGroup(lane, frames.states) == "held" {
				item.HeldSeconds += seconds
			} else {
				item.SystemSeconds += seconds
			}
		}
		if end.LastDone != nil && !end.LastDone.Before(from) && end.LastDone.Before(to) {
			seconds := end.LastDone.Sub(end.Created).Seconds()
			item.LeadSeconds = &seconds
			lead = append(lead, seconds)
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
		for key, cause := range end.Causes {
			cause.Count -= begin.Causes[key].Count
			if cause.Count > 0 {
				aggregate := causes[key]
				aggregate.FromState, aggregate.ReasonDetail = cause.FromState, cause.ReasonDetail
				aggregate.Count += cause.Count
				causes[key] = aggregate
			}
		}
		items = append(items, item)
	}
	for lane, values := range lanes {
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, frames.states), AnalyticsDuration: analyticsDuration(values)})
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
	summary.SystemTotal, summary.HeldTotal, summary.QueueTotal, summary.LeadTotal = analyticsDuration(system), analyticsDuration(held), analyticsDuration(queue), analyticsDuration(lead)
	return summary, items
}

func projectAnalyticsResidenceRollups(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, out *nativeAnalyticsProject) error {
	points := []time.Time{out.Window.From, out.Window.To}
	for _, b := range out.Digest {
		points = append(points, b.From, b.To)
	}
	frames, err := readAnalyticsResidenceFrames(ctx, q, scope, out.Window, points)
	if err != nil {
		return err
	}
	residence := operatortool.AnalyticsResidence{Source: "native_workflow_transitions_and_issue_creation", Coverage: "per_issue; residence_clipped_to_window; system_excludes_intake_terminal_and_held; held_after_first_dispatch; lead_created_to_done_at_completion; aging_at_window_end; quantiles_linear_interpolation", IssuesObserved: len(frames.ids)}
	var items []operatortool.AnalyticsIssueResidence
	residence.AnalyticsResidenceSummary, items = summarizeResidenceFrames(frames, out.Window.From, out.Window.To)
	available := false
	aging := []operatortool.AnalyticsAging{}
	for _, id := range frames.ids {
		sample := frames.samples[out.Window.To.UnixNano()][id]
		residence.EventsObserved += sample.Events
		available = available || sample.Initial != ""
		out.SourceAt = maxTime(out.SourceAt, maxTime(sample.Created, sample.At))
		frames.clipped = frames.clipped || sample.Partial
		if sample.Partial || frames.states[sample.State].Terminal || !sample.Entered.Before(out.Window.To) {
			continue
		}
		item := operatortool.AnalyticsAging{WorkItemID: id, Lane: sample.State, EnteredAt: sample.Entered, Hours: out.Window.To.Sub(sample.Entered).Hours()}
		for _, lane := range residence.Lanes {
			if lane.Lane == item.Lane {
				hours := lane.P90Seconds / 3600
				item.LaneP90Hours = &hours
				break
			}
		}
		aging = append(aging, item)
	}
	residence.Partial = frames.clipped
	residence.Issues = operatortool.OffsetPage(items, r.RowOffset, r.Limit)
	residence.Aging = operatortool.OffsetPage(aging, r.RowOffset, r.Limit)
	out.LaneResidence = residence
	out.Partial = out.Partial || frames.clipped
	for i := range out.Digest {
		b := &out.Digest[i]
		b.LaneResidence, _ = summarizeResidenceFrames(frames, b.From, b.To)
		b.LaneResidence.Partial = frames.clipped
		b.QueueSeconds = b.LaneResidence.QueueTotal.Seconds
	}
	out.QueueTime.Source = residence.Source
	out.QueueTime.Coverage = "todo_residence_and_rework_before_claim; includes_unclaimed_waits; clipped_to_window"
	out.QueueTime.Seconds = residence.QueueTotal.Seconds
	out.QueueTime.Duration = residence.QueueTotal
	out.QueueTime.Partial = frames.clipped
	if !available {
		out.QueueTime.Partial = true
		out.Unavailable = append(out.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "queue_time")
	} else if frames.clipped {
		out.Unavailable = append(out.Unavailable, "lane_residence_complete_population", "queue_time_complete_population")
	}
	return projectResidencePipelineRollups(ctx, q, scope, frames, out)
}

func projectResidencePipelineRollups(ctx context.Context, q nativeQueryer, scope nativeScope, frames analyticsResidenceFrames, out *nativeAnalyticsProject) error {
	ids, err := json.Marshal(frames.ids)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_merge' AND json_extract(data_json,'$.ID') IN (SELECT value FROM json_each(?))`, scope.organization, scope.project, string(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var m analyticsMergeSample
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		sample := frames.samples[out.Window.To.UnixNano()][m.ID]
		if sample.Partial || !m.From.Before(out.Window.To) {
			continue
		}
		end := out.Window.To
		if !m.To.IsZero() {
			end = minTime(end, m.To)
		}
		timeline := analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: m.ID}, spans: []analyticsResidenceSpan{{lane: "Merging", from: m.From, to: end}}}
		if m.Claim != nil && m.Claim.Before(out.Window.To) {
			timeline.issue.events = []analyticsResidenceEvent{{at: *m.Claim, kind: "scheduler.decision"}}
		}
		out.pipelineTimings = append(out.pipelineTimings, residencePipelineTimings(timeline, out.allAttempts)...)
	}
	return rows.Err()
}

func readDiagnosticsResidenceRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) (int, bool, error) {
	points := []time.Time{out.From, out.To}
	for _, b := range out.Capacity {
		points = append(points, minTime(b.Hour.Add(time.Hour), out.To))
	}
	frames, err := readAnalyticsResidenceFrames(ctx, q, scope, operatortool.AnalyticsWindow{From: out.From, To: out.To}, points)
	if err != nil {
		return 0, false, err
	}
	transitions := 0
	for _, id := range frames.ids {
		transitions += frames.samples[out.To.UnixNano()][id].Transitions - frames.samples[out.From.UnixNano()][id].Transitions
	}
	for i := range out.Capacity {
		at := minTime(out.Capacity[i].Hour.Add(time.Hour), out.To)
		count := 0
		partial := frames.clipped
		for _, id := range frames.ids {
			sample, ok := frames.samples[at.UnixNano()][id]
			if !ok {
				continue
			}
			partial = partial || sample.Partial
			if sample.State == "Todo" {
				count++
			}
		}
		if partial {
			out.Capacity[i].Todo = nil
		} else if out.Capacity[i].Todo != nil {
			*out.Capacity[i].Todo += count
		}
	}
	return transitions, frames.clipped, nil
}

func readReportsCompletionRollups(ctx context.Context, q nativeQueryer, scope nativeScope, states map[string]tracker.NativeState, ids []string, to time.Time) (reportsCompletion, error) {
	out := reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	var system, lead, working []float64
	lanes := map[string][]float64{}
	err := forShippedBatches(ids, func(batch string) error {
		rows, err := q.QueryContext(ctx, `SELECT (SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='completion' AND json_extract(data_json,'$.ID')=i.value AND json_extract(data_json,'$.AtNS')<? ORDER BY json_extract(data_json,'$.AtNS') DESC,value DESC LIMIT 1) FROM json_each(?) i`, scope.organization, scope.project, to.UnixNano(), batch)
		if err != nil {
			return fmt.Errorf("read completion rollups: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var raw sql.NullString
			if err := rows.Scan(&raw); err != nil {
				return fmt.Errorf("read completion rollups: %w", err)
			}
			if !raw.Valid {
				out.Partial = true
				continue
			}
			var sample analyticsResidenceSample
			if err := json.Unmarshal([]byte(raw.String), &sample); err != nil {
				return fmt.Errorf("decode completion rollup: %w", err)
			}
			if sample.Partial || sample.Started == nil {
				out.Partial = true
				continue
			}
			item := residenceSampleItem(sample, states, sample.At)
			system = append(system, item.SystemSeconds)
			lead = append(lead, sample.At.Sub(sample.Created).Seconds())
			working = append(working, item.LaneSeconds["In Progress"])
			for lane, seconds := range item.LaneSeconds {
				lanes[lane] = append(lanes[lane], seconds)
			}
		}
		return rows.Err()
	})
	return reportsCompletionDurations(out, states, system, lead, working, lanes), err
}

func readShippedResidenceIssues(ctx context.Context, q nativeQueryer, scope nativeScope, ids []string, to time.Time) ([]analyticsResidenceIssue, error) {
	issues := []analyticsResidenceIssue{}
	err := forShippedBatches(ids, func(batch string) error {
		rows, err := q.QueryContext(ctx, `SELECT i.native_id,i.created_at,coalesce(
(SELECT json_extract(e.data_json,'$.from_state') FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned'
 ORDER BY e.sequence LIMIT 1),CASE WHEN EXISTS (
 SELECT 1 FROM collaboration_events c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.type='issue.created'
 ) THEN ws.detent_state END,'')
FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=? AND i.native_id IN (SELECT value FROM json_each(?))
ORDER BY i.native_id`, scope.organization, scope.project, batch)
		if err != nil {
			return fmt.Errorf("read shipped issues: %w", err)
		}
		defer rows.Close()
		first := len(issues)
		byID := map[string]int{}
		for rows.Next() {
			var issue analyticsResidenceIssue
			var created string
			if err := rows.Scan(&issue.id, &created, &issue.initial); err != nil {
				return fmt.Errorf("read shipped issues: %w", err)
			}
			if issue.created, err = parseTimeValue(created); err != nil {
				return fmt.Errorf("decode shipped issue creation: %w", err)
			}
			byID[issue.id] = len(issues)
			issues = append(issues, issue)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("read shipped issues: %w", err)
		}
		if len(issues) == first {
			return nil
		}
		events, err := q.QueryContext(ctx, `SELECT work_item_id,type,recorded_at,data_json FROM collaboration_events
WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<julianday(?)
AND work_item_id IN (SELECT value FROM json_each(?))
AND (type='workflow.transitioned' OR (type='scheduler.decision' AND json_extract(data_json,'$.decision.source')='native_claim' AND json_extract(data_json,'$.decision.outcome')='claimed'))
ORDER BY work_item_id,sequence`, scope.organization, scope.project, formatHubTime(to), batch)
		if err != nil {
			return fmt.Errorf("read shipped transitions: %w", err)
		}
		defer events.Close()
		for events.Next() {
			var id, at, raw string
			var event analyticsResidenceEvent
			if err := events.Scan(&id, &event.kind, &at, &raw); err != nil {
				return fmt.Errorf("read shipped transitions: %w", err)
			}
			if event.at, err = parseTimeValue(at); err != nil {
				return fmt.Errorf("decode shipped transition time: %w", err)
			}
			if err := json.Unmarshal([]byte(raw), &event.data); err != nil {
				return fmt.Errorf("decode shipped transition: %w", err)
			}
			if i, ok := byID[id]; ok {
				issues[i].events = append(issues[i].events, event)
			}
		}
		return events.Err()
	})
	return issues, err
}

const shippedBatchSize = 500

func forShippedBatches(ids []string, read func(string) error) error {
	for batch := range slices.Chunk(ids, shippedBatchSize) {
		encoded, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("encode shipped issues: %w", err)
		}
		if err := read(string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

func residenceSampleNanoseconds(sample analyticsResidenceSample, states map[string]tracker.NativeState, at time.Time) (map[string]int64, int64) {
	lanes := map[string]int64{}
	for lane, ns := range sample.LanesNS {
		lanes[lane] = ns
	}
	queue := sample.QueueNS
	if sample.Started != nil && !sample.Partial {
		from := maxTime(sample.Entered, *sample.Started)
		if analyticsResidenceGroup(sample.State, states) != "excluded" {
			lanes[sample.State] += max(int64(0), at.Sub(from).Nanoseconds())
		}
		if sample.State == "Todo" || sample.State == "Rework" {
			to := at
			if sample.QueueTo != nil {
				to = minTime(to, *sample.QueueTo)
			}
			queue += max(int64(0), to.Sub(from).Nanoseconds())
		}
	}
	return lanes, queue
}
