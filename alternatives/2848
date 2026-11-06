package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxOverviewFactLimit = 5000

var _ reportstore.Reader = (*session)(nil)
var _ reportstore.OpenReader = (*session)(nil)

func (s *session) ReadOpenFacts(ctx context.Context, query reportstore.OpenQuery) ([]reportstore.OpenFact, error) {
	if len(query.Projects) == 0 || query.Limit < 1 || query.Limit > maxOverviewFactLimit {
		return nil, errors.New("invalid PostgreSQL open report query")
	}
	projects := make([]string, len(query.Projects))
	for i, project := range query.Projects {
		if project == "" {
			return nil, errors.New("invalid PostgreSQL report project")
		}
		projects[i] = string(project)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.project_id,i.native_id,i.title,state.name,coalesce(ws.terminal,false),
	(SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state=state.name AND e.recorded_at <= $3 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1),
	CASE WHEN EXISTS (SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.created' AND e.recorded_at <= $3) THEN i.created_at END,
	(SELECT min(e.recorded_at) FROM collaboration_events e JOIN project_workflow_states started ON started.organization_id=e.organization_id AND started.project_id=e.project_id AND started.name=e.to_state WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.recorded_at <= $3 AND started.dispatchable AND NOT started.terminal AND e.to_state NOT IN ('Backlog','Triage','Blocked','Human Review'))
FROM issues i
LEFT JOIN LATERAL (SELECT e.to_state AS name FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.recorded_at <= $3 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1) previous ON true
LEFT JOIN LATERAL (SELECT e.from_state AS name FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.recorded_at > $3 ORDER BY e.recorded_at,e.sequence LIMIT 1) next ON previous.name IS NULL
LEFT JOIN LATERAL (SELECT coalesce(previous.name,next.name,CASE WHEN i.native_updated_at <= $3 THEN i.workflow_state_name END,'') AS name) state ON true
LEFT JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=state.name
WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND NOT i.archived AND i.created_at < $3 AND (state.name='' OR NOT coalesce(ws.terminal,false)) AND ($4='' OR i.actor_principal_id=$4)
  AND (coalesce(cardinality($5::text[]),0)=0 OR i.issue_type=ANY($5::text[]))
  AND (coalesce(cardinality($6::integer[]),0)=0 OR i.priority=ANY($6::integer[]))
  AND (coalesce(cardinality($7::text[]),0)=0 OR i.actor_principal_id=ANY($7::text[]))
  AND (coalesce(cardinality($8::text[]),0)=0 OR EXISTS (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($8::text[])))
  AND (coalesce(cardinality($9::text[]),0)=0 OR EXISTS (SELECT 1 FROM native_attempts a JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at <= $3 AND la.runner_id=ANY($9::text[])))
ORDER BY i.project_id,i.native_id LIMIT $10`, s.organization, pq.Array(projects), query.At.UTC(), query.Principal, pq.Array(query.Filter.Types), pq.Array(query.Filter.Priorities), pq.Array(query.Filter.FiledBy), pq.Array(query.Filter.Labels), pq.Array(query.Filter.Runners), query.Limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	facts := make([]reportstore.OpenFact, 0, min(query.Limit, 128))
	for rows.Next() {
		var fact reportstore.OpenFact
		var project, workItem string
		var entered, created, firstWorked sql.NullTime
		if err := rows.Scan(&project, &workItem, &fact.Title, &fact.State, &fact.Terminal, &entered, &created, &firstWorked); err != nil {
			return nil, mapError(ctx, err)
		}
		fact.ProjectID, fact.WorkItemID = tracker.ProjectID(project), tracker.NativeWorkItemID(workItem)
		if entered.Valid {
			at := entered.Time.UTC()
			fact.EnteredAt = &at
		} else if created.Valid {
			at := created.Time.UTC()
			fact.EnteredAt = &at
		}
		if firstWorked.Valid {
			at := firstWorked.Time.UTC()
			fact.FirstWorkedAt = &at
		}
		facts = append(facts, fact)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(facts) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return facts, nil
}

func (s *session) ReadOverviewFacts(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.OverviewFact, error) {
	if len(query.Projects) == 0 || query.Limit < 1 || query.Limit > maxOverviewFactLimit || !query.From.Before(query.To) {
		return nil, errors.New("invalid PostgreSQL report query")
	}
	projects := make([]string, len(query.Projects))
	for i, project := range query.Projects {
		if project == "" {
			return nil, errors.New("invalid PostgreSQL report project")
		}
		projects[i] = string(project)
	}
	statement := `WITH candidates AS (
	 SELECT i.organization_id,i.id,i.project_id,i.native_id,i.title,i.created_at,i.workflow_state_name,i.actor_kind,i.actor_principal_id,i.issue_type,i.priority
 FROM issues i
 WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[])
   AND ($3='' OR i.actor_principal_id=$3)
   AND (i.created_at >= $4 AND i.created_at < $5 OR EXISTS (
     SELECT 1 FROM collaboration_events e
     JOIN project_workflow_states ws ON ws.organization_id=e.organization_id AND ws.project_id=e.project_id AND ws.name=e.to_state AND ws.terminal
     WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
       AND e.type='workflow.transitioned' AND e.recorded_at >= $4 AND e.recorded_at < $5
   ) OR EXISTS (
     SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at >= $4 AND a.started_at < $5
   ) OR EXISTS (
     SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
       AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.recorded_at >= $4 AND e.recorded_at < $5
   ) OR EXISTS (
     SELECT 1 FROM change_requests c JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id
     WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id
       AND c.landed_at >= $4 AND c.landed_at < $5 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
   ))
	   AND (coalesce(cardinality($7::text[]),0)=0 OR i.issue_type=ANY($7::text[]))
	   AND (coalesce(cardinality($8::integer[]),0)=0 OR i.priority=ANY($8::integer[]))
	   AND (coalesce(cardinality($9::text[]),0)=0 OR i.actor_principal_id=ANY($9::text[]))
	   AND (coalesce(cardinality($10::text[]),0)=0 OR EXISTS (
	     SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($10::text[])
   ))
   AND (coalesce(cardinality($11::text[]),0)=0 OR EXISTS (
     SELECT 1 FROM native_attempts a JOIN runner_lease_assignments r ON r.organization_id=a.organization_id AND r.lease_id=a.lease_id
     WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at < $5 AND a.updated_at >= $4 AND r.runner_id=ANY($11::text[])
   ))
 AND (coalesce(cardinality($13::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id AND e.recorded_at >= $4 AND e.recorded_at < $5 AND f.model=ANY($13::text[])))
 AND (coalesce(cardinality($14::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id AND e.recorded_at >= $4 AND e.recorded_at < $5 AND f.role=ANY($14::text[])))
 ORDER BY i.created_at,i.project_id,i.native_id LIMIT $12
)
SELECT i.project_id,i.native_id,i.title,i.created_at,coalesce(i.workflow_state_name,''),coalesce(i.actor_kind,''),coalesce(i.actor_principal_id,''),
 completion.recorded_at,coalesce(completion.to_state,''),coalesce(completion.actor_kind,''),coalesce(completion.actor_principal_id,''),
 previous_completion.recorded_at,coalesce(previous_completion.to_state,''),coalesce(previous_completion.actor_kind,''),coalesce(previous_completion.actor_principal_id,''),
 EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=(SELECT id FROM issues WHERE organization_id=i.organization_id AND project_id=i.project_id AND native_id=i.native_id) AND a.started_at >= $6 AND a.started_at < $5),
 EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=(SELECT id FROM issues WHERE organization_id=i.organization_id AND project_id=i.project_id AND native_id=i.native_id) AND a.started_at >= $4 AND a.started_at < $6),
 EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.recorded_at >= $6 AND e.recorded_at < $5),
 EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.recorded_at >= $4 AND e.recorded_at < $6),
 EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=(SELECT id FROM issues WHERE organization_id=i.organization_id AND project_id=i.project_id AND native_id=i.native_id) AND a.started_at >= $4 AND a.started_at < $5)
 ,EXISTS(SELECT 1 FROM change_requests c JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.landed_at >= $6 AND c.landed_at < $5 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
 ,EXISTS(SELECT 1 FROM change_requests c JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.landed_at >= $4 AND c.landed_at < $6 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
FROM candidates i
LEFT JOIN LATERAL (
 SELECT e.recorded_at,e.to_state,e.actor_kind,e.actor_principal_id,ws.terminal
 FROM collaboration_events e LEFT JOIN project_workflow_states ws
   ON ws.organization_id=e.organization_id AND ws.project_id=e.project_id AND ws.name=e.to_state
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
   AND e.type='workflow.transitioned' AND e.recorded_at < $5
 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1
) completion ON completion.terminal
LEFT JOIN LATERAL (
 SELECT e.recorded_at,e.to_state,e.actor_kind,e.actor_principal_id,ws.terminal
 FROM collaboration_events e LEFT JOIN project_workflow_states ws
   ON ws.organization_id=e.organization_id AND ws.project_id=e.project_id AND ws.name=e.to_state
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
   AND e.type='workflow.transitioned' AND e.recorded_at < $6
 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1
) previous_completion ON previous_completion.terminal
ORDER BY i.created_at,i.project_id,i.native_id`
	rows, err := s.db.QueryContext(ctx, statement, s.organization, pq.Array(projects), query.Principal, query.From.UTC(), query.To.UTC(), query.Split.UTC(), pq.Array(query.Filter.Types), pq.Array(query.Filter.Priorities), pq.Array(query.Filter.FiledBy), pq.Array(query.Filter.Labels), pq.Array(query.Filter.Runners), query.Limit+1, pq.Array(query.Filter.Models), pq.Array(query.Filter.Stages))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	facts := make([]reportstore.OverviewFact, 0, min(query.Limit, 128))
	for rows.Next() {
		var fact reportstore.OverviewFact
		var closedAt, previousClosedAt sql.NullTime
		var project, workItem string
		if err := rows.Scan(&project, &workItem, &fact.Title, &fact.CreatedAt, &fact.State, &fact.FilerKind, &fact.FilerID, &closedAt, &fact.CompletedState, &fact.CompletedKind, &fact.CompletedID, &previousClosedAt, &fact.PreviousCompletedState, &fact.PreviousCompletedKind, &fact.PreviousCompletedID, &fact.Worked, &fact.PreviousWorked, &fact.Reworked, &fact.PreviousReworked, &fact.ActivityObserved, &fact.ChangesLanded, &fact.PreviousChangesLanded); err != nil {
			return nil, mapError(ctx, err)
		}
		fact.ProjectID, fact.WorkItemID = tracker.ProjectID(project), tracker.NativeWorkItemID(workItem)
		if closedAt.Valid {
			at := closedAt.Time.UTC()
			fact.CompletedAt = &at
			if fact.CompletedState == "Done" {
				seconds := at.Sub(fact.CreatedAt).Seconds()
				fact.CycleSeconds = &seconds
			}
		}
		if previousClosedAt.Valid {
			at := previousClosedAt.Time.UTC()
			fact.PreviousCompletedAt = &at
			if fact.PreviousCompletedState == "Done" {
				seconds := at.Sub(fact.CreatedAt).Seconds()
				fact.PreviousCycleSeconds = &seconds
			}
		}
		facts = append(facts, fact)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(facts) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return facts, nil
}

func (s *session) ReadFacets(ctx context.Context, projects []tracker.ProjectID, from, to time.Time, principal string, limit int) (reportstore.Facets, error) {
	if len(projects) == 0 || len(projects) > 32 || limit < 1 || limit > 5000 || !from.Before(to) {
		return reportstore.Facets{}, errors.New("invalid PostgreSQL report facets query")
	}
	out := reportstore.Facets{Labels: []string{}, FiledBy: []reportstore.Facet{}, Runners: []reportstore.Facet{}, Models: []string{}, Stages: []string{}, Unavailable: []string{}}
	ids := []int64{}
	filers := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,coalesce(i.actor_principal_id,''),coalesce(nullif(t.name,''),i.actor_principal_id,'') FROM issues i LEFT JOIN api_tokens t ON t.organization_id=i.organization_id AND t.id=i.actor_principal_id WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND ($3='' OR i.actor_principal_id=$3) ORDER BY i.id LIMIT 5001`, s.organization, pq.Array(projects), principal)
	if err != nil {
		return out, mapError(ctx, err)
	}
	for rows.Next() {
		var id int64
		var actor, name string
		if err := rows.Scan(&id, &actor, &name); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		ids = append(ids, id)
		if actor != "" {
			filers[actor] = name
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, mapError(ctx, err)
	}
	if len(ids) > 5000 {
		return out, reportstore.ErrPopulationExceeded
	}
	labels := map[string]bool{}
	rows, err = s.db.QueryContext(ctx, `SELECT l.label FROM issues i JOIN issue_labels l ON l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id WHERE i.organization_id=$1 AND i.id=ANY($2::bigint[]) ORDER BY i.id,l.label LIMIT 100001`, s.organization, pq.Array(ids))
	if err != nil {
		return out, mapError(ctx, err)
	}
	count := 0
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			rows.Close()
			return out, mapError(ctx, err)
		}
		count++
		labels[label] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, mapError(ctx, err)
	}
	if count > 100000 {
		return out, reportstore.ErrPopulationExceeded
	}
	attempts, err := s.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: projects, Principal: principal, From: from, To: to, Limit: 50000})
	if err != nil {
		return out, err
	}
	models, stages := map[string]bool{}, map[string]bool{}
	runners := map[string]string{}
	for _, a := range attempts {
		if a.Model != "" {
			models[a.Model] = true
		}
		if a.Role != "" {
			stages[a.Role] = true
		}
		if a.RunnerID != "" {
			runners[a.RunnerID] = a.RunnerName
		}
	}
	if len(labels) > limit || len(filers) > limit || len(models) > limit || len(stages) > limit || len(runners) > limit {
		return out, reportstore.ErrPopulationExceeded
	}
	for v := range labels {
		out.Labels = append(out.Labels, v)
	}
	for id, name := range filers {
		out.FiledBy = append(out.FiledBy, reportstore.Facet{ID: id, Name: name})
	}
	for id, name := range runners {
		out.Runners = append(out.Runners, reportstore.Facet{ID: id, Name: name})
	}
	for v := range models {
		out.Models = append(out.Models, v)
	}
	for v := range stages {
		out.Stages = append(out.Stages, v)
	}
	slices.Sort(out.Labels)
	slices.Sort(out.Models)
	slices.Sort(out.Stages)
	slices.SortFunc(out.FiledBy, func(a, b reportstore.Facet) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Runners, func(a, b reportstore.Facet) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *session) ReadAttemptFacts(ctx context.Context, query reportstore.AttemptQuery) ([]reportstore.AttemptFact, error) {
	if len(query.Projects) == 0 || query.Limit < 1 || query.Limit > 50000 || !query.From.Before(query.To) {
		return nil, errors.New("invalid PostgreSQL report attempt query")
	}
	projects := make([]string, len(query.Projects))
	for i, project := range query.Projects {
		if project == "" {
			return nil, errors.New("invalid PostgreSQL report project")
		}
		projects[i] = string(project)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.project_id,i.native_id,a.id,coalesce(r.runner_id,''),coalesce(r.display_name,''),coalesce(nullif(f.run_status,''),'running'),a.started_at,coalesce(f.recorded_at,a.started_at),coalesce(f.role,''),coalesce(f.model,''),coalesce(f.effort,''),f.event_id IS NOT NULL,coalesce(f.failed,false),coalesce(f.failure_class,''),coalesce(f.failure_kind,''),coalesce(f.landing_receipt,false),coalesce(f.landed,false),coalesce(f.landing_change_id,''),coalesce(f.duration_seconds,0)
FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
LEFT JOIN report_attempt_facts af ON af.organization_id=a.organization_id AND af.attempt_id=a.id
LEFT JOIN collaboration_events outcome ON outcome.organization_id=af.organization_id AND outcome.id=af.outcome_event_id
LEFT JOIN collaboration_events latest ON latest.organization_id=af.organization_id AND latest.id=af.latest_event_id
LEFT JOIN collaboration_events first ON first.organization_id=af.organization_id AND first.id=af.first_event_id
LEFT JOIN LATERAL(SELECT CASE WHEN outcome.recorded_at<$5 THEN outcome.id WHEN latest.recorded_at<$5 THEN latest.id WHEN first.recorded_at<$5 THEN first.id END AS id) chosen ON true
LEFT JOIN LATERAL(SELECT rf.*,e.recorded_at FROM collaboration_events e JOIN report_event_facts rf ON rf.organization_id=e.organization_id AND rf.event_id=e.id WHERE e.organization_id=a.organization_id AND e.id=chosen.id) f ON true
LEFT JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id
LEFT JOIN runner_identities r ON r.organization_id=la.organization_id AND r.runner_id=la.runner_id
WHERE a.organization_id=$1 AND i.project_id=ANY($2::text[]) AND ($3='' OR i.actor_principal_id=$3)
  AND a.started_at < $5 AND (f.recorded_at >= $4 OR coalesce(f.run_status,'running')='running' OR EXISTS(SELECT 1 FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.attempt_id=a.id AND u.period>=$4 AND u.period<$5) OR EXISTS(SELECT 1 FROM report_usage_current u WHERE u.organization_id=a.organization_id AND u.attempt_id=a.id AND u.period>=$4 AND u.period<$5))
  AND (coalesce(cardinality($6::text[]),0)=0 OR i.issue_type=ANY($6::text[]))
  AND (coalesce(cardinality($7::integer[]),0)=0 OR i.priority=ANY($7::integer[]))
  AND (coalesce(cardinality($8::text[]),0)=0 OR i.actor_principal_id=ANY($8::text[]))
  AND (coalesce(cardinality($9::text[]),0)=0 OR EXISTS (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($9::text[])))
  AND (coalesce(cardinality($10::text[]),0)=0 OR r.runner_id=ANY($10::text[]))
AND (coalesce(cardinality($12::text[]),0)=0 OR f.model=ANY($12::text[]))
AND (coalesce(cardinality($13::text[]),0)=0 OR f.role=ANY($13::text[]))
ORDER BY a.updated_at,a.id LIMIT $11`, s.organization, pq.Array(projects), query.Principal, query.From.UTC(), query.To.UTC(), pq.Array(query.Filter.Types), pq.Array(query.Filter.Priorities), pq.Array(query.Filter.FiledBy), pq.Array(query.Filter.Labels), pq.Array(query.Filter.Runners), query.Limit+1, pq.Array(query.Filter.Models), pq.Array(query.Filter.Stages))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	attempts := make([]reportstore.AttemptFact, 0, min(query.Limit, 128))
	for rows.Next() {
		var fact reportstore.AttemptFact
		var project, workItem string
		if err := rows.Scan(&project, &workItem, &fact.AttemptID, &fact.RunnerID, &fact.RunnerName, &fact.Status, &fact.StartedAt, &fact.OutcomeAt, &fact.Role, &fact.Model, &fact.Effort, &fact.ProjectionKnown, &fact.Failed, &fact.FailureClass, &fact.FailureKind, &fact.LandingReceipt, &fact.Landed, &fact.LandingChangeID, &fact.DurationSeconds); err != nil {
			return nil, mapError(ctx, err)
		}
		fact.UpdatedAt = fact.OutcomeAt
		fact.ProjectID, fact.WorkItemID = tracker.ProjectID(project), tracker.NativeWorkItemID(workItem)
		attempts = append(attempts, fact)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(attempts) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return attempts, nil
}

func (s *session) HasPriorData(ctx context.Context, projects []tracker.ProjectID, before time.Time, principal string) (bool, error) {
	if len(projects) == 0 {
		return false, errors.New("invalid PostgreSQL report projects")
	}
	projectIDs := make([]string, len(projects))
	for i, project := range projects {
		projectIDs[i] = string(project)
	}
	var recorded bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE organization_id=$1 AND project_id=ANY($2::text[]) AND ($3='' OR actor_principal_id=$3) AND created_at<$4)`, s.organization, pq.Array(projectIDs), principal, before.UTC()).Scan(&recorded)
	return recorded, mapError(ctx, err)
}
