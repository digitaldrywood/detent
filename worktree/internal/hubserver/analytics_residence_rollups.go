package hubserver

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

func writeAnalyticsResidenceSample(ctx context.Context, tx nativeExecer, scope nativeScope, metric, dimension string, sample analyticsResidenceSample) error {
	sample.AtNS = sample.At.UnixNano()
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	err = writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), sample.At.Truncate(time.Hour).Unix(), metric, dimension, float64(sample.Events), string(raw))
	return err
}

func refreshAnalyticsResidence(ctx context.Context, tx projectionQuery, scope nativeScope, id string, after int64, before ...int64) error {
	states, err := analyticsResidenceStates(ctx, tx, scope)
	if err != nil {
		return err
	}
	var created, initial string
	err = tx.QueryRowContext(ctx, `SELECT i.created_at,coalesce((SELECT analytics_from_state FROM collaboration_events WHERE organization_id=i.organization_id AND project_id=i.project_id AND work_item_id=i.native_id AND type='workflow.transitioned' ORDER BY sequence LIMIT 1),CASE WHEN EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=i.organization_id AND work_item_id=i.native_id AND type='issue.created') THEN ws.detent_state END,'') FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, scope.organization, scope.project, id).Scan(&created, &initial)
	if err != nil {
		return err
	}
	start, err := parseTimeValue(created)
	if err != nil {
		return err
	}
	sample := analyticsResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
	var previous string
	if len(before) == 0 {
		err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&previous)
	} else {
		var prior int64
		err = tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed'))`, scope.organization, scope.project, id, after).Scan(&prior)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, scope.organization, scope.project, id+":"+strconv.FormatInt(prior, 10)).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&previous)
			if err == nil {
				var retained analyticsResidenceSample
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
		sample = analyticsResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
		if states[initial].Dispatchable && analyticsResidenceGroup(initial, states) == "system" {
			sample.Started = &start
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry') AND sample_id=?`, scope.organization, scope.project, id); err != nil {
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
	rows, err := tx.QueryContext(ctx, `SELECT sequence,type,recorded_at,`+collaborationPayloadColumn("")+` FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed')) ORDER BY sequence`, scope.organization, scope.project, id, max(sample.Sequence, after), projectionUpper(before))
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
		if e.kind == "workflow.transitioned" {
			if err := json.Unmarshal([]byte(raw), &e.data); err != nil {
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
		raw, err := json.Marshal(analyticsMergeSample{FromNS: maxTime(sample.Entered, *sample.Started).UnixNano(), ID: id, From: maxTime(sample.Entered, *sample.Started), To: to, Claim: mergeClaim})
		if err != nil {
			return err
		}
		err = writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), sample.Entered.Truncate(time.Hour).Unix(), "residence_merge", id+":"+strconv.FormatInt(sample.Entered.UnixNano(), 10), 1, string(raw))
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

// analyticsResidenceFrames holds each issue's residence sample at each of
// the requested points. An issue's sample at a point is its last edge earlier
// in the point's hour, or else its last hourly sample before that hour.
type analyticsResidenceFrames struct {
	retained map[string]time.Time
	states   map[string]tracker.NativeState
	ids      []string
	points   map[int64]int
	issues   map[string]int
	samples  []*analyticsResidenceSample
	cutoffs  map[string]residenceCutoff
	clipped  bool
}

// residenceRow is one residence rollup row; edges sort by hour, AtNS and
// event count, hourly samples by hour. Its sample is decoded only when a
// point selects it.
type residenceRow struct {
	hour, atNS int64
	value      float64
	raw        string
	sample     *analyticsResidenceSample
}

func (r *residenceRow) decode() (*analyticsResidenceSample, error) {
	if r.sample == nil {
		r.sample = &analyticsResidenceSample{}
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
	sample  analyticsResidenceSample
}

// sample is the issue's residence sample at one of the frame points. A time
// that is not a point, or an issue with no sample yet, has none.
func (f analyticsResidenceFrames) sample(at time.Time, id string) (analyticsResidenceSample, bool) {
	point, ok := f.points[at.UnixNano()]
	issue, known := f.issues[id]
	if !ok || !known {
		return analyticsResidenceSample{}, false
	}
	found := f.samples[point*len(f.ids)+issue]
	if found == nil {
		return analyticsResidenceSample{}, false
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

func (f analyticsResidenceFrames) at(at time.Time, id string) analyticsResidenceSample {
	sample, _ := f.sample(at, id)
	return sample
}

func readAnalyticsResidenceFrames(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, points []time.Time) (analyticsResidenceFrames, error) {
	out := analyticsResidenceFrames{points: map[int64]int{}, issues: map[string]int{}, cutoffs: map[string]residenceCutoff{}}
	var err error
	out.states, err = analyticsResidenceStates(ctx, q, scope)
	if err != nil {
		return out, err
	}
	out.retained = map[string]time.Time{}
	retained, err := q.QueryContext(ctx, `SELECT work_item_id,through_hour FROM analytics_residence_retention WHERE organization_id=? AND project_id=? AND through_hour>?`, scope.organization, scope.project, w.From.Unix())
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
		out.retained[id] = at
		if at.Before(w.To) {
			points = append(points, at)
		}
	}
	if err := errors.Join(retained.Err(), retained.Close()); err != nil {
		return out, err
	}
	issues, issueArgs := reportsIssueClause(ctx, "i.native_id")
	rows, err := queryAnalyticsPopulation(ctx, q, "residence_issues", `SELECT native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND julianday(i.created_at)<julianday(?) AND (coalesce(ws.terminal,0)=0 OR julianday(i.updated_at)>=julianday(?))`+issues+` ORDER BY native_id LIMIT ? OFFSET ?`, append([]any{scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From)}, issueArgs...)...)
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
	if len(points) == 0 || len(out.ids) == 0 {
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
	for i, id := range out.ids {
		out.issues[id] = i
	}
	edges, hours := map[string][]residenceRow{}, map[string][]residenceRow{}
	baselines := map[string]analyticsResidenceSample{}
	ids, err := json.Marshal(out.ids)
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
	if err := read(q.QueryContext(ctx, `SELECT metric,dimension,'',hour,0,value,data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND hour>=? AND hour<?`,
		scope.organization, scope.project, firstHour, lastHour)); err != nil {
		return out, err
	}
	var edgeHours []int64
	for _, ns := range instants {
		if hour := residenceHour(ns); hour*int64(time.Second) != ns && !slices.Contains(edgeHours, hour) {
			edgeHours = append(edgeHours, hour)
		}
	}
	if len(edgeHours) > 0 {
		encoded, err := json.Marshal(edgeHours)
		if err != nil {
			return out, err
		}
		if err := read(q.QueryContext(ctx, `SELECT r.metric,r.dimension,coalesce(r.sample_id,''),r.hour,coalesce(r.sample_ns,0),r.value,r.data_json FROM json_each(?) h CROSS JOIN analytics_hourly_rollups r ON r.organization_id=? AND r.project_id=? AND r.metric='residence_edge' AND r.hour=h.value`,
			string(encoded), scope.organization, scope.project)); err != nil {
			return out, err
		}
	}
	if err := read(q.QueryContext(ctx, `SELECT r.metric,r.dimension,'',r.hour,0,r.value,r.data_json FROM json_each(?) i JOIN analytics_hourly_rollups r ON r.rowid=(SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=i.value AND hour<? ORDER BY hour DESC LIMIT 1)`,
		string(ids), scope.organization, scope.project, firstHour)); err != nil {
		return out, err
	}
	if err := read(q.QueryContext(ctx, `SELECT 'residence_baseline',p.work_item_id,'',r.hour,0,r.value,r.data_json
FROM json_each(?) i JOIN analytics_residence_retention p ON p.organization_id=? AND p.project_id=? AND p.work_item_id=i.value
JOIN analytics_hourly_rollups r ON r.rowid=(SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=p.organization_id AND project_id=p.project_id AND metric='residence_hour' AND dimension=p.work_item_id AND hour<p.through_hour ORDER BY hour DESC LIMIT 1)`,
		string(ids), scope.organization, scope.project)); err != nil {
		return out, err
	}
	out.samples = make([]*analyticsResidenceSample, len(instants)*len(out.ids))
	for issue, id := range out.ids {
		if err := selectResidenceSamples(out.samples[issue:], len(out.ids), instants, edges[id], hours[id]); err != nil {
			return out, err
		}
	}
	remaining := maxAnalyticsPopulation
	var clipped []residenceClip
	for _, id := range out.ids {
		end := out.at(w.To, id)
		baseline, archived := baselines[id]
		events := max(0, end.Events-baseline.Events)
		allowed := min(remaining, events)
		remaining -= allowed
		allowed += baseline.Events
		if end.Events <= allowed {
			continue
		}
		out.clipped = true
		if page := analyticsPagingFromContext(ctx); page != nil {
			page.next["residence_events"] = "residence_events:1000:" + strconv.Itoa(analyticsPopulationOffset(ctx, "residence_issues"))
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
func selectResidenceSamples(selected []*analyticsResidenceSample, stride int, instants []int64, edges, hours []residenceRow) error {
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
func readResidenceCutoffs(ctx context.Context, q nativeQueryer, scope nativeScope, clipped []residenceClip, cutoffs map[string]residenceCutoff) error {
	encoded, err := json.Marshal(clipped)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT json_extract(c.value,'$.id'),r.data_json FROM json_each(?) c CROSS JOIN analytics_hourly_rollups r ON r.organization_id=? AND r.project_id=? AND r.metric='residence_edge' AND r.sample_id=json_extract(c.value,'$.id') AND r.value=json_extract(c.value,'$.allowed')`, string(encoded), scope.organization, scope.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var sample analyticsResidenceSample
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

func summarizeResidenceFrames(frames analyticsResidenceFrames, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	causes := map[string]operatortool.AnalyticsReworkCause{}
	var system, held, queue, lead []float64
	for _, id := range frames.ids {
		from := maxTime(from, frames.retained[id])
		if !from.Before(to) {
			continue
		}
		end := frames.at(to, id)
		begin := frames.at(from, id)
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
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, frames.states), AnalyticsDuration: sampledDuration(values)})
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
	summary.SystemTotal, summary.HeldTotal, summary.QueueTotal, summary.LeadTotal = sampledDuration(system), sampledDuration(held), sampledDuration(queue), sampledDuration(lead)
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
		sample := frames.at(out.Window.To, id)
		residence.EventsObserved += sample.Events
		available = available || sample.Initial != ""
		out.SourceAt = maxTime(out.SourceAt, maxTime(sample.Created, sample.At))
		frames.clipped = frames.clipped || sample.Partial
		if sample.Partial || frames.states[sample.State].Terminal || !sample.Entered.Before(out.Window.To) {
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
	if err := addAnalyticsResidenceHours(ctx, q, scope, out); err != nil {
		return err
	}
	return projectResidencePipelineRollups(ctx, q, scope, frames, out)
}

func projectResidencePipelineRollups(ctx context.Context, q nativeQueryer, scope nativeScope, frames analyticsResidenceFrames, out *nativeAnalyticsProject) error {
	ids, err := json.Marshal(frames.ids)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_merge' AND sample_id IN (SELECT value FROM json_each(?))`, scope.organization, scope.project, string(ids))
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
		sample := frames.at(out.Window.To, m.ID)
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
	hours, err := readDiagnosticsResidenceHours(ctx, q, scope, out.From, out.To)
	if err != nil {
		return 0, false, err
	}
	transitions := 0
	partial := frames.clipped
	for _, id := range frames.ids {
		from := maxTime(out.From, frames.retained[id])
		if from.Before(out.To) {
			transitions += frames.at(out.To, id).Transitions - frames.at(from, id).Transitions
		}
	}
	for hour, h := range hours {
		transitions += h.transitions
		partial = partial || h.partial || hour < out.From.Unix() || hour+3600 > out.To.Unix()
	}
	for i := range out.Capacity {
		at := minTime(out.Capacity[i].Hour.Add(time.Hour), out.To)
		count := 0
		depthPartial := frames.clipped
		for _, id := range frames.ids {
			retained := frames.retained[id]
			if !retained.IsZero() && !at.After(retained) {
				depthPartial = depthPartial || !at.Equal(at.Truncate(time.Hour))
				continue
			}
			sample, ok := frames.sample(at, id)
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

func readReportsCompletionRollups(ctx context.Context, q nativeQueryer, scope nativeScope, states map[string]tracker.NativeState, ids []string, to time.Time) (reportsCompletion, error) {
	out := reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}}
	var system, lead, working []float64
	lanes := map[string][]float64{}
	err := forShippedBatches(ids, func(batch string) error {
		rows, err := q.QueryContext(ctx, `SELECT (SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='completion' AND sample_id=i.value AND sample_ns<? ORDER BY sample_ns DESC,value DESC LIMIT 1) FROM json_each(?) i`, scope.organization, scope.project, to.UnixNano(), batch)
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
(SELECT e.analytics_from_state FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned'
 ORDER BY e.sequence LIMIT 1),CASE WHEN EXISTS (
 SELECT 1 FROM collaboration_events c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.type='issue.created'
 ) THEN ws.detent_state END,''),
(SELECT CASE WHEN e.analytics_projection_version IS NULL THEN e.data_json END FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1)
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
			var unknown sql.NullString
			if err := rows.Scan(&issue.id, &created, &issue.initial, &unknown); err != nil {
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
					issue.initial = *data.FromState
				}
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
		events, err := q.QueryContext(ctx, `SELECT work_item_id,type,recorded_at,`+collaborationPayloadColumn("")+` FROM collaboration_events
WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<julianday(?)
AND work_item_id IN (SELECT value FROM json_each(?))
AND (type='workflow.transitioned' OR (type='scheduler.decision' AND (scheduler_outcome IS NULL OR (scheduler_source='native_claim' AND scheduler_outcome='claimed'))))
ORDER BY work_item_id,sequence`, scope.organization, scope.project, formatHubTime(to), batch)
		if err != nil {
			return fmt.Errorf("read shipped transitions: %w", err)
		}
		defer events.Close()
		type record struct {
			id, at, raw string
			event       analyticsResidenceEvent
		}
		var records []record
		for events.Next() {
			var r record
			if err := events.Scan(&r.id, &r.event.kind, &r.at, &r.raw); err != nil {
				return fmt.Errorf("read shipped transitions: %w", err)
			}
			records = append(records, r)
		}
		if err := errors.Join(events.Err(), events.Close()); err != nil {
			return err
		}
		for _, r := range records {
			id, event := r.id, r.event
			if event.at, err = parseTimeValue(r.at); err != nil {
				return err
			}
			if err := decodeRunHistory(ctx, q, r.raw, &event.data); err != nil {
				return err
			}
			if event.kind == "scheduler.decision" && (event.data.Decision == nil || event.data.Decision.Source != "native_claim" || event.data.Decision.Outcome != "claimed") {
				continue
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
