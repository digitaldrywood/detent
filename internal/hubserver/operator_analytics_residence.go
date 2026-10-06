package hubserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsResidenceIssue struct {
	id      string
	created time.Time
	initial string
	events  []analyticsResidenceEvent
	partial bool
}

type analyticsResidenceEvent struct {
	at   time.Time
	kind string
	data tracker.CollaborationData
}

type analyticsResidenceSpan struct {
	lane    string
	from    time.Time
	to      time.Time
	queueTo time.Time
}

type analyticsResidenceTimeline struct {
	issue   analyticsResidenceIssue
	started *time.Time
	spans   []analyticsResidenceSpan
	state   string
	entered time.Time
	done    []time.Time
	rework  []analyticsResidenceEvent
}

func projectNativeAnalyticsResidence(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, out *nativeAnalyticsProject) error {
	states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, q, scope, out.Window)
	if err != nil {
		return err
	}
	residence := operatortool.AnalyticsResidence{
		Source:         "native_workflow_transitions_and_issue_creation",
		Coverage:       "per_issue; residence_clipped_to_window; system_excludes_intake_terminal_and_held; held_after_first_dispatch; lead_created_to_done_at_completion; aging_at_window_end; quantiles_linear_interpolation",
		IssuesObserved: len(issues),
	}
	timelines := make([]analyticsResidenceTimeline, 0, len(issues))
	available := false
	for _, issue := range issues {
		available = available || issue.initial != ""
		residence.EventsObserved += len(issue.events)
		for _, event := range issue.events {
			if event.at.After(out.SourceAt) {
				out.SourceAt = event.at
			}
		}
		if issue.created.After(out.SourceAt) {
			out.SourceAt = issue.created
		}
		timeline := buildAnalyticsResidenceTimeline(issue, states, out.Window.To)
		clipped = clipped || timeline.issue.partial
		timelines = append(timelines, timeline)
	}
	var items []operatortool.AnalyticsIssueResidence
	residence.AnalyticsResidenceSummary, items = summarizeAnalyticsResidence(timelines, states, out.Window.From, out.Window.To)
	residence.Partial = clipped
	residence.Issues = operatortool.OffsetPage(items, r.RowOffset, r.Limit)
	aging := []operatortool.AnalyticsAging{}
	for _, timeline := range timelines {
		if timeline.issue.partial || states[timeline.state].Terminal || !timeline.entered.Before(out.Window.To) {
			continue
		}
		item := operatortool.AnalyticsAging{WorkItemID: timeline.issue.id, Lane: timeline.state, EnteredAt: timeline.entered, Hours: out.Window.To.Sub(timeline.entered).Hours()}
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

func readAnalyticsResidenceHistory(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) (map[string]tracker.NativeState, []analyticsResidenceIssue, bool, error) {
	states := map[string]tracker.NativeState{}
	rows, err := q.QueryContext(ctx, `SELECT ws.detent_state,ws.terminal,ws.dispatchable FROM workflow_states ws
JOIN projects p ON p.id=ws.project_id WHERE p.organization_id=? AND p.id=? LIMIT 50`, scope.organization, scope.project)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var state tracker.NativeState
		if err := rows.Scan(&state.Name, &state.Terminal, &state.Dispatchable); err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		states[state.Name] = state
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, false, err
	}
	rows, err = q.QueryContext(ctx, `SELECT i.native_id,i.created_at,coalesce(
(SELECT json_extract(e.data_json,'$.from_state') FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned'
 ORDER BY e.sequence LIMIT 1),CASE WHEN EXISTS (
 SELECT 1 FROM collaboration_events c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.type='issue.created'
 ) THEN ws.detent_state END,'')
FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=? AND julianday(i.created_at)<julianday(?)
AND (coalesce(ws.terminal,0)=0 OR julianday(i.updated_at)>=julianday(?))
ORDER BY i.native_id LIMIT ?`, scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From), maxAnalyticsPopulation+1)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	issues := []analyticsResidenceIssue{}
	clipped := false
	for rows.Next() {
		if len(issues) == maxAnalyticsPopulation {
			clipped = true
			break
		}
		var issue analyticsResidenceIssue
		var created string
		if err := rows.Scan(&issue.id, &created, &issue.initial); err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		issue.created, err = parseTimeValue(created)
		if err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, false, err
	}
	if len(issues) == 0 {
		return states, issues, clipped, nil
	}
	args := []any{scope.organization, scope.project, formatHubTime(w.To)}
	byID := make(map[string]int, len(issues))
	for i, issue := range issues {
		byID[issue.id] = i
		args = append(args, issue.id)
	}
	args = append(args, maxAnalyticsPopulation+1)
	rows, err = q.QueryContext(ctx, `SELECT work_item_id,type,recorded_at,data_json FROM collaboration_events
WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<julianday(?)
AND work_item_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(issues)), ",")+`)
AND (type='workflow.transitioned' OR (type='scheduler.decision' AND json_extract(data_json,'$.decision.source')='native_claim' AND json_extract(data_json,'$.decision.outcome')='claimed'))
ORDER BY work_item_id,sequence LIMIT ?`, args...)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	observed := 0
	for rows.Next() {
		var id, at, raw string
		var event analyticsResidenceEvent
		if err := rows.Scan(&id, &event.kind, &at, &raw); err != nil {
			return nil, nil, false, err
		}
		if observed == maxAnalyticsPopulation {
			clipped = true
			for i := byID[id]; i < len(issues); i++ {
				issues[i].partial = true
			}
			break
		}
		observed++
		event.at, err = parseTimeValue(at)
		if err != nil {
			return nil, nil, false, err
		}
		if err := json.Unmarshal([]byte(raw), &event.data); err != nil {
			return nil, nil, false, err
		}
		issues[byID[id]].events = append(issues[byID[id]].events, event)
	}
	return states, issues, clipped, rows.Err()
}

func buildAnalyticsResidenceTimeline(issue analyticsResidenceIssue, states map[string]tracker.NativeState, to time.Time) analyticsResidenceTimeline {
	timeline := analyticsResidenceTimeline{issue: issue, state: issue.initial, entered: issue.created}
	if issue.initial == "" {
		timeline.issue.partial = true
	}
	if states[issue.initial].Dispatchable && analyticsResidenceGroup(issue.initial, states) == "system" {
		at := issue.created
		timeline.started = &at
	}
	queueTo := to
	last := issue.created
	for _, event := range issue.events {
		if event.at.Before(last) {
			timeline.issue.partial = true
			continue
		}
		last = event.at
		if event.kind == "scheduler.decision" {
			if timeline.state == "Rework" && queueTo.After(event.at) {
				queueTo = event.at
			}
			continue
		}
		if event.data.FromState != timeline.state || event.data.ToState == "" {
			timeline.issue.partial = true
		} else if event.data.ToState != timeline.state && timeline.started != nil {
			timeline.spans = append(timeline.spans, analyticsResidenceSpan{lane: timeline.state, from: maxTime(timeline.entered, *timeline.started), to: event.at, queueTo: minTime(queueTo, event.at)})
		}
		if event.data.ToState == "" || event.data.ToState == timeline.state {
			continue
		}
		timeline.state, timeline.entered = event.data.ToState, event.at
		queueTo = to
		if timeline.started == nil && states[timeline.state].Dispatchable && analyticsResidenceGroup(timeline.state, states) == "system" {
			at := event.at
			timeline.started = &at
		}
		if timeline.state == "Done" {
			timeline.done = append(timeline.done, event.at)
		}
		if timeline.state == "Rework" {
			timeline.rework = append(timeline.rework, event)
		}
	}
	if timeline.started != nil && !timeline.issue.partial {
		timeline.spans = append(timeline.spans, analyticsResidenceSpan{lane: timeline.state, from: maxTime(timeline.entered, *timeline.started), to: to, queueTo: queueTo})
	}
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

func summarizeAnalyticsResidence(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	var system, held, lead, queue []float64
	causes := map[[2]string]operatortool.AnalyticsReworkCause{}
	for _, timeline := range timelines {
		item := operatortool.AnalyticsIssueResidence{WorkItemID: timeline.issue.id, SystemStartedAt: timeline.started, LaneSeconds: map[string]float64{}, Partial: timeline.issue.partial}
		for _, span := range timeline.spans {
			group := analyticsResidenceGroup(span.lane, states)
			seconds := minTime(span.to, to).Sub(maxTime(span.from, from)).Seconds()
			if seconds <= 0 || group == "excluded" {
				continue
			}
			item.LaneSeconds[span.lane] += seconds
			if group == "held" {
				item.HeldSeconds += seconds
			} else {
				item.SystemSeconds += seconds
			}
			if span.lane == "Todo" || span.lane == "Rework" {
				end := span.to
				if span.lane == "Rework" {
					end = span.queueTo
				}
				item.QueueSeconds += max(0, minTime(end, to).Sub(maxTime(span.from, from)).Seconds())
			}
		}
		for _, done := range timeline.done {
			if !done.Before(from) && done.Before(to) {
				seconds := done.Sub(timeline.issue.created).Seconds()
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
			if event.at.Before(from) || !event.at.Before(to) {
				continue
			}
			key := [2]string{event.data.FromState, event.data.ReasonDetail}
			cause := causes[key]
			cause.FromState, cause.ReasonDetail = event.data.FromState, event.data.ReasonDetail
			cause.Count++
			causes[key] = cause
		}
		items = append(items, item)
	}
	for lane, values := range lanes {
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, states), AnalyticsDuration: analyticsDuration(values)})
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
	summary.SystemTotal, summary.HeldTotal = analyticsDuration(system), analyticsDuration(held)
	summary.LeadTotal, summary.QueueTotal = analyticsDuration(lead), analyticsDuration(queue)
	return summary, items
}

func analyticsDuration(values []float64) operatortool.AnalyticsDuration {
	out := operatortool.AnalyticsDuration{Count: len(values)}
	if len(values) == 0 {
		return out
	}
	sort.Float64s(values)
	for _, seconds := range values {
		out.Seconds += seconds
	}
	quantile := func(q float64) float64 {
		index := q * float64(len(values)-1)
		lower := int(index)
		upper := min(lower+1, len(values)-1)
		return values[lower] + (values[upper]-values[lower])*(index-float64(lower))
	}
	out.P50Seconds, out.P90Seconds = quantile(0.5), quantile(0.9)
	return out
}
