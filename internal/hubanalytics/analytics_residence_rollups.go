package hubanalytics

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *Store) ProjectResidenceRollups(ctx context.Context, q hubsql.Queryer, scope Scope, r operatortool.AnalyticsRequest, out *Project) error {
	points := []time.Time{out.Window.From, out.Window.To}
	for _, b := range out.Digest {
		points = append(points, b.From, b.To)
	}
	frames, err := d.readResidenceFrames(ctx, q, scope, out.Window, points)
	if err != nil {
		return err
	}
	residence := operatortool.AnalyticsResidence{Source: "native_workflow_transitions_and_issue_creation", Coverage: "per_issue; residence_clipped_to_window; system_excludes_intake_terminal_and_held; held_after_first_dispatch; lead_created_to_done_at_completion; aging_at_window_end; quantiles_linear_interpolation", IssuesObserved: len(frames.IDs)}
	var items []operatortool.AnalyticsIssueResidence
	residence.AnalyticsResidenceSummary, items = SummarizeResidenceFrames(frames, out.Window.From, out.Window.To)
	available := false
	aging := []operatortool.AnalyticsAging{}
	for _, id := range frames.IDs {
		sample := frames.At(out.Window.To, id)
		residence.EventsObserved += sample.Events
		available = available || sample.Initial != ""
		out.SourceAt = maxTime(out.SourceAt, maxTime(sample.Created, sample.At))
		frames.Clipped = frames.Clipped || sample.Partial
		if sample.Partial || frames.States[sample.State].Terminal || !sample.Entered.Before(out.Window.To) {
			continue
		}
		item := operatortool.AnalyticsAging{WorkItemID: id, Lane: sample.State, EnteredAt: sample.Entered, Hours: out.Window.To.Sub(sample.Entered).Hours()}
		if sample.Started != nil {
			since := out.Window.To.Sub(*sample.Started).Hours()
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
	residence.Partial = frames.Clipped
	residence.Issues = operatortool.OffsetPage(items, r.RowOffset, r.Limit)
	residence.Aging = operatortool.OffsetPage(aging, r.RowOffset, r.Limit)
	out.LaneResidence = residence
	out.Partial = out.Partial || frames.Clipped
	for i := range out.Digest {
		b := &out.Digest[i]
		b.LaneResidence, _ = SummarizeResidenceFrames(frames, b.From, b.To)
		b.LaneResidence.Partial = frames.Clipped
		b.QueueSeconds = b.LaneResidence.QueueTotal.Seconds
	}
	out.QueueTime.Source = residence.Source
	out.QueueTime.Coverage = "todo_residence_and_rework_before_claim; includes_unclaimed_waits; clipped_to_window"
	out.QueueTime.Seconds = residence.QueueTotal.Seconds
	out.QueueTime.Duration = residence.QueueTotal
	out.QueueTime.Partial = frames.Clipped
	if !available {
		out.QueueTime.Partial = true
		out.Unavailable = append(out.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "queue_time")
	} else if frames.Clipped {
		out.Unavailable = append(out.Unavailable, "lane_residence_complete_population", "queue_time_complete_population")
	}
	if err := d.addResidenceHours(ctx, q, scope, out); err != nil {
		return err
	}
	return d.ProjectResidencePipelineRollups(ctx, q, scope, frames, out)
}

func (d *Store) ProjectResidencePipelineRollups(ctx context.Context, q hubsql.Queryer, scope Scope, frames ResidenceFrames, out *Project) error {
	ids, err := hubsql.List(frames.IDs)
	if err != nil {
		return err
	}
	rows, err := hubsql.Query(ctx, q, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_merge' AND sample_id IN (SELECT value FROM sql_rows(?))`, scope.Organization, scope.Project, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var m MergeSample
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		sample := frames.At(out.Window.To, m.ID)
		if sample.Partial || !m.From.Before(out.Window.To) {
			continue
		}
		end := out.Window.To
		if !m.To.IsZero() {
			end = minTime(end, m.To)
		}
		timeline := ResidenceTimeline{Issue: ResidenceIssue{ID: m.ID}, Spans: []ResidenceSpan{{Lane: "Merging", From: m.From, To: end}}}
		if m.Claim != nil && m.Claim.Before(out.Window.To) {
			timeline.Issue.Events = []ResidenceEvent{{At: *m.Claim, Kind: "scheduler.decision"}}
		}
		out.PipelineTimings = append(out.PipelineTimings, ResidencePipelineTimings(timeline, out.AllAttempts)...)
	}
	return rows.Err()
}

func (d *Store) ReadDiagnosticsResidenceRollups(ctx context.Context, q hubsql.Queryer, scope Scope, out *DiagnosticsView) (int, bool, error) {
	points := []time.Time{out.From, out.To}
	for _, b := range out.Capacity {
		points = append(points, minTime(b.Hour.Add(time.Hour), out.To))
	}
	frames, err := d.readResidenceFrames(ctx, q, scope, operatortool.AnalyticsWindow{From: out.From, To: out.To}, points)
	if err != nil {
		return 0, false, err
	}
	hours, err := d.ReadDiagnosticsResidenceHours(ctx, q, scope, out.From, out.To)
	if err != nil {
		return 0, false, err
	}
	transitions := 0
	partial := frames.Clipped
	for _, id := range frames.IDs {
		from := maxTime(out.From, frames.Retained[id])
		if from.Before(out.To) {
			transitions += frames.At(out.To, id).Transitions - frames.At(from, id).Transitions
		}
	}
	for hour, h := range hours {
		transitions += h.transitions
		partial = partial || h.partial || hour < out.From.Unix() || hour+3600 > out.To.Unix()
	}
	for i := range out.Capacity {
		at := minTime(out.Capacity[i].Hour.Add(time.Hour), out.To)
		count := 0
		depthPartial := frames.Clipped
		for _, id := range frames.IDs {
			retained := frames.Retained[id]
			if !retained.IsZero() && !at.After(retained) {
				depthPartial = depthPartial || !at.Equal(at.Truncate(time.Hour))
				continue
			}
			sample, ok := frames.Sample(at, id)
			if !ok {
				continue
			}
			depthPartial = depthPartial || sample.Partial
			if sample.State == "Todo" {
				count++
			}
		}
		if at.Equal(at.Truncate(time.Hour)) {
			if h, found := hours[at.Unix()-3600]; found {
				depthPartial = depthPartial || h.partial || h.todo == nil
				if h.todo != nil {
					count += *h.todo
				}
			}
		}
		if depthPartial {
			out.Capacity[i].Todo = nil
		} else if out.Capacity[i].Todo != nil {
			*out.Capacity[i].Todo += count
		}
		partial = partial || depthPartial
	}
	return transitions, partial, nil
}

func (d *Store) ReadReportsCompletionRollups(ctx context.Context, q hubsql.Queryer, scope Scope, states map[string]tracker.NativeState, ids []string, to time.Time) (Completion, error) {
	out := Completion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	var system, lead, working []float64
	lanes := map[string][]float64{}
	err := ForShippedBatches(ids, func(batch hubsql.ParameterRows) error {
		rows, err := hubsql.Query(ctx, q, `SELECT (SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='completion' AND sample_id=i.value AND sample_ns<? ORDER BY sample_ns DESC,value DESC LIMIT 1) FROM sql_rows(?) i`, scope.Organization, scope.Project, to.UnixNano(), batch)
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
			var sample ResidenceSample
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
	return CompletionDurations(out, states, system, lead, working, lanes), err
}

func (d *Store) ReadShippedResidenceIssues(ctx context.Context, q hubsql.Queryer, scope Scope, ids []string, to time.Time) ([]ResidenceIssue, error) {
	issues := []ResidenceIssue{}
	err := ForShippedBatches(ids, func(batch hubsql.ParameterRows) error {
		rows, err := hubsql.Query(ctx, q, `SELECT i.native_id,i.created_at,coalesce(
(SELECT e.analytics_from_state FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned'
 ORDER BY e.sequence LIMIT 1),CASE WHEN EXISTS (
 SELECT 1 FROM collaboration_events c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.type='issue.created'
 ) THEN ws.detent_state END,''),
(SELECT CASE WHEN e.analytics_projection_version IS NULL THEN e.data_json END FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1)
FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=? AND i.native_id IN (SELECT value FROM sql_rows(?))
ORDER BY i.native_id`, scope.Organization, scope.Project, batch)
		if err != nil {
			return fmt.Errorf("read shipped issues: %w", err)
		}
		defer rows.Close()
		first := len(issues)
		byID := map[string]int{}
		for rows.Next() {
			var issue ResidenceIssue
			var created string
			var unknown sql.NullString
			if err := rows.Scan(&issue.ID, &created, &issue.Initial, &unknown); err != nil {
				return fmt.Errorf("read shipped issues: %w", err)
			}
			if unknown.Valid {
				var data struct {
					FromState *string `json:"from_state"`
				}
				if err := json.Unmarshal([]byte(unknown.String), &data); err != nil {
					return err
				}
				if data.FromState != nil {
					issue.Initial = *data.FromState
				}
			}
			if issue.Created, err = parseTimeValue(created); err != nil {
				return fmt.Errorf("decode shipped issue creation: %w", err)
			}
			byID[issue.ID] = len(issues)
			issues = append(issues, issue)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("read shipped issues: %w", err)
		}
		if len(issues) == first {
			return nil
		}
		events, err := hubsql.Query(ctx, q, `SELECT work_item_id,type,recorded_at,`+collaborationPayloadColumn("")+` FROM collaboration_events
WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<julianday(?)
AND work_item_id IN (SELECT value FROM sql_rows(?))
AND (type='workflow.transitioned' OR (type='scheduler.decision' AND (scheduler_outcome IS NULL OR (scheduler_source='native_claim' AND scheduler_outcome='claimed'))))
ORDER BY work_item_id,sequence`, scope.Organization, scope.Project, formatHubTime(to), batch)
		if err != nil {
			return fmt.Errorf("read shipped transitions: %w", err)
		}
		defer events.Close()
		type record struct {
			id, at, raw string
			event       ResidenceEvent
		}
		var records []record
		for events.Next() {
			var r record
			if err := events.Scan(&r.id, &r.event.Kind, &r.at, &r.raw); err != nil {
				return fmt.Errorf("read shipped transitions: %w", err)
			}
			records = append(records, r)
		}
		if err := errors.Join(events.Err(), events.Close()); err != nil {
			return err
		}
		for _, r := range records {
			id, event := r.id, r.event
			if event.At, err = parseTimeValue(r.at); err != nil {
				return err
			}
			if err := d.hooks.DecodeRunHistory(ctx, q, r.raw, &event.Data); err != nil {
				return err
			}
			if event.Kind == "scheduler.decision" && (event.Data.Decision == nil || event.Data.Decision.Source != "native_claim" || event.Data.Decision.Outcome != "claimed") {
				continue
			}
			if i, ok := byID[id]; ok {
				issues[i].Events = append(issues[i].Events, event)
			}
		}
		return events.Err()
	})
	return issues, err
}

const shippedBatchSize = 500

func ForShippedBatches(ids []string, read func(hubsql.ParameterRows) error) error {
	for batch := range slices.Chunk(ids, shippedBatchSize) {
		encoded, err := hubsql.List(batch)
		if err != nil {
			return fmt.Errorf("encode shipped issues: %w", err)
		}
		if err := read(encoded); err != nil {
			return err
		}
	}
	return nil
}

type ResidenceSample struct {
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

func ResidenceStates(ctx context.Context, q hubsql.Queryer, scope Scope) (map[string]tracker.NativeState, error) {
	rows, err := q.QueryContext(ctx, `SELECT detent_state,terminal,dispatchable FROM workflow_states WHERE project_id=? LIMIT 50`, scope.Project)
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

func WriteResidenceSample(ctx context.Context, tx hubsql.Execer, scope Scope, metric, dimension string, sample ResidenceSample) error {
	sample.AtNS = sample.At.UnixNano()
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	err = writeAnalyticsSample(ctx, tx, string(scope.Organization), string(scope.Project), sample.At.Truncate(time.Hour).Unix(), metric, dimension, float64(sample.Events), string(raw))
	return err
}

func refreshResidence(ctx context.Context, tx ProjectionQuery, scope Scope, id string, after int64, before ...int64) error {
	states, err := ResidenceStates(ctx, tx, scope)
	if err != nil {
		return err
	}
	var created, initial string
	err = tx.QueryRowContext(ctx, `SELECT i.created_at,coalesce((SELECT analytics_from_state FROM collaboration_events WHERE organization_id=i.organization_id AND project_id=i.project_id AND work_item_id=i.native_id AND type='workflow.transitioned' ORDER BY sequence LIMIT 1),CASE WHEN EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=i.organization_id AND work_item_id=i.native_id AND type='issue.created') THEN ws.detent_state END,'') FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, scope.Organization, scope.Project, id).Scan(&created, &initial)
	if err != nil {
		return err
	}
	start, err := parseTimeValue(created)
	if err != nil {
		return err
	}
	sample := ResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
	var previous string
	if len(before) == 0 {
		err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.Organization, scope.Project, id).Scan(&previous)
	} else {
		var prior int64
		err = tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed'))`, scope.Organization, scope.Project, id, after).Scan(&prior)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, scope.Organization, scope.Project, id+":"+strconv.FormatInt(prior, 10)).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.Organization, scope.Project, id).Scan(&previous)
			if err == nil {
				var retained ResidenceSample
				if err := json.Unmarshal([]byte(previous), &retained); err != nil {
					return err
				}
				if retained.Sequence > after {
					err = sql.ErrNoRows
				}
			}
		}
	}
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return err
	}
	if !fresh {
		if err := json.Unmarshal([]byte(previous), &sample); err != nil {
			return err
		}
		fresh = sample.Initial != initial
		if fresh && len(before) > 0 {
			return errAnalyticsResidenceReset
		}
	}
	if fresh {
		after = 0
		sample = ResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
		if states[initial].Dispatchable && ResidenceGroup(initial, states) == "system" {
			sample.Started = &start
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry') AND sample_id=?`, scope.Organization, scope.Project, id); err != nil {
			return err
		}
	}
	write := func(sequence int64) error {
		if err := WriteResidenceSample(ctx, tx, scope, "residence_hour", id, sample); err != nil {
			return err
		}
		return WriteResidenceSample(ctx, tx, scope, "residence_edge", id+":"+strconv.FormatInt(sequence, 10), sample)
	}
	if fresh {
		if err := write(0); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,type,recorded_at,`+collaborationPayloadColumn("")+` FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed')) ORDER BY sequence`, scope.Organization, scope.Project, id, max(sample.Sequence, after), projectionUpper(before))
	if err != nil {
		return err
	}
	defer rows.Close()
	type event struct {
		sequence int64
		ResidenceEvent
	}
	var events []event
	for rows.Next() {
		var e event
		var at, raw string
		if err := rows.Scan(&e.sequence, &e.Kind, &at, &raw); err != nil {
			return err
		}
		e.At, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		if e.Kind == "workflow.transitioned" {
			if err := json.Unmarshal([]byte(raw), &e.Data); err != nil {
				return err
			}
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
		raw, err := json.Marshal(MergeSample{FromNS: maxTime(sample.Entered, *sample.Started).UnixNano(), ID: id, From: maxTime(sample.Entered, *sample.Started), To: to, Claim: mergeClaim})
		if err != nil {
			return err
		}
		err = writeAnalyticsSample(ctx, tx, string(scope.Organization), string(scope.Project), sample.Entered.Truncate(time.Hour).Unix(), "residence_merge", id+":"+strconv.FormatInt(sample.Entered.UnixNano(), 10), 1, string(raw))
		return err
	}
	for _, e := range events {
		sample.Sequence = e.sequence
		sample.Events++
		if e.Kind == "workflow.transitioned" {
			sample.Transitions++
			if e.Data.ToState == "Merging" {
				at := sample.At
				sample.At = e.At
				if err := WriteResidenceSample(ctx, tx, scope, "merge_entry", id+":"+strconv.FormatInt(e.sequence, 10), sample); err != nil {
					return err
				}
				sample.At = at
			}
		}
		if e.At.Before(sample.At) {
			sample.Partial = true
			if err := write(e.sequence); err != nil {
				return err
			}
			continue
		}
		sample.At = e.At
		if e.Kind == "scheduler.decision" {
			if sample.State == "Rework" && sample.QueueTo == nil {
				at := e.At
				sample.QueueTo = &at
			}
			if sample.State == "Merging" && mergeClaim == nil {
				at := e.At
				mergeClaim = &at
			}
		} else {
			if e.Data.FromState != sample.State || e.Data.ToState == "" {
				sample.Partial = true
			} else if e.Data.ToState != sample.State && sample.Started != nil {
				from := maxTime(sample.Entered, *sample.Started)
				if ResidenceGroup(sample.State, states) != "excluded" {
					sample.Lanes[sample.State] += max(0, e.At.Sub(from).Seconds())
					sample.LanesNS[sample.State] += max(int64(0), e.At.Sub(from).Nanoseconds())
				}
				if sample.State == "Todo" || sample.State == "Rework" {
					to := e.At
					if sample.QueueTo != nil {
						to = minTime(to, *sample.QueueTo)
					}
					sample.Queue += max(0, to.Sub(from).Seconds())
					sample.QueueNS += max(int64(0), to.Sub(from).Nanoseconds())
				}
				if sample.State == "Merging" {
					if err := writeMerge(e.At); err != nil {
						return err
					}
				}
			}
			if e.Data.ToState != "" && e.Data.ToState != sample.State {
				sample.State, sample.Entered = e.Data.ToState, e.At
				sample.QueueTo = nil
				mergeClaim = nil
				if sample.Started == nil && states[sample.State].Dispatchable && ResidenceGroup(sample.State, states) == "system" {
					at := e.At
					sample.Started = &at
				}
				if sample.State == "Done" {
					at := e.At
					sample.LastDone = &at
					if err := WriteResidenceSample(ctx, tx, scope, "completion", id+":"+strconv.FormatInt(e.sequence, 10), sample); err != nil {
						return err
					}
				}
				if sample.State == "Rework" {
					key := e.Data.FromState + "\x00" + e.Data.ReasonDetail
					cause := sample.Causes[key]
					cause.FromState, cause.ReasonDetail = e.Data.FromState, e.Data.ReasonDetail
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

type MergeSample struct {
	FromNS int64

	ID       string
	From, To time.Time
	Claim    *time.Time
}

func residenceSampleItem(sample ResidenceSample, states map[string]tracker.NativeState, at time.Time) operatortool.AnalyticsIssueResidence {
	item := operatortool.AnalyticsIssueResidence{WorkItemID: sample.ID, SystemStartedAt: sample.Started, LaneSeconds: map[string]float64{}, QueueSeconds: sample.Queue, Partial: sample.Partial}
	for lane, seconds := range sample.Lanes {
		item.LaneSeconds[lane] = seconds
	}
	if sample.Started != nil && !sample.Partial {
		from := maxTime(sample.Entered, *sample.Started)
		if ResidenceGroup(sample.State, states) != "excluded" {
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
		if ResidenceGroup(lane, states) == "held" {
			item.HeldSeconds += seconds
		} else {
			item.SystemSeconds += seconds
		}
	}
	return item
}

// analyticsResidenceFrames holds each issue's residence sample at each of
// the requested points. An issue's sample at a point is its last edge earlier
// in the point's hour, or else its last hourly sample before that hour.
type ResidenceFrames struct {
	Retained map[string]time.Time
	States   map[string]tracker.NativeState
	IDs      []string
	points   map[int64]int
	issues   map[string]int
	samples  []*ResidenceSample
	cutoffs  map[string]residenceCutoff
	Clipped  bool
}

// residenceRow is one residence rollup row; edges sort by hour, AtNS and
// event count, hourly samples by hour. Its sample is decoded only when a
// point selects it.
type residenceRow struct {
	hour, atNS int64
	value      float64
	raw        string
	sample     *ResidenceSample
}

func (r *residenceRow) decode() (*ResidenceSample, error) {
	if r.sample == nil {
		r.sample = &ResidenceSample{}
		if err := json.Unmarshal([]byte(r.raw), r.sample); err != nil {
			return nil, err
		}
	}
	return r.sample, nil
}

// residenceCutoff replaces the samples of an issue whose events exceed the
// population budget with the sample at the last allowed event.
type residenceCutoff struct {
	allowed int
	sample  ResidenceSample
}

// sample is the issue's residence sample at one of the frame points. A time
// that is not a point, or an issue with no sample yet, has none.
func (f ResidenceFrames) Sample(at time.Time, id string) (ResidenceSample, bool) {
	point, ok := f.points[at.UnixNano()]
	issue, known := f.issues[id]
	if !ok || !known {
		return ResidenceSample{}, false
	}
	found := f.samples[point*len(f.IDs)+issue]
	if found == nil {
		return ResidenceSample{}, false
	}
	sample := *found
	if cutoff, clipped := f.cutoffs[id]; clipped {
		if sample.Events > cutoff.allowed {
			sample = cutoff.sample
		}
		sample.Partial = true
	}
	return sample, true
}

func (f ResidenceFrames) At(at time.Time, id string) ResidenceSample {
	sample, _ := f.Sample(at, id)
	return sample
}

func ReadResidenceFrames(ctx context.Context, q hubsql.Queryer, scope Scope, w operatortool.AnalyticsWindow, points []time.Time, issues string, issueArgs []any) (ResidenceFrames, error) {
	out := ResidenceFrames{points: map[int64]int{}, issues: map[string]int{}, cutoffs: map[string]residenceCutoff{}}
	var err error
	out.States, err = ResidenceStates(ctx, q, scope)
	if err != nil {
		return out, err
	}
	out.Retained = map[string]time.Time{}
	retained, err := hubsql.Query(ctx, q, `SELECT work_item_id,through_hour FROM analytics_residence_retention WHERE organization_id=? AND project_id=? AND through_hour>?`, scope.Organization, scope.Project, w.From.Unix())
	if err != nil {
		return out, err
	}
	defer retained.Close()
	for retained.Next() {
		var id string
		var hour int64
		if err := retained.Scan(&id, &hour); err != nil {
			return out, err
		}
		at := time.Unix(hour, 0).UTC()
		out.Retained[id] = at
		if at.Before(w.To) {
			points = append(points, at)
		}
	}
	if err := errors.Join(retained.Err(), retained.Close()); err != nil {
		return out, err
	}
	rows, err := QueryAnalyticsPopulation(ctx, q, "residence_issues", `SELECT native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND julianday(i.created_at)<julianday(?) AND (coalesce(ws.terminal,0)=0 OR julianday(i.updated_at)>=julianday(?))`+issues+` ORDER BY native_id LIMIT ? OFFSET ?`, append([]any{scope.Organization, scope.Project, formatHubTime(w.To), formatHubTime(w.From)}, issueArgs...)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Clipped = PopulationOffset(ctx, "residence_issues") > 0 || PopulationOffset(ctx, "residence_events") > 0
	for rows.Next() {
		if len(out.IDs) == MaxPopulation {
			out.Clipped = true
			break
		}
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out.IDs = append(out.IDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(points) == 0 || len(out.IDs) == 0 {
		return out, nil
	}
	instants := make([]int64, 0, len(points))
	for _, at := range points {
		instants = append(instants, at.UnixNano())
	}
	slices.Sort(instants)
	instants = slices.Compact(instants)
	for i, ns := range instants {
		out.points[ns] = i
	}
	firstHour, lastHour := residenceHour(instants[0]), residenceHour(instants[len(instants)-1])
	for i, id := range out.IDs {
		out.issues[id] = i
	}
	edges, hours := map[string][]residenceRow{}, map[string][]residenceRow{}
	baselines := map[string]ResidenceSample{}
	ids, err := hubsql.List(out.IDs)
	if err != nil {
		return out, err
	}
	read := func(rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var metric, dimension, id string
			var row residenceRow
			if err := rows.Scan(&metric, &dimension, &id, &row.hour, &row.atNS, &row.value, &row.raw); err != nil {
				return err
			}
			if metric == "residence_baseline" {
				sample, err := row.decode()
				if err != nil {
					return err
				}
				baselines[dimension] = *sample
			} else if _, wanted := out.issues[id]; wanted && metric == "residence_edge" {
				edges[id] = append(edges[id], row)
			} else if _, wanted := out.issues[dimension]; wanted && metric == "residence_hour" {
				hours[dimension] = append(hours[dimension], row)
			}
		}
		return rows.Err()
	}
	if err := read(hubsql.Query(ctx, q, `SELECT metric,dimension,'',hour,0,value,data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND hour>=? AND hour<?`,
		scope.Organization, scope.Project, firstHour, lastHour)); err != nil {
		return out, err
	}
	var edgeHours []int64
	for _, ns := range instants {
		if hour := residenceHour(ns); hour*int64(time.Second) != ns && !slices.Contains(edgeHours, hour) {
			edgeHours = append(edgeHours, hour)
		}
	}
	if len(edgeHours) > 0 {
		encoded, err := hubsql.List(edgeHours)
		if err != nil {
			return out, err
		}
		if err := read(hubsql.Query(ctx, q, `SELECT r.metric,r.dimension,coalesce(r.sample_id,''),r.hour,coalesce(r.sample_ns,0),r.value,r.data_json FROM sql_rows(?) h CROSS JOIN analytics_hourly_rollups r ON r.organization_id=? AND r.project_id=? AND r.metric='residence_edge' AND r.hour=h.value`,
			encoded, scope.Organization, scope.Project)); err != nil {
			return out, err
		}
	}
	if err := read(hubsql.Query(ctx, q, `SELECT r.metric,r.dimension,'',r.hour,0,r.value,r.data_json FROM sql_rows(?) i JOIN analytics_hourly_rollups r ON r.rowid=(SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=i.value AND hour<? ORDER BY hour DESC LIMIT 1)`,
		ids, scope.Organization, scope.Project, firstHour)); err != nil {
		return out, err
	}
	if err := read(hubsql.Query(ctx, q, `SELECT 'residence_baseline',p.work_item_id,'',r.hour,0,r.value,r.data_json
FROM sql_rows(?) i JOIN analytics_residence_retention p ON p.organization_id=? AND p.project_id=? AND p.work_item_id=i.value
JOIN analytics_hourly_rollups r ON r.rowid=(SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=p.organization_id AND project_id=p.project_id AND metric='residence_hour' AND dimension=p.work_item_id AND hour<p.through_hour ORDER BY hour DESC LIMIT 1)`,
		ids, scope.Organization, scope.Project)); err != nil {
		return out, err
	}
	out.samples = make([]*ResidenceSample, len(instants)*len(out.IDs))
	for issue, id := range out.IDs {
		if err := selectResidenceSamples(out.samples[issue:], len(out.IDs), instants, edges[id], hours[id]); err != nil {
			return out, err
		}
	}
	remaining := MaxPopulation
	var clipped []residenceClip
	for _, id := range out.IDs {
		end := out.At(w.To, id)
		baseline, archived := baselines[id]
		events := max(0, end.Events-baseline.Events)
		allowed := min(remaining, events)
		remaining -= allowed
		allowed += baseline.Events
		if end.Events <= allowed {
			continue
		}
		out.Clipped = true
		if page := analyticsPagingFromContext(ctx); page != nil {
			page.Next["residence_events"] = "residence_events:1000:" + strconv.Itoa(PopulationOffset(ctx, "residence_issues"))
		}
		if allowed == baseline.Events && archived {
			out.cutoffs[id] = residenceCutoff{allowed: allowed, sample: baseline}
			continue
		}
		clipped = append(clipped, residenceClip{ID: id, Allowed: allowed})
	}
	if len(clipped) == 0 {
		return out, nil
	}
	return out, readResidenceCutoffs(ctx, q, scope, clipped, out.cutoffs)
}

func residenceHour(ns int64) int64 {
	return time.Unix(0, ns).Truncate(time.Hour).Unix()
}

// selectResidenceSamples walks the ascending points and one issue's rows
// together and stores the sample each point selects at every stride-th
// entry of selected, decoding only the rows a point selects.
func selectResidenceSamples(selected []*ResidenceSample, stride int, instants []int64, edges, hours []residenceRow) error {
	slices.SortFunc(edges, func(a, b residenceRow) int {
		return cmp.Or(cmp.Compare(a.hour, b.hour), cmp.Compare(a.atNS, b.atNS), cmp.Compare(a.value, b.value))
	})
	slices.SortFunc(hours, func(a, b residenceRow) int { return cmp.Compare(a.hour, b.hour) })
	edge, hourly := 0, 0
	for point, ns := range instants {
		hour := residenceHour(ns)
		for edge < len(edges) && (edges[edge].hour < hour || edges[edge].hour == hour && edges[edge].atNS < ns) {
			edge++
		}
		for hourly < len(hours) && hours[hourly].hour < hour {
			hourly++
		}
		var row *residenceRow
		if edge > 0 && len(edges) >= edge && edges[edge-1].hour == hour {
			row = &edges[edge-1]
		} else if hourly > 0 && len(hours) >= hourly {
			row = &hours[hourly-1]
		}
		if row == nil {
			continue
		}
		sample, err := row.decode()
		if err != nil {
			return err
		}
		selected[point*stride] = sample
	}
	return nil
}

type residenceClip struct {
	ID      string `json:"id"`
	Allowed int    `json:"allowed"`
}

// readResidenceCutoffs reads, for each clipped issue and its allowed event
// count, the earliest edge at that count in one statement.
func readResidenceCutoffs(ctx context.Context, q hubsql.Queryer, scope Scope, clipped []residenceClip, cutoffs map[string]residenceCutoff) error {
	var tuples [][]any
	for _, clip := range clipped {
		tuples = append(tuples, []any{clip.ID, clip.Allowed})
	}
	encoded, err := hubsql.TypedRows([]string{"id", "allowed"}, tuples)
	if err != nil {
		return err
	}
	rows, err := hubsql.Query(ctx, q, `SELECT c.id,r.data_json FROM sql_rows(?) c CROSS JOIN analytics_hourly_rollups r ON r.organization_id=? AND r.project_id=? AND r.metric='residence_edge' AND r.sample_id=c.id AND r.value=c.allowed`, encoded, scope.Organization, scope.Project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var sample ResidenceSample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			return err
		}
		current, seen := cutoffs[id]
		if !seen || sample.AtNS < current.sample.AtNS || sample.AtNS == current.sample.AtNS && sample.Sequence < current.sample.Sequence {
			cutoffs[id] = residenceCutoff{sample: sample}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, clip := range clipped {
		cutoff, found := cutoffs[clip.ID]
		if !found {
			return sql.ErrNoRows
		}
		cutoff.allowed = clip.Allowed
		cutoffs[clip.ID] = cutoff
	}
	return nil
}

func SummarizeResidenceFrames(frames ResidenceFrames, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	causes := map[string]operatortool.AnalyticsReworkCause{}
	var system, held, queue, lead []float64
	for _, id := range frames.IDs {
		from := maxTime(from, frames.Retained[id])
		if !from.Before(to) {
			continue
		}
		end := frames.At(to, id)
		begin := frames.At(from, id)
		item := residenceSampleItem(end, frames.States, to)
		item.WorkItemID = id
		endLanes, endQueue := residenceSampleNanoseconds(end, frames.States, to)
		beginLanes, beginQueue := residenceSampleNanoseconds(begin, frames.States, from)
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
			if ResidenceGroup(lane, frames.States) == "held" {
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
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: ResidenceGroup(lane, frames.States), AnalyticsDuration: SampledDuration(values)})
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
	summary.SystemTotal, summary.HeldTotal, summary.QueueTotal, summary.LeadTotal = SampledDuration(system), SampledDuration(held), SampledDuration(queue), SampledDuration(lead)
	return summary, items
}

func residenceSampleNanoseconds(sample ResidenceSample, states map[string]tracker.NativeState, at time.Time) (map[string]int64, int64) {
	lanes := map[string]int64{}
	for lane, ns := range sample.LanesNS {
		lanes[lane] = ns
	}
	queue := sample.QueueNS
	if sample.Started != nil && !sample.Partial {
		from := maxTime(sample.Entered, *sample.Started)
		if ResidenceGroup(sample.State, states) != "excluded" {
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
