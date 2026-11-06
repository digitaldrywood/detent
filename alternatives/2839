package postgres

import (
	"context"
	"errors"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const reportIssueFilter = `i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND ($3='' OR i.actor_principal_id=$3)
AND (coalesce(cardinality($6::text[]),0)=0 OR i.issue_type=ANY($6::text[]))
AND (coalesce(cardinality($7::integer[]),0)=0 OR i.priority=ANY($7::integer[]))
AND (coalesce(cardinality($8::text[]),0)=0 OR i.actor_principal_id=ANY($8::text[]))
AND (coalesce(cardinality($9::text[]),0)=0 OR EXISTS (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($9::text[])))
AND (coalesce(cardinality($10::text[]),0)=0 OR EXISTS (SELECT 1 FROM native_attempts a JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<$5 AND a.updated_at>=$4 AND la.runner_id=ANY($10::text[])))
AND (coalesce(cardinality($12::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.recorded_at>=$4 AND e.recorded_at<$5 AND f.model=ANY($12::text[])))
AND (coalesce(cardinality($13::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.recorded_at>=$4 AND e.recorded_at<$5 AND f.role=ANY($13::text[])))`

func (s *session) reportQueryArgs(query reportstore.OverviewQuery) ([]any, error) {
	if len(query.Projects) == 0 || query.Limit < 1 || query.Limit > maxOverviewFactLimit || !query.From.Before(query.To) {
		return nil, errors.New("invalid PostgreSQL report history query")
	}
	projects := make([]string, len(query.Projects))
	for i, p := range query.Projects {
		if p == "" {
			return nil, errors.New("invalid PostgreSQL report project")
		}
		projects[i] = string(p)
	}
	return []any{s.organization, pq.Array(projects), query.Principal, query.From.UTC(), query.To.UTC(), pq.Array(query.Filter.Types), pq.Array(query.Filter.Priorities), pq.Array(query.Filter.FiledBy), pq.Array(query.Filter.Labels), pq.Array(query.Filter.Runners), query.Limit + 1, pq.Array(query.Filter.Models), pq.Array(query.Filter.Stages)}, nil
}

func (s *session) ReadHistory(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.ProjectHistory, error) {
	args, err := s.reportQueryArgs(query)
	if err != nil {
		return nil, err
	}
	out := make([]reportstore.ProjectHistory, len(query.Projects))
	projects := map[tracker.ProjectID]int{}
	for i, id := range query.Projects {
		projects[id] = i
		out[i] = reportstore.ProjectHistory{ProjectID: id, States: []tracker.NativeState{}, Issues: []reportstore.HistoryIssue{}}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project_id,name,terminal,dispatchable FROM project_workflow_states WHERE organization_id=$1 AND project_id=ANY($2::text[]) ORDER BY project_id,board_rank LIMIT 5001`, args[0], args[1])
	if err != nil {
		return nil, mapError(ctx, err)
	}
	stateCount := 0
	for rows.Next() {
		var project tracker.ProjectID
		var state tracker.NativeState
		if err := rows.Scan(&project, &state.Name, &state.Terminal, &state.Dispatchable); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		stateCount++
		out[projects[project]].States = append(out[projects[project]].States, state)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if stateCount > 5000 {
		return nil, reportstore.ErrPopulationExceeded
	}
	rows, err = s.db.QueryContext(ctx, `SELECT i.project_id,i.native_id,i.created_at,coalesce(
(SELECT e.from_state FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1),
CASE WHEN EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.created') THEN i.workflow_state_name END,'')
FROM issues i LEFT JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name
WHERE `+reportIssueFilter+` AND i.created_at<$5 AND (NOT coalesce(ws.terminal,false) OR i.native_updated_at>=$4 OR EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.recorded_at>=$4 AND e.recorded_at<$5))
ORDER BY i.project_id,i.native_id LIMIT $11`, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	type location struct{ project, issue int }
	locations := map[tracker.NativeWorkItemID]location{}
	ids := []string{}
	for rows.Next() {
		var project tracker.ProjectID
		var issue reportstore.HistoryIssue
		if err := rows.Scan(&project, &issue.WorkItemID, &issue.CreatedAt, &issue.InitialState); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		issue.Events = []reportstore.WorkflowEvent{}
		index := projects[project]
		locations[issue.WorkItemID] = location{index, len(out[index].Issues)}
		out[index].Issues = append(out[index].Issues, issue)
		ids = append(ids, string(issue.WorkItemID))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(ids) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	if len(ids) == 0 {
		return out, nil
	}
	eventLimit := min(query.Limit*100, 100000)
	rows, err = s.db.QueryContext(ctx, `SELECT e.work_item_id,e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state,coalesce(f.rework_class,''),coalesce(f.rework_kind,'')
FROM collaboration_events e LEFT JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id
WHERE e.organization_id=$1 AND e.work_item_id=ANY($2::text[]) AND e.recorded_at<$3 AND e.type IN ('workflow.transitioned','scheduler.decision')
ORDER BY e.project_id,e.work_item_id,e.sequence LIMIT $4`, s.organization, pq.Array(ids), query.To.UTC(), eventLimit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	count := 0
	for rows.Next() {
		var id tracker.NativeWorkItemID
		var event reportstore.WorkflowEvent
		if err := rows.Scan(&id, &event.ID, &event.VersionID, &event.At, &event.Type, &event.FromState, &event.ToState, &event.Class.Name, &event.Class.Kind); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		location := locations[id]
		out[location.project].Issues[location.issue].Events = append(out[location.project].Issues[location.issue].Events, event)
		count++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if count > eventLimit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}
