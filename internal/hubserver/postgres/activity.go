package postgres

import (
	"cmp"
	"context"
	"slices"
	"time"

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
	durations, clipped, err := s.activityDurationPopulation(ctx, filter, projects)
	if err != nil {
		return out, err
	}
	out.Durations, out.DurationsPartial = durations, clipped
	rows, err := s.db.Query(ctx, `SELECT w.project_id,w.name,w.terminal,w.dispatchable,w.operator_only,coalesce(c.sequence,0) FROM project_workflow_states w LEFT JOIN project_event_cursors c ON c.organization_id=w.organization_id AND c.project_id=w.project_id WHERE w.organization_id=$1 AND w.project_id=ANY($2) ORDER BY w.project_id,w.board_rank`, s.organization, projects)
	if err != nil {
		return out, mapError(ctx, err)
	}
	for rows.Next() {
		var project tracker.ProjectID
		var state tracker.NativeState
		var sequence int64
		if err := rows.Scan(&project, &state.Name, &state.Terminal, &state.Dispatchable, &state.OperatorOnly, &sequence); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		out.States[project] = append(out.States[project], state)
		out.Sequences[string(project)] = sequence
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, mapError(ctx, err)
	}
	issues := map[int64]activitystore.Issue{}
	for _, running := range []bool{true, false} {
		attempts, partial, err := s.activityCandidates(ctx, filter, projects, running)
		if err != nil {
			return out, err
		}
		out.Partial = out.Partial || partial
		for _, attempt := range attempts {
			out.Attempts = append(out.Attempts, attempt)
			issues[attempt.IssueID] = activitystore.Issue{ID: attempt.IssueID, ProjectID: attempt.ProjectID, WorkItemID: attempt.WorkItemID, Number: attempt.Number, Title: attempt.Title, ProjectName: attempt.ProjectName}
		}
	}
	var population int
	err = s.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM issues WHERE organization_id=$1 AND native_updated_at>=$2 ORDER BY native_updated_at DESC,id DESC LIMIT $3) candidates`, s.organization, filter.From, activitystore.PopulationLimit+1).Scan(&population)
	if err != nil {
		return out, mapError(ctx, err)
	}
	out.IssuesPartial = out.IssuesPartial || population > activitystore.PopulationLimit
	rows, err = s.db.Query(ctx, `WITH recent AS MATERIALIZED(SELECT * FROM issues WHERE organization_id=$1 AND native_updated_at>=$3 ORDER BY native_updated_at DESC,id DESC LIMIT $4)
 SELECT i.id,i.project_id,i.native_id,i.number,i.title,p.name,coalesce(i.workflow_state_name,''),i.native_created_at FROM recent i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
 WHERE i.project_id=ANY($2) AND NOT i.archived AND p.deleted_at IS NULL AND w.terminal AND ($5='' OR i.actor_principal_id=$5) ORDER BY i.native_updated_at DESC,i.native_id LIMIT $6`, s.organization, projects, filter.From, activitystore.PopulationLimit, filter.Principal, filter.Limit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	count := 0
	for rows.Next() {
		var issue activitystore.Issue
		if err := rows.Scan(&issue.ID, &issue.ProjectID, &issue.WorkItemID, &issue.Number, &issue.Title, &issue.ProjectName, &issue.State, &issue.CreatedAt); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		count++
		if count <= filter.Limit {
			issues[issue.ID] = issue
		} else {
			out.IssuesPartial = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
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
	rows, err = s.db.Query(ctx, `SELECT i.id,e.sequence,e.type,e.from_state,e.to_state,e.recorded_at FROM issues i CROSS JOIN LATERAL(SELECT sequence,type,from_state,to_state,recorded_at FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type IN('issue.created','workflow.transitioned') AND e.recorded_at<=$3 ORDER BY e.sequence DESC LIMIT $4)e WHERE i.organization_id=$1 AND i.id=ANY($2) ORDER BY i.id,e.sequence DESC`, s.organization, ids, filter.Now, activitystore.HistoryLimit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	historyCount := map[int64]int{}
	for rows.Next() {
		var transition activitystore.Transition
		var kind string
		if err := rows.Scan(&transition.IssueID, &transition.Sequence, &kind, &transition.From, &transition.To, &transition.At); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		historyCount[transition.IssueID]++
		if historyCount[transition.IssueID] > activitystore.HistoryLimit {
			issue := issues[transition.IssueID]
			issue.Partial = true
			issues[transition.IssueID] = issue
			out.IssuesPartial = true
			continue
		}
		if kind == "issue.created" {
			out.Created[transition.IssueID] = true
		} else {
			out.Transitions = append(out.Transitions, transition)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, mapError(ctx, err)
	}
	known := map[string]int{}
	for i, a := range out.Attempts {
		known[a.ID] = i
	}
	rows, err = s.db.Query(ctx, `SELECT a.id,a.issue_id,i.project_id,i.native_id,i.number,i.title,p.name,coalesce(r.runner_id,''),l.session_id,coalesce(pr.requirement_role,''),a.status,a.started_at,CASE WHEN a.status='running' THEN least(coalesce(l.released_at,l.expires_at),l.expires_at) ELSE a.updated_at END,a.status='running' AND l.released_at IS NULL AND l.renewed_at<=$3 AND l.expires_at>$3
 FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id CROSS JOIN LATERAL(SELECT * FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id ORDER BY a.fencing_token DESC LIMIT $4)a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id LEFT JOIN runner_lease_assignments r ON r.organization_id=a.organization_id AND r.lease_id=a.lease_id LEFT JOIN provider_reservations pr ON pr.organization_id=a.organization_id AND pr.lease_id=a.lease_id WHERE i.organization_id=$1 AND i.id=ANY($2) ORDER BY i.id,a.fencing_token DESC`, s.organization, ids, filter.Now, activitystore.HistoryLimit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	attemptCount := map[int64]int{}
	for rows.Next() {
		a, err := scanActivityAttempt(rows)
		if err != nil {
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
	links, err := s.queries.ActivityIssueLinks(ctx, pgquery.ActivityIssueLinksParams{OrganizationID: s.organization, IssueIds: ids})
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

type activityRowScanner interface{ Scan(...any) error }

func scanActivityAttempt(row activityRowScanner) (activitystore.Attempt, error) {
	var a activitystore.Attempt
	err := row.Scan(&a.ID, &a.IssueID, &a.ProjectID, &a.WorkItemID, &a.Number, &a.Title, &a.ProjectName, &a.RunnerID, &a.SessionID, &a.Role, &a.Status, &a.StartedAt, &a.FinishedAt, &a.Live)
	return a, err
}

func (s *session) activityCandidates(ctx context.Context, f activitystore.Filter, projects []string, running bool) ([]activitystore.Attempt, bool, error) {
	condition := `status='running'`
	order := `id`
	args := []any{s.organization, projects, f.From, f.To, f.Now, activitystore.PopulationLimit, f.Principal, f.RunnerID, f.Limit + 1}
	if !running {
		condition = `status<>'running' AND updated_at>=$3 AND updated_at<$4`
		order = `updated_at DESC,id DESC`
	}
	countSQL := `SELECT count(*) FROM(SELECT id FROM native_attempts WHERE organization_id=$1 AND status='running' ORDER BY id LIMIT $2)candidates`
	countArgs := []any{s.organization, activitystore.PopulationLimit + 1}
	if !running {
		countSQL = `SELECT count(*) FROM(SELECT id FROM native_attempts WHERE organization_id=$1 AND status<>'running' AND updated_at>=$2 AND updated_at<$3 ORDER BY updated_at DESC,id DESC LIMIT $4)candidates`
		countArgs = []any{s.organization, f.From, f.To, activitystore.PopulationLimit + 1}
	}
	var count int
	err := s.db.QueryRow(ctx, countSQL, countArgs...).Scan(&count)
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	liveFilter := `a.status='running' AND l.released_at IS NULL AND l.renewed_at<=$5 AND l.expires_at>$5`
	finish := `CASE WHEN a.status='running' THEN least(coalesce(l.released_at,l.expires_at),l.expires_at) ELSE a.updated_at END`
	if running {
		liveFilter = `(` + liveFilter + ` OR (` + finish + `>=$3 AND ` + finish + `<$4))`
	}
	if !running {
		liveFilter = `NOT(a.status='running' AND l.released_at IS NULL AND l.renewed_at<=$5 AND l.expires_at>$5)`
	}
	rows, err := s.db.Query(ctx, `WITH selected AS MATERIALIZED(SELECT * FROM native_attempts WHERE organization_id=$1 AND `+condition+` ORDER BY `+order+` LIMIT $6)
 SELECT a.id,a.issue_id,i.project_id,i.native_id,i.number,i.title,p.name,coalesce(r.runner_id,''),l.session_id,coalesce(pr.requirement_role,''),a.status,a.started_at,`+finish+`,a.status='running' AND l.released_at IS NULL AND l.renewed_at<=$5 AND l.expires_at>$5
 FROM selected a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id LEFT JOIN runner_lease_assignments r ON r.organization_id=a.organization_id AND r.lease_id=a.lease_id LEFT JOIN provider_reservations pr ON pr.organization_id=a.organization_id AND pr.lease_id=a.lease_id WHERE i.project_id=ANY($2) AND NOT i.archived AND p.deleted_at IS NULL AND ($7='' OR i.actor_principal_id=$7) AND ($8='' OR r.runner_id=$8) AND `+liveFilter+` ORDER BY `+finish+` DESC,a.id DESC LIMIT $9`, args...)
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	defer rows.Close()
	out := []activitystore.Attempt{}
	partial := count > activitystore.PopulationLimit
	for rows.Next() {
		a, err := scanActivityAttempt(rows)
		if err != nil {
			return nil, false, mapError(ctx, err)
		}
		a.Selected = true
		if len(out) == f.Limit {
			partial = true
			continue
		}
		out = append(out, a)
	}
	return out, partial, mapError(ctx, rows.Err())
}

func (s *session) activityDurationPopulation(ctx context.Context, filter activitystore.Filter, projects []string) ([]activitystore.Duration, bool, error) {
	if len(projects) == 0 {
		return nil, false, nil
	}
	var count int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM native_attempts WHERE organization_id=$1 AND status<>'running' AND updated_at>=$2 ORDER BY updated_at DESC,id DESC LIMIT $3) candidates`, s.organization, filter.From, activitystore.PopulationLimit+1).Scan(&count); err != nil {
		return nil, false, mapError(ctx, err)
	}
	var running, examined int
	if err := s.db.QueryRow(ctx, `WITH candidates AS MATERIALIZED (
 SELECT updated_at,started_at FROM native_attempts WHERE organization_id=$1 AND status='running' ORDER BY id LIMIT $4
) SELECT count(*) FILTER (WHERE updated_at>=$2 AND started_at<$3),count(*) FROM candidates`, s.organization, filter.From, filter.To, activitystore.PopulationLimit+1).Scan(&running, &examined); err != nil {
		return nil, false, mapError(ctx, err)
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS MATERIALIZED (
 SELECT id,issue_id,lease_id,status,started_at FROM native_attempts WHERE organization_id=$1 AND status<>'running' AND updated_at>=$3 ORDER BY updated_at DESC,id DESC LIMIT $5
)
 SELECT i.project_id,a.id,coalesce(f.role,''),a.status,coalesce(f.duration_seconds,0),coalesce(f.phase_timing_status,'')<>'complete'
 FROM candidates a CROSS JOIN LATERAL (SELECT i.organization_id,i.project_id,i.archived,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.id=a.issue_id LIMIT 1) i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 LEFT JOIN runner_lease_assignments r ON r.organization_id=$1 AND r.lease_id=a.lease_id
 LEFT JOIN LATERAL (SELECT af.latest_event_id FROM report_attempt_facts af WHERE af.organization_id=$1 AND af.attempt_id=a.id LIMIT 1) af ON true
 LEFT JOIN report_event_facts f ON f.organization_id=$1 AND f.event_id=af.latest_event_id
 WHERE i.project_id=ANY($2) AND NOT i.archived AND p.deleted_at IS NULL AND a.started_at<$4 AND ($6='' OR i.actor_principal_id=$6) AND ($7='' OR r.runner_id=$7)
 ORDER BY a.id`, s.organization, projects, filter.From, filter.To, activitystore.PopulationLimit, filter.Principal, filter.RunnerID)
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	defer rows.Close()
	out := []activitystore.Duration{}
	for rows.Next() {
		var fact activitystore.Duration
		if err := rows.Scan(&fact.ProjectID, &fact.AttemptID, &fact.Role, &fact.Status, &fact.Seconds, &fact.Partial); err != nil {
			return nil, false, mapError(ctx, err)
		}
		out = append(out, fact)
	}
	return out, count+running > activitystore.PopulationLimit || examined > activitystore.PopulationLimit, mapError(ctx, rows.Err())
}
