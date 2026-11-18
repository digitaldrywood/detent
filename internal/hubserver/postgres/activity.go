package postgres

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ActivitySnapshot(ctx context.Context, filter activitystore.Filter) (activitystore.Snapshot, error) {
	out := activitystore.Snapshot{States: map[tracker.ProjectID][]tracker.NativeState{}, Sequences: map[string]int64{}, Created: map[int64]bool{}}
	if len(filter.Projects) > 100 || filter.Limit < 1 || filter.Limit > activitystore.IssuePopulationLimit || !filter.From.Before(filter.To) || filter.To.After(filter.Now) || filter.To.Sub(filter.From) > 90*24*time.Hour {
		return out, corestore.ErrInvalidPage
	}
	projects := make([]string, len(filter.Projects))
	for i, id := range filter.Projects {
		projects[i] = string(id)
	}
	if err := s.activityAttempts(ctx, filter, projects, &out); err != nil {
		return out, err
	}
	rows, err := s.db.Query(ctx, `SELECT w.project_id,w.name,w.terminal,w.dispatchable,w.operator_only,coalesce(c.sequence,0) FROM project_workflow_states w LEFT JOIN project_event_cursors c ON c.organization_id=w.organization_id AND c.project_id=w.project_id WHERE w.organization_id=$1 AND w.project_id=ANY($2) ORDER BY w.project_id,w.board_rank`, s.organization, projects)
	if err != nil {
		return out, mapError(ctx, err)
	}
	var project tracker.ProjectID
	var state tracker.NativeState
	var sequence int64
	if _, err := pgx.ForEachRow(rows, []any{&project, &state.Name, &state.Terminal, &state.Dispatchable, &state.OperatorOnly, &sequence}, func() error {
		out.States[project] = append(out.States[project], state)
		out.Sequences[string(project)] = sequence
		return nil
	}); err != nil {
		return out, mapError(ctx, err)
	}
	issues := map[int64]activitystore.Issue{}
	for _, attempt := range out.Attempts {
		issues[attempt.IssueID] = activitystore.Issue{ID: attempt.IssueID, ProjectID: attempt.ProjectID, WorkItemID: attempt.WorkItemID, Number: attempt.Number, Title: attempt.Title, ProjectName: attempt.ProjectName}
	}
	rows, err = s.db.Query(ctx, `SELECT id FROM issues WHERE organization_id=$1 AND native_updated_at>=$2 ORDER BY native_updated_at DESC,id DESC LIMIT $3`, s.organization, filter.From, activitystore.PopulationLimit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	recent, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return out, mapError(ctx, err)
	}
	out.IssuesPartial = len(recent) > activitystore.PopulationLimit
	recent = recent[:min(len(recent), activitystore.PopulationLimit)]
	rows, err = s.db.Query(ctx, `WITH recent_issues AS MATERIALIZED (
 SELECT i.id,i.project_id,i.native_id,i.number,i.title,i.workflow_state_name,i.native_created_at,i.native_updated_at FROM (SELECT id FROM unnest($3::bigint[]) AS recent(id) LIMIT $4) recent
 CROSS JOIN LATERAL (SELECT id,project_id,native_id,number,title,workflow_state_name,native_created_at,native_updated_at,archived,actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.id=recent.id LIMIT 1) i
 WHERE i.project_id=ANY($2) AND NOT i.archived AND ($5='' OR i.actor_principal_id=$5)
) SELECT i.id,i.project_id,i.native_id,i.number,i.title,p.name,coalesce(i.workflow_state_name,''),i.native_created_at
 FROM recent_issues i JOIN projects p ON p.organization_id=$1 AND p.id=i.project_id
 JOIN project_workflow_states w ON w.organization_id=$1 AND w.project_id=i.project_id AND w.name=i.workflow_state_name
 WHERE p.deleted_at IS NULL AND w.terminal ORDER BY i.native_updated_at DESC,i.native_id LIMIT $6`, s.organization, projects, recent, activitystore.PopulationLimit, filter.Principal, filter.Limit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	var issue activitystore.Issue
	count := 0
	if _, err := pgx.ForEachRow(rows, []any{&issue.ID, &issue.ProjectID, &issue.WorkItemID, &issue.Number, &issue.Title, &issue.ProjectName, &issue.State, &issue.CreatedAt}, func() error {
		count++
		if count <= filter.Limit {
			issues[issue.ID] = issue
		} else {
			out.IssuesPartial = true
		}
		return nil
	}); err != nil {
		return out, mapError(ctx, err)
	}
	ids := make([]int64, 0, len(issues))
	for id := range issues {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err = s.db.Query(ctx, `SELECT id,project_id,native_id,coalesce(workflow_state_name,''),native_created_at,native_updated_at FROM issues WHERE organization_id=$1 AND id=ANY($2) ORDER BY native_updated_at DESC,native_id LIMIT $3`, s.organization, ids, filter.Limit)
	if err != nil {
		return out, mapError(ctx, err)
	}
	selectedIDs := make([]int64, 0, min(len(ids), filter.Limit))
	for rows.Next() {
		var id int64
		var project tracker.ProjectID
		var item tracker.NativeWorkItemID
		var state string
		var created, updated time.Time
		if err := rows.Scan(&id, &project, &item, &state, &created, &updated); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		issue := issues[id]
		issue.State = state
		issue.CreatedAt, issue.UpdatedAt = created, updated
		issues[id] = issue
		selectedIDs = append(selectedIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, mapError(ctx, err)
	}
	if len(selectedIDs) < len(ids) {
		out.IssuesPartial = true
	}
	ids = selectedIDs
	selectedProjects := make([]string, len(ids))
	selectedItems := make([]string, len(ids))
	for n, id := range ids {
		selectedProjects[n] = string(issues[id].ProjectID)
		selectedItems[n] = string(issues[id].WorkItemID)
	}
	rows, err = s.db.Query(ctx, `SELECT i.id,e.sequence,e.type,e.from_state,e.to_state,e.recorded_at
 FROM (SELECT * FROM unnest($2::bigint[],$5::text[],$6::text[]) AS items(id,project_id,native_id) LIMIT $7)i
 CROSS JOIN LATERAL(SELECT sequence,type,from_state,to_state,recorded_at FROM collaboration_events e
 WHERE e.organization_id=$1 AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type IN('issue.created','workflow.transitioned') AND e.recorded_at<=$3
 ORDER BY e.sequence DESC LIMIT $4)e ORDER BY i.id,e.sequence DESC`, s.organization, ids, filter.Now, activitystore.HistoryLimit+1, selectedProjects, selectedItems, len(ids))
	if err != nil {
		return out, mapError(ctx, err)
	}
	historyCount := map[int64]int{}
	var transition activitystore.Transition
	var kind string
	if _, err := pgx.ForEachRow(rows, []any{&transition.IssueID, &transition.Sequence, &kind, &transition.From, &transition.To, &transition.At}, func() error {
		historyCount[transition.IssueID]++
		switch {
		case historyCount[transition.IssueID] > activitystore.HistoryLimit:
			issue := issues[transition.IssueID]
			issue.Partial = true
			issues[transition.IssueID] = issue
			out.IssuesPartial = true
		case kind == "issue.created":
			out.Created[transition.IssueID] = true
		default:
			out.Transitions = append(out.Transitions, transition)
		}
		return nil
	}); err != nil {
		return out, mapError(ctx, err)
	}
	known := map[string]int{}
	for i, a := range out.Attempts {
		known[a.ID] = i
	}
	rows, err = s.db.Query(ctx, `SELECT a.id,a.issue_id,coalesce(r.runner_id,''),l.session_id,coalesce(pr.requirement_role,''),a.status,a.started_at,CASE WHEN a.status='running' THEN least(coalesce(l.released_at,l.expires_at),l.expires_at) ELSE a.updated_at END,a.status='running' AND l.released_at IS NULL AND l.renewed_at<=$3 AND l.expires_at>$3
 FROM (SELECT id FROM unnest($2::bigint[]) AS items(id) LIMIT $5)i
 CROSS JOIN LATERAL(SELECT organization_id,id,issue_id,lease_id,status,started_at,updated_at,fencing_token FROM native_attempts a WHERE a.organization_id=$1 AND a.issue_id=i.id ORDER BY a.fencing_token DESC LIMIT $4)a
 JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id LEFT JOIN runner_lease_assignments r ON r.organization_id=a.organization_id AND r.lease_id=a.lease_id LEFT JOIN provider_reservations pr ON pr.organization_id=a.organization_id AND pr.lease_id=a.lease_id ORDER BY i.id,a.fencing_token DESC`, s.organization, ids, filter.Now, activitystore.HistoryLimit+1, len(ids))
	if err != nil {
		return out, mapError(ctx, err)
	}
	attemptCount := map[int64]int{}
	for rows.Next() {
		var a activitystore.Attempt
		if err := rows.Scan(&a.ID, &a.IssueID, &a.RunnerID, &a.SessionID, &a.Role, &a.Status, &a.StartedAt, &a.FinishedAt, &a.Live); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		attemptCount[a.IssueID]++
		if attemptCount[a.IssueID] > activitystore.HistoryLimit {
			issue := issues[a.IssueID]
			issue.Partial = true
			issues[a.IssueID] = issue
			out.IssuesPartial = true
			continue
		}
		if _, ok := known[a.ID]; !ok {
			issue := issues[a.IssueID]
			a.ProjectID, a.WorkItemID, a.Number, a.Title, a.ProjectName = issue.ProjectID, issue.WorkItemID, issue.Number, issue.Title, issue.ProjectName
			known[a.ID] = len(out.Attempts)
			out.Attempts = append(out.Attempts, a)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, mapError(ctx, err)
	}
	slices.SortStableFunc(out.Attempts, func(a, b activitystore.Attempt) int {
		if a.Live != b.Live {
			if a.Live {
				return -1
			}
			return 1
		}
		if a.Selected != b.Selected {
			if a.Selected {
				return -1
			}
			return 1
		}
		return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(a.ID, b.ID))
	})
	attemptIDs := []string{}
	for _, a := range out.Attempts[:min(len(out.Attempts), activitystore.ObjectLimit)] {
		attemptIDs = append(attemptIDs, a.ID)
	}
	byAttempt := map[string]eventstore.Event{}
	if len(attemptIDs) > 0 {
		events, err := s.queries.ActivityLatestRunEvents(ctx, pgquery.ActivityLatestRunEventsParams{OrganizationID: s.organization, AttemptIds: attemptIDs})
		if err != nil {
			return out, mapError(ctx, err)
		}
		for _, row := range events {
			byAttempt[row.AttemptID] = eventRecord(row)
		}
	}
	links, err := s.queries.ActivityIssueLinks(ctx, pgquery.ActivityIssueLinksParams{OrganizationID: s.organization, IssueIds: ids, ProjectIds: selectedProjects, WorkItemIds: selectedItems, IssueCount: int32(len(ids))})
	if err != nil {
		return out, mapError(ctx, err)
	}
	for _, link := range links {
		issue := issues[link.ID]
		if link.WorkspaceID != nil {
			issue.WorkspaceIDs = []string{*link.WorkspaceID}
		}
		issue.ChangeID = link.ChangeID
		issue.PublicationURL = link.PublicationUrl
		issues[link.ID] = issue
	}

	for i := range out.Attempts {
		if event, ok := byAttempt[out.Attempts[i].ID]; ok && event.RecordedAt.Compare(filter.Now) <= 0 {
			out.Attempts[i].Event = &event
		}
	}
	for _, id := range ids {
		out.Issues = append(out.Issues, issues[id])
	}
	return out, nil
}

func (s *session) activityAttempts(ctx context.Context, f activitystore.Filter, projects []string, out *activitystore.Snapshot) error {
	const limit = activitystore.PopulationLimit
	f.From = f.From.Truncate(time.Microsecond)
	f.To = f.To.Truncate(time.Microsecond)
	var running []string
	var overlapping int
	err := s.db.QueryRow(ctx, `SELECT coalesce(array_agg(id ORDER BY id),'{}'),count(*) FILTER (WHERE updated_at>=$2 AND started_at<$3)
 FROM (SELECT id,updated_at,started_at FROM native_attempts WHERE organization_id=$1 AND status='running' ORDER BY id LIMIT $4) candidates`,
		s.organization, f.From, f.To, limit+1).Scan(&running, &overlapping)
	if err != nil {
		return mapError(ctx, err)
	}
	terminalIDs := func(condition string, args ...any) ([]string, error) {
		rows, err := s.db.Query(ctx, `SELECT id FROM native_attempts WHERE organization_id=$1 AND status<>'running' AND updated_at>=$2 `+condition+` ORDER BY updated_at DESC,id DESC LIMIT $3`, args...)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		return ids, mapError(ctx, err)
	}
	finished, err := terminalIDs("AND updated_at<$4", s.organization, f.From, limit+1, f.To)
	if err != nil {
		return err
	}
	out.Partial = len(running) > limit || len(finished) > limit
	if len(projects) == 0 {
		return nil
	}
	later, err := terminalIDs("", s.organization, f.To, limit+1)
	if err != nil {
		return err
	}
	out.DurationsPartial = len(later)+len(finished)+overlapping > limit || len(running) > limit
	running = running[:min(len(running), limit)]
	later = later[:min(len(later), limit)]
	finished = finished[:min(len(finished), limit)]
	durationIDs := make(map[string]bool)
	for _, id := range later {
		durationIDs[id] = true
	}
	for _, id := range finished[:min(len(finished), limit-len(later))] {
		durationIDs[id] = true
	}
	ids := slices.Concat(running, finished, later)
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.Query(ctx, `WITH attempt_items AS MATERIALIZED (
 SELECT a.*,i.project_id,i.native_id,i.number,i.title
 FROM (SELECT id FROM unnest($2::text[]) AS wanted(id) LIMIT $7) selected
 CROSS JOIN LATERAL (SELECT id,issue_id,lease_id,status,started_at,updated_at FROM native_attempts a WHERE a.organization_id=$1 AND a.id=selected.id LIMIT 1) a
 CROSS JOIN LATERAL (SELECT project_id,native_id,number,title,archived,actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.id=a.issue_id LIMIT 1) i
 WHERE i.project_id=ANY($3) AND NOT i.archived AND ($5='' OR i.actor_principal_id=$5)
) SELECT a.id,a.issue_id,a.project_id,a.native_id,a.number,a.title,p.name,coalesce(r.runner_id,''),l.session_id,coalesce(pr.requirement_role,''),a.status,a.started_at,
 CASE WHEN a.status='running' THEN least(coalesce(l.released_at,l.expires_at),l.expires_at) ELSE a.updated_at END AS finished_at,
 a.status='running' AND l.released_at IS NULL AND l.renewed_at<=$4 AND l.expires_at>$4,
 coalesce(f.role,''),coalesce(f.duration_seconds,0),coalesce(f.phase_timing_status,'')<>'complete'
 FROM attempt_items a JOIN projects p ON p.organization_id=$1 AND p.id=a.project_id
 JOIN leases l ON l.organization_id=$1 AND l.lease_id=a.lease_id
 LEFT JOIN runner_lease_assignments r ON r.organization_id=$1 AND r.lease_id=a.lease_id
 LEFT JOIN provider_reservations pr ON pr.organization_id=$1 AND pr.lease_id=a.lease_id
 LEFT JOIN LATERAL (SELECT latest_event_id FROM report_attempt_facts af WHERE af.organization_id=$1 AND af.attempt_id=a.id AND a.status<>'running' LIMIT 1) af ON true
 LEFT JOIN report_event_facts f ON f.organization_id=$1 AND f.event_id=af.latest_event_id
 WHERE p.deleted_at IS NULL AND ($6='' OR r.runner_id=$6)
 ORDER BY finished_at DESC,a.id DESC`, s.organization, ids, projects, f.Now, f.Principal, f.RunnerID, len(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	var runningCount, finishedCount int
	for rows.Next() {
		var a activitystore.Attempt
		var d activitystore.Duration
		if err := rows.Scan(&a.ID, &a.IssueID, &a.ProjectID, &a.WorkItemID, &a.Number, &a.Title, &a.ProjectName, &a.RunnerID, &a.SessionID, &a.Role, &a.Status, &a.StartedAt, &a.FinishedAt, &a.Live, &d.Role, &d.Seconds, &d.Partial); err != nil {
			return mapError(ctx, err)
		}
		if durationIDs[a.ID] && a.StartedAt.Before(f.To) {
			d.ProjectID, d.AttemptID, d.Status = a.ProjectID, a.ID, a.Status
			out.Durations = append(out.Durations, d)
		}
		if !a.Live && (a.FinishedAt.Before(f.From) || !a.FinishedAt.Before(f.To)) {
			continue
		}
		count := &finishedCount
		if a.Status == "running" {
			count = &runningCount
		}
		if *count == f.Limit {
			out.Partial = true
			continue
		}
		*count++
		a.Selected = true
		out.Attempts = append(out.Attempts, a)
	}
	slices.SortFunc(out.Durations, func(a, b activitystore.Duration) int { return cmp.Compare(a.AttemptID, b.AttemptID) })
	return mapError(ctx, rows.Err())
}
