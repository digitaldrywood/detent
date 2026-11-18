package reportcheckpoints

import (
	"cmp"
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportreads"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadHistoryEvents(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, query reportstore.OverviewQuery, projects []reportstore.ProjectHistory, heads map[string]HistoryPosition) error {
	eventLimit := min(query.Limit*100, 100000)
	ids, count, err := readHistoryBlocks(ctx, db, organization, query, projects, heads, eventLimit)
	if err != nil || len(ids) == 0 {
		return err
	}
	issues := map[tracker.NativeWorkItemID]*reportstore.HistoryIssue{}
	var projectIDs []string
	for _, project := range projects {
		projectIDs = append(projectIDs, string(project.ProjectID))
		for i := range project.Issues {
			issue := &project.Issues[i]
			issues[issue.WorkItemID] = issue
		}
	}
	const events = `SELECT e.project_id,e.work_item_id,e.recorded_at,e.sequence,e.id,e.version_id,e.type,e.from_state,e.to_state
 FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=ANY($2::text[]) AND e.work_item_id=ANY($3::text[])
 AND e.type IN ('workflow.transitioned','scheduler.decision') AND e.recorded_at<$4`
	pages := reportreads.FactPages("report_page", events, "project_id,work_item_id,recorded_at,sequence", eventLimit-count)
	rows, err := db.Query(ctx, "WITH "+pages+` SELECT e.work_item_id,e.sequence,e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state,coalesce(f.rework_class,''),coalesce(f.rework_kind,'')
 FROM report_page e LEFT JOIN LATERAL (SELECT f.rework_class,f.rework_kind FROM report_event_facts f WHERE f.organization_id=$1 AND f.event_id=e.id LIMIT 1) f ON true`, organization, projectIDs, ids, query.To.UTC())
	if err != nil {
		return err
	}
	sequences := make(map[string]int64)
	for rows.Next() {
		var id tracker.NativeWorkItemID
		var sequence int64
		var event reportstore.WorkflowEvent
		if err := rows.Scan(&id, &sequence, &event.ID, &event.VersionID, &event.At, &event.Type, &event.FromState, &event.ToState, &event.Class.Name, &event.Class.Kind); err != nil {
			rows.Close()
			return err
		}
		sequences[event.ID] = sequence
		issue := issues[id]
		issue.Events = append(issue.Events, event)
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count > eventLimit {
		return reportstore.ErrPopulationExceeded
	}
	for _, id := range ids {
		slices.SortFunc(issues[tracker.NativeWorkItemID(id)].Events, func(a, b reportstore.WorkflowEvent) int { return cmp.Compare(sequences[a.ID], sequences[b.ID]) })
	}
	return nil
}
