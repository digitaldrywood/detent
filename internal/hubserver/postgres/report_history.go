package postgres

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportreads"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	return []any{s.organization, projects, query.Principal, query.From.UTC(), query.To.UTC(), query.Filter.Types, query.Filter.Priorities, query.Filter.FiledBy, query.Filter.Labels, query.Filter.Runners, query.Limit + 1, query.Filter.Models, query.Filter.Stages}, nil
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
	rows, err := s.db.Query(ctx, `SELECT project_id,name,terminal,dispatchable FROM project_workflow_states WHERE organization_id=$1 AND project_id=ANY($2::text[]) ORDER BY project_id,board_rank LIMIT 5001`, args[0], args[1])
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
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if stateCount > 5000 {
		return nil, reportstore.ErrPopulationExceeded
	}
	projectIDs, ok := args[1].([]string)
	if !ok {
		return nil, errors.New("report query projects have an unexpected type")
	}
	candidateIDs, active, err := s.readHistoryCandidates(ctx, projectIDs, query.From.UTC(), query.To.UTC())
	if err != nil {
		return nil, err
	}
	rows, err = s.db.Query(ctx, `SELECT i.project_id,i.native_id,i.created_at,coalesce(
(SELECT e.from_state FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1),
CASE WHEN (SELECT true FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.created' LIMIT 1) THEN i.workflow_state_name END,'')
FROM unnest($14::bigint[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=$1 AND i.id=candidate.id LIMIT 1) i
LEFT JOIN LATERAL (SELECT ws.terminal FROM project_workflow_states ws WHERE ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name LIMIT 1) ws ON true
WHERE `+reportreads.IssueFilter+` AND i.created_at<$5 AND (NOT coalesce(ws.terminal,false) OR i.native_updated_at>=$4 OR i.native_id=ANY($15::text[]))
ORDER BY i.project_id,i.native_id LIMIT $11`, append(args, candidateIDs, active)...)
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
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(ids) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	if len(ids) == 0 {
		return out, nil
	}
	eventLimit := min(query.Limit*100, 100000)
	const events = `SELECT e.project_id,e.work_item_id,e.recorded_at,e.sequence,e.id,e.version_id,e.type,e.from_state,e.to_state
 FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=ANY($2::text[]) AND e.work_item_id=ANY($3::text[])
 AND e.type IN ('workflow.transitioned','scheduler.decision') AND e.recorded_at<$4`
	pages := reportreads.FactPages("report_page", events, "project_id,work_item_id,recorded_at,sequence", eventLimit)
	rows, err = s.db.Query(ctx, "WITH "+pages+` SELECT e.work_item_id,e.sequence,e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state,coalesce(f.rework_class,''),coalesce(f.rework_kind,'')
 FROM report_page e LEFT JOIN LATERAL (SELECT f.rework_class,f.rework_kind FROM report_event_facts f WHERE f.organization_id=$1 AND f.event_id=e.id LIMIT 1) f ON true`, s.organization, projectIDs, ids, query.To.UTC())
	if err != nil {
		return nil, mapError(ctx, err)
	}
	sequences := make(map[string]int64)
	count := 0
	for rows.Next() {
		var id tracker.NativeWorkItemID
		var sequence int64
		var event reportstore.WorkflowEvent
		if err := rows.Scan(&id, &sequence, &event.ID, &event.VersionID, &event.At, &event.Type, &event.FromState, &event.ToState, &event.Class.Name, &event.Class.Kind); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		sequences[event.ID] = sequence
		location := locations[id]
		out[location.project].Issues[location.issue].Events = append(out[location.project].Issues[location.issue].Events, event)
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if count > eventLimit {
		return nil, reportstore.ErrPopulationExceeded
	}
	for _, project := range out {
		for _, issue := range project.Issues {
			slices.SortFunc(issue.Events, func(a, b reportstore.WorkflowEvent) int {
				return cmp.Compare(sequences[a.ID], sequences[b.ID])
			})
		}
	}
	return out, nil
}

func (s *session) readHistoryCandidates(ctx context.Context, projects []string, from, to time.Time) ([]int64, []string, error) {
	return reportreads.HistoryCandidates(ctx, s.db, s.organization, projects, from, to)
}

