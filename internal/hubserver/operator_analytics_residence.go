package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectNativeAnalyticsResidence(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, out *hubanalytics.Project) error {
	if _, projected := q.(analyticsRollupReader); projected && hubanalytics.PopulationOffset(ctx, "residence_events") == 0 {
		return projectAnalyticsResidenceRollups(ctx, q, scope, r, out)
	}
	return projectNativeAnalyticsResidenceHistory(ctx, q, scope, r, out)
}

func projectNativeAnalyticsResidenceHistory(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, out *hubanalytics.Project) error {
	states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, q, scope, out.Window)
	if err != nil {
		return err
	}
	residence := operatortool.AnalyticsResidence{
		Source:         "native_workflow_transitions_and_issue_creation",
		Coverage:       "per_issue; residence_clipped_to_window; system_excludes_intake_terminal_and_held; held_after_first_dispatch; lead_created_to_done_at_completion; aging_at_window_end; quantiles_linear_interpolation",
		IssuesObserved: len(issues),
	}
	timelines := make([]hubanalytics.ResidenceTimeline, 0, len(issues))
	available := false
	for _, issue := range issues {
		available = available || issue.Initial != ""
		residence.EventsObserved += len(issue.Events)
		for _, event := range issue.Events {
			if event.At.After(out.SourceAt) {
				out.SourceAt = event.At
			}
		}
		if issue.Created.After(out.SourceAt) {
			out.SourceAt = issue.Created
		}
		timeline := hubanalytics.BuildResidenceTimeline(issue, states, out.Window.To)
		clipped = clipped || timeline.Issue.Partial
		timelines = append(timelines, timeline)
		out.PipelineTimings = append(out.PipelineTimings, hubanalytics.ResidencePipelineTimings(timeline, out.AllAttempts)...)
	}
	var items []operatortool.AnalyticsIssueResidence
	residence.AnalyticsResidenceSummary, items = hubanalytics.SummarizeResidence(timelines, states, out.Window.From, out.Window.To)
	residence.Partial = clipped
	residence.Issues = operatortool.OffsetPage(items, r.RowOffset, r.Limit)
	aging := []operatortool.AnalyticsAging{}
	for _, timeline := range timelines {
		if timeline.Issue.Partial || states[timeline.State].Terminal || !timeline.Entered.Before(out.Window.To) {
			continue
		}
		item := operatortool.AnalyticsAging{WorkItemID: timeline.Issue.ID, Lane: timeline.State, EnteredAt: timeline.Entered, Hours: out.Window.To.Sub(timeline.Entered).Hours()}
		if timeline.Started != nil {
			since := out.Window.To.Sub(*timeline.Started).Hours()
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
	residence.Aging = operatortool.OffsetPage(aging, r.RowOffset, r.Limit)
	out.LaneResidence = residence
	out.Partial = out.Partial || clipped
	for i := range out.Digest {
		bucket := &out.Digest[i]
		bucket.LaneResidence, _ = hubanalytics.SummarizeResidence(timelines, states, bucket.From, bucket.To)
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

type analyticsLiveIssuesKey struct{}

// withAnalyticsLiveIssues leaves archived nonterminal issues out of residence
// history so they neither appear as current work nor consume the population cap.
func withAnalyticsLiveIssues(ctx context.Context) context.Context {
	return context.WithValue(ctx, analyticsLiveIssuesKey{}, true)
}

func readAnalyticsResidenceHistory(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) (map[string]tracker.NativeState, []hubanalytics.ResidenceIssue, bool, error) {
	q, prepareErr := prepareReportsQuery(ctx, q, scope, "report_events")
	if prepareErr != nil {
		return nil, nil, false, prepareErr
	}
	states := map[string]tracker.NativeState{}
	rows, err := hubanalytics.QueryAnalyticsRows(ctx, q, `SELECT ws.detent_state,ws.terminal,ws.dispatchable FROM workflow_states ws
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
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	issueFilter, issueArgs := reportsIssueClause(ctx, "i.native_id")
	if _, live := ctx.Value(analyticsLiveIssuesKey{}).(bool); live {
		issueFilter += " AND NOT (i.archived=1 AND coalesce(ws.terminal,0)=0)"
	}
	rows, err = hubanalytics.QueryAnalyticsPopulation(ctx, q, "residence_issues", `SELECT i.native_id,i.created_at,coalesce(
(SELECT e.analytics_from_state FROM report_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned'
 ORDER BY e.sequence LIMIT 1),CASE WHEN EXISTS (
 SELECT 1 FROM report_events c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.type='issue.created'
 ) THEN ws.detent_state END,'')
FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=? AND julianday(i.created_at)<julianday(?)
AND (coalesce(ws.terminal,0)=0 OR julianday(i.updated_at)>=julianday(?))`+issueFilter+`
ORDER BY i.native_id LIMIT ? OFFSET ?`, append([]any{scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From)}, issueArgs...)...)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	issues := []hubanalytics.ResidenceIssue{}
	clipped := hubanalytics.PopulationOffset(ctx, "residence_issues") > 0 || hubanalytics.PopulationOffset(ctx, "residence_events") > 0
	for rows.Next() {
		if len(issues) == hubanalytics.MaxPopulation {
			clipped = true
			break
		}
		var issue hubanalytics.ResidenceIssue
		var created string
		if err := rows.Scan(&issue.ID, &created, &issue.Initial); err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		issue.Created, err = parseTimeValue(created)
		if err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		issue.Partial = hubanalytics.PopulationOffset(ctx, "residence_events") > 0
		if issue.Partial {
			issue.Initial = ""
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, false, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	if len(issues) == 0 {
		return states, issues, clipped, nil
	}
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	encodedIDs, err := sqlList(ids)
	if err != nil {
		return nil, nil, false, err
	}
	args := []any{scope.organization, scope.project, formatHubTime(w.To), encodedIDs}
	byID := make(map[string]int, len(issues))
	for i, issue := range issues {
		byID[issue.ID] = i
	}
	rows, err = hubanalytics.QueryAnalyticsPopulation(ctx, q, "residence_events", `SELECT work_item_id,type,recorded_at,data_json FROM report_events
WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<julianday(?)
AND work_item_id IN (SELECT value FROM sql_rows(?))
AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed'))
ORDER BY work_item_id,sequence LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	observed := 0
	type record struct {
		id, at, raw string
		event       hubanalytics.ResidenceEvent
	}
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.id, &r.event.Kind, &r.at, &r.raw); err != nil {
			return nil, nil, false, err
		}
		records = append(records, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	for _, r := range records {
		id, at, raw, event := r.id, r.at, r.raw, r.event
		if observed == hubanalytics.MaxPopulation {
			clipped = true
			for i := byID[id]; i < len(issues); i++ {
				issues[i].Partial = true
			}
			break
		}
		observed++
		event.At, err = parseTimeValue(at)
		if err != nil {
			return nil, nil, false, err
		}
		if err := decodeRunHistory(ctx, q, raw, &event.Data); err != nil {
			return nil, nil, false, err
		}
		issues[byID[id]].Events = append(issues[byID[id]].Events, event)
	}
	return states, issues, clipped, rows.Err()
}