func (s *session) ReadHistoryIssueEvents(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor reportstore.HistoryEventCursor, limit int) (reportstore.HistoryEventPage, error) {
	if err := checkPage(limit); err != nil {
		return reportstore.HistoryEventPage{}, err
	}
	if project == "" || item == "" || cursor.After < 0 || cursor.Through < 0 || cursor.After > cursor.Through {
		return reportstore.HistoryEventPage{}, corestore.ErrInvalidCursor
	}
	if _, err := s.WorkItem(ctx, project, item); err != nil {
		return reportstore.HistoryEventPage{}, err
	}
	rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED (
 SELECT sequence,id,version_id,recorded_at,type,from_state,to_state FROM collaboration_events
 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND sequence>$4 AND sequence<=$5
 ORDER BY sequence LIMIT $6
)
SELECT e.sequence,e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state,
 coalesce(f.rework_class,''),coalesce(f.rework_kind,'')
FROM examined e LEFT JOIN report_event_facts f ON f.organization_id=$1 AND f.event_id=e.id
ORDER BY e.sequence`, s.organization, project, item, cursor.After, cursor.Through, limit)
	if err != nil {
		return reportstore.HistoryEventPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := reportstore.HistoryEventPage{After: cursor.After}
	for rows.Next() {
		var event reportstore.WorkflowEvent
		if err := rows.Scan(&page.After, &event.ID, &event.VersionID, &event.At, &event.Type, &event.FromState, &event.ToState, &event.Class.Name, &event.Class.Kind); err != nil {
			return reportstore.HistoryEventPage{}, mapError(ctx, err)
		}
		page.Examined++
		if event.Type == "workflow.transitioned" || event.Type == "scheduler.decision" {
			page.Events = append(page.Events, event)
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return reportstore.HistoryEventPage{}, err
	}
	page.Exhausted = page.Examined < limit || page.After == cursor.Through
	return page, nil
}

func (s *session) ReadAnalyticsResidencePage(ctx context.Context, query eventstore.AnalyticsResidenceQuery) (eventstore.AnalyticsResidencePage, error) {
	out := eventstore.AnalyticsResidencePage{States: []tracker.NativeState{}, Issues: []eventstore.AnalyticsResidenceIssue{}, Events: []eventstore.Event{}}
	if query.ProjectID == "" || query.Limit < 1 || query.Limit > 1000 || !query.From.Before(query.To) || query.EventsAfterSequence < 0 || (query.EventsAfterItem == "") != (query.EventsAfterSequence == 0) {
		return out, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT name,terminal,dispatchable FROM project_workflow_states WHERE organization_id=$1 AND project_id=$2 ORDER BY board_rank LIMIT 51`, s.organization, query.ProjectID)
	if err != nil {
		return out, mapError(ctx, err)
	}
	for rows.Next() {
		var state tracker.NativeState
		if err := rows.Scan(&state.Name, &state.Terminal, &state.Dispatchable); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		out.States = append(out.States, state)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, mapError(ctx, err)
	}
	rows.Close()
	if len(out.States) > 50 {
		return out, reportstore.ErrPopulationExceeded
	}
	rows, err = s.db.Query(ctx, `WITH candidates AS MATERIALIZED (
 SELECT i.native_id,i.created_at,i.native_updated_at,i.workflow_state_name,i.archived,i.actor_principal_id
 FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id>$3 ORDER BY i.native_id LIMIT $4
 ) SELECT c.native_id,c.created_at,c.native_updated_at,c.archived,coalesce(c.actor_principal_id,''),coalesce(ws.terminal,false),coalesce(first_event.from_state,CASE WHEN creation.present THEN c.workflow_state_name END,'')
 FROM candidates c LEFT JOIN project_workflow_states ws ON ws.organization_id=$1 AND ws.project_id=$2 AND ws.name=c.workflow_state_name
 LEFT JOIN LATERAL(SELECT e.from_state FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=c.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1) first_event ON true
 LEFT JOIN LATERAL(SELECT true AS present FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=c.native_id AND e.type='issue.created' ORDER BY e.sequence LIMIT 1) creation ON true
 ORDER BY c.native_id`, s.organization, query.ProjectID, query.IssuesAfter, query.Limit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type candidate struct {
		issue              eventstore.AnalyticsResidenceIssue
		updated            time.Time
		archived, terminal bool
		principal          string
	}
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.issue.WorkItemID, &c.issue.CreatedAt, &c.updated, &c.archived, &c.principal, &c.terminal, &c.issue.InitialState); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, mapError(ctx, err)
	}
	rows.Close()
	out.IssuesMore = len(candidates) > query.Limit
	candidates = candidates[:min(len(candidates), query.Limit)]
	visible := map[tracker.NativeWorkItemID]bool{}
	for _, c := range candidates {
		out.IssuesAfter = c.issue.WorkItemID
		if !c.issue.CreatedAt.Before(query.To) || c.terminal && c.updated.Before(query.From) || query.Live && c.archived && !c.terminal || query.Principal != "" && c.principal != query.Principal {
			continue
		}
		visible[c.issue.WorkItemID] = true
		out.Issues = append(out.Issues, c.issue)
	}
	if len(candidates) == 0 {
		return out, nil
	}
	afterItem, afterSequence := query.EventsAfterItem, query.EventsAfterSequence
	if afterItem == "" {
		afterItem = candidates[0].issue.WorkItemID
	}
	rows, err = s.db.Query(ctx, `SELECT e.* FROM collaboration_events e
 WHERE e.organization_id=$1 AND e.project_id=$2 AND (e.work_item_id,e.sequence)>($3,$4) AND e.work_item_id<=$5
 AND (e.type='workflow.transitioned' OR e.type='scheduler.decision' AND e.scheduler_outcome='claimed')
 ORDER BY e.work_item_id,e.sequence LIMIT $6`, s.organization, query.ProjectID, afterItem, afterSequence, candidates[len(candidates)-1].issue.WorkItemID, query.Limit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.CollaborationEvent])
	if err != nil {
		return out, mapError(ctx, err)
	}
	out.EventsMore = len(events) > query.Limit
	events = events[:min(len(events), query.Limit)]
	for _, e := range events {
		out.EventsAfterItem, out.EventsAfterSequence = e.WorkItemID, e.Sequence
		if visible[e.WorkItemID] && e.RecordedAt.Before(query.To) {
			out.Events = append(out.Events, eventRecord(e))
		}
	}
	return out, nil
}
