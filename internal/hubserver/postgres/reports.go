package postgres

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	maxOverviewFactLimit     = 5000
	maxOverviewCandidateRows = 50000
	maxReportProjects        = 1000
	maxReportStateNames      = 1000
)

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
	if len(projects) > maxReportProjects {
		return nil, reportstore.ErrPopulationExceeded
	}
	candidateIDs, err := s.readOpenCandidates(ctx, projects, query.At.UTC())
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT i.project_id,i.native_id,i.title,state.name,coalesce(ws.terminal,false),
	previous.recorded_at,
	CASE WHEN (SELECT true FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.created' AND e.recorded_at <= $3 LIMIT 1) THEN i.created_at END,
	(SELECT e.recorded_at FROM collaboration_events e JOIN project_workflow_states started ON started.organization_id=e.organization_id AND started.project_id=e.project_id AND started.name=e.to_state WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.recorded_at <= $3 AND started.dispatchable AND NOT started.terminal AND e.to_state NOT IN ('Backlog','Triage','Blocked','Human Review') ORDER BY e.recorded_at LIMIT 1)
FROM unnest($11::bigint[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=$1 AND i.id=candidate.id LIMIT 1) i
LEFT JOIN LATERAL (SELECT e.to_state AS name,e.recorded_at FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.recorded_at <= $3 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1) previous ON true
LEFT JOIN LATERAL (SELECT e.from_state AS name FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.recorded_at > $3 ORDER BY e.recorded_at,e.sequence LIMIT 1) next ON previous.name IS NULL
LEFT JOIN LATERAL (SELECT coalesce(previous.name,next.name,CASE WHEN i.native_updated_at <= $3 THEN i.workflow_state_name END,'') AS name) state ON true
LEFT JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=state.name
WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND NOT i.archived AND i.created_at < $3 AND (state.name='' OR NOT coalesce(ws.terminal,false)) AND ($4='' OR i.actor_principal_id=$4)
  AND (coalesce(cardinality($5::text[]),0)=0 OR i.issue_type=ANY($5::text[]))
  AND (coalesce(cardinality($6::integer[]),0)=0 OR i.priority=ANY($6::integer[]))
  AND (coalesce(cardinality($7::text[]),0)=0 OR i.actor_principal_id=ANY($7::text[]))
  AND (coalesce(cardinality($8::text[]),0)=0 OR EXISTS (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($8::text[])))
  AND (coalesce(cardinality($9::text[]),0)=0 OR EXISTS (SELECT 1 FROM native_attempts a JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at <= $3 AND la.runner_id=ANY($9::text[])))
ORDER BY i.project_id,i.native_id LIMIT $10`, s.organization, projects, query.At.UTC(), query.Principal, query.Filter.Types, query.Filter.Priorities, query.Filter.FiledBy, query.Filter.Labels, query.Filter.Runners, query.Limit+1, candidateIDs)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	facts := make([]reportstore.OpenFact, 0, min(query.Limit, 128))
	for rows.Next() {
		var fact reportstore.OpenFact
		var project, workItem string
		var entered, created, firstWorked pgtype.Timestamptz
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
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(facts) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return facts, nil
}

func (s *session) readOpenCandidates(ctx context.Context, projects []string, at time.Time) ([]int64, error) {
	var exceeded bool
	var ids []int64
	err := s.db.QueryRow(ctx, `WITH RECURSIVE states AS (
 SELECT p.project_id,first.name,1 AS depth FROM unnest($2::text[]) p(project_id) CROSS JOIN LATERAL (
  SELECT i.workflow_state_name AS name FROM issues i WHERE i.organization_id=$1 AND i.project_id=p.project_id AND NOT i.archived AND i.workflow_state_name IS NOT NULL ORDER BY i.workflow_state_name LIMIT 1
 ) first
 UNION ALL
 SELECT s.project_id,next.name,s.depth+1 FROM states s CROSS JOIN LATERAL (
  SELECT i.workflow_state_name AS name FROM issues i WHERE i.organization_id=$1 AND i.project_id=s.project_id AND NOT i.archived AND i.workflow_state_name>s.name ORDER BY i.workflow_state_name LIMIT 1
 ) next WHERE s.depth<=$5
), open AS MATERIALIZED (
 SELECT i.id FROM states s LEFT JOIN project_workflow_states ws ON ws.organization_id=$1 AND ws.project_id=s.project_id AND ws.name=s.name
 CROSS JOIN LATERAL (
  SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=s.project_id AND NOT i.archived AND i.workflow_state_name=s.name ORDER BY i.id LIMIT $4
 ) i WHERE NOT coalesce(ws.terminal,false)
 UNION ALL
 SELECT i.id FROM unnest($2::text[]) p(project_id) CROSS JOIN LATERAL (
  SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=p.project_id AND NOT i.archived AND i.workflow_state_name IS NULL ORDER BY i.id LIMIT $4
 ) i
), terminal AS MATERIALIZED (
 SELECT i.id,i.project_id FROM issues i WHERE i.organization_id=$1 AND NOT i.archived AND i.terminal_entered_at IS NOT NULL AND i.terminal_entered_at>$3 ORDER BY i.terminal_entered_at,i.id LIMIT $4
), updated AS MATERIALIZED (
 SELECT i.id,i.project_id,i.archived FROM issues i WHERE i.organization_id=$1 AND i.native_updated_at>$3 ORDER BY i.native_updated_at DESC,i.id DESC LIMIT $4
), keyed AS (
 SELECT id FROM open
 UNION SELECT id FROM terminal WHERE project_id=ANY($2::text[])
 UNION SELECT id FROM updated WHERE project_id=ANY($2::text[]) AND NOT archived
)
SELECT EXISTS(SELECT 1 FROM states WHERE depth>$5) OR (SELECT count(*) FROM open)>=$4 OR (SELECT count(*) FROM terminal)>=$4 OR (SELECT count(*) FROM updated)>=$4,
 coalesce((SELECT array_agg(id ORDER BY id) FROM keyed),'{}'::bigint[])`, s.organization, projects, at, maxOverviewCandidateRows+1, maxReportStateNames).Scan(&exceeded, &ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if exceeded || len(ids) > maxOverviewCandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	return ids, nil
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
	if len(projects) > maxReportProjects || len(query.ActivityWorkItems) > maxOverviewCandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	activityIDs := make([]string, len(query.ActivityWorkItems))
	for i, id := range query.ActivityWorkItems {
		activityIDs[i] = string(id)
	}
	candidateIDs, err := s.readOverviewCandidates(ctx, projects, query.From.UTC(), query.To.UTC(), activityIDs)
	if err != nil {
		return nil, err
	}
	statement := `WITH candidates AS (
	 SELECT i.organization_id,i.id,i.project_id,i.native_id,i.title,i.created_at,i.workflow_state_name,i.actor_kind,i.actor_principal_id,i.issue_type,i.priority,i.revision,i.doctor_identity_revision
 FROM unnest($15::bigint[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=$1 AND i.id=candidate.id LIMIT 1) i
 WHERE i.project_id=ANY($2::text[])
   AND ($3='' OR i.actor_principal_id=$3)
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
 AND (coalesce(cardinality($13::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id AND e.recorded_at >= $4 AND e.recorded_at < $5 AND coalesce(nullif(trim(f.model),''),'Unavailable')=ANY($13::text[])))
 AND (coalesce(cardinality($14::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id AND e.recorded_at >= $4 AND e.recorded_at < $5 AND coalesce(nullif(trim(f.role),''),'Unavailable')=ANY($14::text[])))
 ORDER BY i.created_at,i.project_id,i.native_id LIMIT $12
)
SELECT i.project_id,i.native_id,i.title,i.created_at,coalesce(i.workflow_state_name,''),
 coalesce(i.actor_kind IN ('machine','automation','system','integration'),false) OR coalesce(i.doctor_identity_revision=i.revision,false) AND (coalesce(filing.actor_envelope_known,false) OR coalesce(filer_page.revision=i.revision,false) OR EXISTS(SELECT 1 FROM doctor_proposal_identities origin WHERE origin.organization_id=i.organization_id AND origin.project_id=i.project_id AND origin.work_item_id=i.native_id AND origin.kind='fingerprint' AND origin.source_revision=i.revision)),
 coalesce(i.actor_kind IN ('machine','automation','system','integration'),false) OR coalesce(i.doctor_identity_revision=i.revision,false),coalesce(filing.id,''),
 CASE WHEN i.actor_kind IN ('machine','automation','system','integration') OR EXISTS(SELECT 1 FROM doctor_proposal_identities origin WHERE origin.organization_id=i.organization_id AND origin.project_id=i.project_id AND origin.work_item_id=i.native_id AND origin.kind='fingerprint' AND origin.source_revision=i.revision AND i.doctor_identity_revision=i.revision) THEN 'automation'
 WHEN i.actor_kind IN ('agent','runner') OR coalesce(CASE WHEN filing.actor_envelope_known THEN filing.actor_agent ELSE filer_page.actor_agent END,'')<>'' THEN 'agents'
 WHEN i.actor_kind='human' THEN 'people' ELSE 'unknown' END,coalesce(i.actor_principal_id,''),
 completion.recorded_at,coalesce(completion.to_state,''),coalesce(completion.actor_envelope_known,false),coalesce(completion.id,''),
 CASE WHEN completion.actor_envelope_known AND completion.actor_kind='human' AND completion.actor_agent='' THEN 'people' WHEN final_runner.runner_id IS NOT NULL THEN 'runner' ELSE 'unknown' END,
 CASE WHEN completion.actor_envelope_known AND completion.actor_kind='human' AND completion.actor_agent='' THEN completion.actor_principal_id ELSE coalesce(final_runner.runner_id,'') END,
 CASE WHEN completion.actor_envelope_known AND completion.actor_kind='human' AND completion.actor_agent='' THEN completion.actor_display_name ELSE coalesce(final_runner.display_name,'') END,
 previous_completion.recorded_at,coalesce(previous_completion.to_state,''),coalesce(previous_completion.actor_envelope_known,false),coalesce(previous_completion.id,''),
 CASE WHEN previous_completion.actor_envelope_known AND previous_completion.actor_kind='human' AND previous_completion.actor_agent='' THEN 'people' WHEN previous_final_runner.runner_id IS NOT NULL THEN 'runner' ELSE 'unknown' END,
 CASE WHEN previous_completion.actor_envelope_known AND previous_completion.actor_kind='human' AND previous_completion.actor_agent='' THEN previous_completion.actor_principal_id ELSE coalesce(previous_final_runner.runner_id,'') END,
 CASE WHEN previous_completion.actor_envelope_known AND previous_completion.actor_kind='human' AND previous_completion.actor_agent='' THEN previous_completion.actor_display_name ELSE coalesce(previous_final_runner.display_name,'') END,
 EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=(SELECT id FROM issues WHERE organization_id=i.organization_id AND project_id=i.project_id AND native_id=i.native_id) AND a.started_at >= $6 AND a.started_at < $5),
 EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=(SELECT id FROM issues WHERE organization_id=i.organization_id AND project_id=i.project_id AND native_id=i.native_id) AND a.started_at >= $4 AND a.started_at < $6),
 EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.recorded_at >= $6 AND e.recorded_at < $5),
 EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.recorded_at >= $4 AND e.recorded_at < $6),
 EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=(SELECT id FROM issues WHERE organization_id=i.organization_id AND project_id=i.project_id AND native_id=i.native_id) AND a.started_at >= $4 AND a.started_at < $5)
 ,EXISTS(SELECT 1 FROM change_requests c JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.landed_at >= $6 AND c.landed_at < $5 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
 ,EXISTS(SELECT 1 FROM change_requests c JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.landed_at >= $4 AND c.landed_at < $6 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
FROM candidates i
LEFT JOIN LATERAL (
 SELECT e.id,e.actor_agent,e.actor_envelope_known FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.created'
 ORDER BY e.sequence LIMIT 1
) filing ON true
LEFT JOIN native_work_item_pages filer_page ON filer_page.organization_id=i.organization_id AND filer_page.project_id=i.project_id AND filer_page.work_item_id=i.native_id AND filer_page.revision=i.revision
LEFT JOIN LATERAL (
 SELECT e.id,e.recorded_at,e.to_state,e.actor_kind,e.actor_principal_id,e.actor_agent,e.actor_display_name,e.actor_envelope_known,ws.terminal
 FROM collaboration_events e LEFT JOIN project_workflow_states ws
   ON ws.organization_id=e.organization_id AND ws.project_id=e.project_id AND ws.name=e.to_state
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
   AND e.type='workflow.transitioned' AND e.recorded_at < $5
 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1
) completion ON completion.terminal
LEFT JOIN LATERAL (
 SELECT r.runner_id,r.display_name
 FROM native_attempts a
 LEFT JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
 LEFT JOIN runner_lease_assignments assignment ON assignment.organization_id=l.organization_id AND assignment.lease_id=l.lease_id
 LEFT JOIN runner_identities r ON r.organization_id=assignment.organization_id AND r.runner_id=assignment.runner_id AND r.machine_id=l.machine_id
 WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=completion.recorded_at
 ORDER BY a.started_at DESC,a.started_at_submicro_ns DESC,a.id DESC LIMIT 1
) final_runner ON completion.to_state='Done' 
LEFT JOIN LATERAL (
 SELECT e.id,e.recorded_at,e.to_state,e.actor_kind,e.actor_principal_id,e.actor_agent,e.actor_display_name,e.actor_envelope_known,ws.terminal
 FROM collaboration_events e LEFT JOIN project_workflow_states ws
   ON ws.organization_id=e.organization_id AND ws.project_id=e.project_id AND ws.name=e.to_state
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
   AND e.type='workflow.transitioned' AND e.recorded_at < $6
 ORDER BY e.recorded_at DESC,e.sequence DESC LIMIT 1
) previous_completion ON previous_completion.terminal
LEFT JOIN LATERAL (
 SELECT r.runner_id,r.display_name
 FROM native_attempts a
 LEFT JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
 LEFT JOIN runner_lease_assignments assignment ON assignment.organization_id=l.organization_id AND assignment.lease_id=l.lease_id
 LEFT JOIN runner_identities r ON r.organization_id=assignment.organization_id AND r.runner_id=assignment.runner_id AND r.machine_id=l.machine_id
 WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=previous_completion.recorded_at
 ORDER BY a.started_at DESC,a.started_at_submicro_ns DESC,a.id DESC LIMIT 1
) previous_final_runner ON previous_completion.to_state='Done' 
ORDER BY i.created_at,i.project_id,i.native_id`
	rows, err := s.db.Query(ctx, statement, s.organization, projects, query.Principal, query.From.UTC(), query.To.UTC(), query.Split.UTC(), query.Filter.Types, query.Filter.Priorities, query.Filter.FiledBy, query.Filter.Labels, query.Filter.Runners, query.Limit+1, query.Filter.Models, query.Filter.Stages, candidateIDs)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	facts := make([]reportstore.OverviewFact, 0, min(query.Limit, 128))
	for rows.Next() {
		var fact reportstore.OverviewFact
		var closedAt, previousClosedAt pgtype.Timestamptz
		var project, workItem string
		if err := rows.Scan(&project, &workItem, &fact.Title, &fact.CreatedAt, &fact.State, &fact.FilerKnown, &fact.FilerOriginKnown, &fact.FilerEventID, &fact.FilerKind, &fact.FilerID, &closedAt, &fact.CompletedState, &fact.CompletedActorKnown, &fact.CompletedEventID, &fact.CompletedKind, &fact.CompletedID, &fact.CompletedName, &previousClosedAt, &fact.PreviousCompletedState, &fact.PreviousCompletedActorKnown, &fact.PreviousCompletedEventID, &fact.PreviousCompletedKind, &fact.PreviousCompletedID, &fact.PreviousCompletedName, &fact.Worked, &fact.PreviousWorked, &fact.Reworked, &fact.PreviousReworked, &fact.ActivityObserved, &fact.ChangesLanded, &fact.PreviousChangesLanded); err != nil {
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
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(facts) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return facts, nil
}

func (s *session) readOverviewCandidates(ctx context.Context, projects []string, from, to time.Time, named []string) ([]int64, error) {
	var exceeded bool
	var ids []int64
	err := s.db.QueryRow(ctx, `WITH created AS MATERIALIZED (
 SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND i.created_at>=$3 AND i.created_at<$4 ORDER BY i.project_id,i.created_at,i.id LIMIT $6
), named AS MATERIALIZED (
 SELECT i.id FROM unnest($5::text[]) n(native_id) CROSS JOIN LATERAL (
  SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.native_id=n.native_id AND i.project_id=ANY($2::text[])
 ) i LIMIT $6
), transitioned AS MATERIALIZED (
 SELECT e.project_id,e.work_item_id FROM (
  SELECT ws.project_id,ws.name FROM project_workflow_states ws WHERE ws.organization_id=$1 AND ws.project_id=ANY($2::text[]) AND ws.terminal
  UNION SELECT p.project_id,'Rework' FROM unnest($2::text[]) p(project_id)
 ) state CROSS JOIN LATERAL (
  SELECT e.project_id,e.work_item_id FROM collaboration_events e
  WHERE e.organization_id=$1 AND e.project_id=state.project_id AND e.type='workflow.transitioned' AND e.to_state=state.name AND e.recorded_at>=$3 AND e.recorded_at<$4 LIMIT $6
 ) e LIMIT $6
), started AS MATERIALIZED (
 SELECT a.id FROM native_attempts a WHERE a.organization_id=$1 AND a.project_id=ANY($2::text[]) AND a.started_at>=$3 AND a.started_at<$4 ORDER BY a.project_id,a.started_at,a.started_at_submicro_ns,a.id LIMIT $6
), landed AS MATERIALIZED (
 SELECT c.project_id,c.work_item_id FROM change_requests c JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id
 WHERE c.organization_id=$1 AND c.project_id=ANY($2::text[]) AND c.landed_at>=$3 AND c.landed_at<$4 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$' ORDER BY c.project_id,c.landed_at,c.id LIMIT $6
), keyed AS (
 SELECT id FROM created UNION SELECT id FROM named
 UNION SELECT a.issue_id FROM started s CROSS JOIN LATERAL (SELECT a.issue_id FROM native_attempts a WHERE a.organization_id=$1 AND a.id=s.id LIMIT 1) a
 UNION SELECT i.id FROM transitioned t CROSS JOIN LATERAL (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=t.project_id AND i.native_id=t.work_item_id LIMIT 1) i
 UNION SELECT i.id FROM landed l CROSS JOIN LATERAL (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=l.project_id AND i.native_id=l.work_item_id LIMIT 1) i
)
SELECT (SELECT count(*) FROM created)>=$6 OR (SELECT count(*) FROM named)>=$6 OR (SELECT count(*) FROM transitioned)>=$6 OR (SELECT count(*) FROM started)>=$6 OR (SELECT count(*) FROM landed)>=$6,
 coalesce((SELECT array_agg(id ORDER BY id) FROM keyed),'{}'::bigint[])`, s.organization, projects, from, to, named, maxOverviewCandidateRows+1).Scan(&exceeded, &ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if exceeded || len(ids) > maxOverviewCandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	return ids, nil
}

func (s *session) ReadFacets(ctx context.Context, projects []tracker.ProjectID, from, to time.Time, principal string, limit int) (reportstore.Facets, error) {
	if len(projects) == 0 || len(projects) > 32 || limit < 1 || limit > 5000 || !from.Before(to) {
		return reportstore.Facets{}, errors.New("invalid PostgreSQL report facets query")
	}
	out := reportstore.Facets{Labels: []string{}, FiledBy: []reportstore.Facet{}, Runners: []reportstore.Facet{}, Models: []string{}, Stages: []string{}, Unavailable: []string{}}
	ids := []int64{}
	filers := map[string]string{}
	projectIDs := make([]string, len(projects))
	for i, project := range projects {
		projectIDs[i] = string(project)
	}
	windowIDs, err := s.readOverviewCandidates(ctx, projectIDs, from.UTC(), to.UTC(), nil)
	if err != nil {
		return out, err
	}
	openIDs, err := s.readOpenCandidates(ctx, projectIDs, to.UTC())
	if err != nil {
		return out, err
	}
	rows, err := s.db.Query(ctx, `SELECT i.id,coalesce(i.actor_principal_id,''),coalesce(nullif(t.name,''),i.actor_principal_id,'') FROM (SELECT DISTINCT id FROM unnest($4::bigint[] || $5::bigint[]) candidate(id)) candidate CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=$1 AND i.id=candidate.id LIMIT 1) i LEFT JOIN api_tokens t ON t.organization_id=i.organization_id AND t.id=i.actor_principal_id WHERE i.project_id=ANY($2::text[]) AND ($3='' OR i.actor_principal_id=$3) ORDER BY i.id LIMIT 5001`, s.organization, projectIDs, principal, windowIDs, openIDs)
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
	if err := rows.Err(); err != nil {
		return out, mapError(ctx, err)
	}
	if len(ids) > 5000 {
		return out, reportstore.ErrPopulationExceeded
	}
	labels := map[string]bool{}
	rows, err = s.db.Query(ctx, `SELECT l.label FROM issues i JOIN issue_labels l ON l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id WHERE i.organization_id=$1 AND i.id=ANY($2::bigint[]) ORDER BY i.id,l.label LIMIT 100001`, s.organization, ids)
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
	if err := rows.Err(); err != nil {
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
	now, err := s.leaseTime(ctx)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	candidateIDs, err := s.readAttemptCandidates(ctx, query.From.UTC(), query.To.UTC())
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT i.project_id,i.native_id,a.id,coalesce(r.runner_id,''),coalesce(r.display_name,''),CASE WHEN lost.ended_at<$5 THEN 'interrupted' ELSE coalesce(nullif(f.run_status,''),'running') END,a.started_at,CASE WHEN lost.ended_at<$5 THEN greatest(lost.ended_at,coalesce(f.recorded_at,a.started_at)) ELSE coalesce(f.recorded_at,a.started_at) END,coalesce(f.role,''),coalesce(f.model,''),coalesce(f.effort,''),f.event_id IS NOT NULL,coalesce(f.failed,false),coalesce(f.failure_class,''),coalesce(f.failure_kind,''),coalesce(f.landing_receipt,false),coalesce(f.landed,false),coalesce(f.landing_change_id,''),coalesce(f.duration_seconds,0),coalesce(f.event_id,'')
FROM unnest($15::text[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM native_attempts a WHERE a.organization_id=$1 AND a.id=candidate.id LIMIT 1) a
CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=a.organization_id AND i.id=a.issue_id LIMIT 1) i
LEFT JOIN LATERAL (SELECT * FROM report_attempt_facts af WHERE af.organization_id=a.organization_id AND af.attempt_id=a.id LIMIT 1) af ON true
LEFT JOIN LATERAL (SELECT e.id,e.recorded_at FROM collaboration_events e WHERE e.organization_id=af.organization_id AND e.id=af.outcome_event_id LIMIT 1) outcome ON true
LEFT JOIN LATERAL (SELECT e.id,e.recorded_at FROM collaboration_events e WHERE e.organization_id=af.organization_id AND e.id=af.latest_event_id LIMIT 1) latest ON true
LEFT JOIN LATERAL (SELECT e.id,e.recorded_at FROM collaboration_events e WHERE e.organization_id=af.organization_id AND e.id=af.first_event_id LIMIT 1) first ON true
LEFT JOIN LATERAL(SELECT CASE WHEN outcome.recorded_at<$5 THEN outcome.id WHEN latest.recorded_at<$5 THEN latest.id WHEN first.recorded_at<$5 THEN first.id END AS id) chosen ON true
LEFT JOIN LATERAL(SELECT rf.*,e.recorded_at FROM collaboration_events e JOIN report_event_facts rf ON rf.organization_id=e.organization_id AND rf.event_id=e.id WHERE e.organization_id=a.organization_id AND e.id=chosen.id LIMIT 1) f ON true
LEFT JOIN LATERAL (SELECT * FROM leases l WHERE l.organization_id=a.organization_id AND l.lease_id=a.lease_id LIMIT 1) l ON true
LEFT JOIN LATERAL(SELECT CASE WHEN coalesce(nullif(f.run_status,''),'running')='running' AND (l.released_at IS NOT NULL OR l.expires_at<=$14) THEN least(coalesce(l.released_at,l.expires_at),l.expires_at) END AS ended_at) lost ON true
LEFT JOIN LATERAL (SELECT * FROM runner_lease_assignments la WHERE la.organization_id=a.organization_id AND la.lease_id=a.lease_id LIMIT 1) la ON true
LEFT JOIN LATERAL (SELECT * FROM runner_identities r WHERE r.organization_id=la.organization_id AND r.runner_id=la.runner_id LIMIT 1) r ON true
WHERE a.organization_id=$1 AND i.project_id=ANY($2::text[]) AND ($3='' OR i.actor_principal_id=$3)
  AND a.started_at < $5 AND (f.recorded_at >= $4 OR coalesce(f.run_status,'running')='running' AND (lost.ended_at IS NULL OR lost.ended_at>=$4) OR (SELECT true FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.attempt_id=a.id AND u.period>=$4 AND u.period<$5 LIMIT 1) OR (SELECT true FROM report_usage_current u WHERE u.organization_id=a.organization_id AND u.attempt_id=a.id AND u.period>=$4 AND u.period<$5 LIMIT 1))
  AND (coalesce(cardinality($6::text[]),0)=0 OR i.issue_type=ANY($6::text[]))
  AND (coalesce(cardinality($7::integer[]),0)=0 OR i.priority=ANY($7::integer[]))
  AND (coalesce(cardinality($8::text[]),0)=0 OR i.actor_principal_id=ANY($8::text[]))
  AND (coalesce(cardinality($9::text[]),0)=0 OR EXISTS (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($9::text[])))
  AND (coalesce(cardinality($10::text[]),0)=0 OR r.runner_id=ANY($10::text[]))
AND (coalesce(cardinality($12::text[]),0)=0 OR coalesce(nullif(trim(f.model),''),'Unavailable')=ANY($12::text[]))
AND (coalesce(cardinality($13::text[]),0)=0 OR coalesce(nullif(trim(f.role),''),'Unavailable')=ANY($13::text[]))
ORDER BY a.updated_at,a.id LIMIT $11`, s.organization, projects, query.Principal, query.From.UTC(), query.To.UTC(), query.Filter.Types, query.Filter.Priorities, query.Filter.FiledBy, query.Filter.Labels, query.Filter.Runners, query.Limit+1, query.Filter.Models, query.Filter.Stages, now, candidateIDs)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	attempts := make([]reportstore.AttemptFact, 0, min(query.Limit, 128))
	for rows.Next() {
		var fact reportstore.AttemptFact
		var project, workItem string
		if err := rows.Scan(&project, &workItem, &fact.AttemptID, &fact.RunnerID, &fact.RunnerName, &fact.Status, &fact.StartedAt, &fact.OutcomeAt, &fact.Role, &fact.Model, &fact.Effort, &fact.ProjectionKnown, &fact.Failed, &fact.FailureClass, &fact.FailureKind, &fact.LandingReceipt, &fact.Landed, &fact.LandingChangeID, &fact.DurationSeconds, &fact.EventID); err != nil {
			return nil, mapError(ctx, err)
		}
		fact.UpdatedAt = fact.OutcomeAt
		fact.ProjectID, fact.WorkItemID = tracker.ProjectID(project), tracker.NativeWorkItemID(workItem)
		attempts = append(attempts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(attempts) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return attempts, nil
}

func (s *session) readAttemptCandidates(ctx context.Context, from, to time.Time) ([]string, error) {
	var exceeded bool
	var ids []string
	err := s.db.QueryRow(ctx, `WITH RECURSIVE machines AS (
 SELECT first.machine_id,1 AS depth FROM (SELECT l.machine_id FROM leases l WHERE l.organization_id=$1 ORDER BY l.machine_id LIMIT 1) first
 UNION ALL
 SELECT next.machine_id,m.depth+1 FROM machines m CROSS JOIN LATERAL (
  SELECT l.machine_id FROM leases l WHERE l.organization_id=$1 AND l.machine_id>m.machine_id ORDER BY l.machine_id LIMIT 1
 ) next WHERE m.depth<=$5
), usage_projects AS (
 SELECT first.project_id,1 AS depth FROM (SELECT u.project_id FROM attempt_usage u WHERE u.organization_id=$1 ORDER BY u.project_id LIMIT 1) first
 UNION ALL
 SELECT next.project_id,p.depth+1 FROM usage_projects p CROSS JOIN LATERAL (
  SELECT u.project_id FROM attempt_usage u WHERE u.organization_id=$1 AND u.project_id>p.project_id ORDER BY u.project_id LIMIT 1
 ) next WHERE p.depth<=$5
), current_projects AS (
 SELECT first.project_id,1 AS depth FROM (SELECT u.project_id FROM report_usage_current u WHERE u.organization_id=$1 ORDER BY u.project_id LIMIT 1) first
 UNION ALL
 SELECT next.project_id,p.depth+1 FROM current_projects p CROSS JOIN LATERAL (
  SELECT u.project_id FROM report_usage_current u WHERE u.organization_id=$1 AND u.project_id>p.project_id ORDER BY u.project_id LIMIT 1
 ) next WHERE p.depth<=$5
), leased AS MATERIALIZED (
 SELECT a.id FROM machines m CROSS JOIN LATERAL (
  SELECT l.lease_id FROM leases l WHERE l.organization_id=$1 AND l.machine_id=m.machine_id AND l.expires_at>=$2 ORDER BY l.expires_at LIMIT $4
 ) l CROSS JOIN LATERAL (
  SELECT a.id FROM native_attempts a WHERE a.organization_id=$1 AND a.lease_id=l.lease_id LIMIT 1
 ) a WHERE m.depth<=$5
), evented AS MATERIALIZED (
 SELECT e.attempt_id FROM collaboration_events e WHERE e.organization_id=$1 AND e.recorded_at>=$2 AND e.recorded_at<$3 AND nullif(e.attempt_id,'') IS NOT NULL ORDER BY e.recorded_at DESC,e.id DESC LIMIT $4
), used AS MATERIALIZED (
 SELECT u.attempt_id FROM usage_projects p CROSS JOIN LATERAL (
  SELECT u.attempt_id FROM attempt_usage u WHERE u.organization_id=$1 AND u.project_id=p.project_id AND u.period>=$2 AND u.period<$3 ORDER BY u.period,u.attempt_id LIMIT $4
 ) u WHERE p.depth<=$5
 UNION ALL
 SELECT u.attempt_id FROM current_projects p CROSS JOIN LATERAL (
  SELECT u.attempt_id FROM report_usage_current u WHERE u.organization_id=$1 AND u.project_id=p.project_id AND u.period>=$2 AND u.period<$3 ORDER BY u.period LIMIT $4
 ) u WHERE p.depth<=$5
), keyed AS (
 SELECT id FROM leased UNION SELECT attempt_id FROM evented UNION SELECT attempt_id FROM used
)
SELECT EXISTS(SELECT 1 FROM machines WHERE depth>$5) OR EXISTS(SELECT 1 FROM usage_projects WHERE depth>$5) OR EXISTS(SELECT 1 FROM current_projects WHERE depth>$5)
 OR (SELECT count(*) FROM leased)>=$4 OR (SELECT count(*) FROM evented)>=$4 OR (SELECT count(*) FROM used)>=$4,
 coalesce((SELECT array_agg(id ORDER BY id) FROM keyed),'{}'::text[])`, s.organization, from, to, maxOverviewCandidateRows+1, maxReportProjects).Scan(&exceeded, &ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if exceeded || len(ids) > maxOverviewCandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	return ids, nil
}

func (s *session) HasPriorData(ctx context.Context, projects []tracker.ProjectID, before time.Time, principal string) (bool, error) {
	projectIDs, err := priorProjectIDs(projects)
	if err != nil {
		return false, err
	}
	var recorded bool
	if principal != "" {
		err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM unnest($2::text[]) p(id) CROSS JOIN LATERAL (
 SELECT 1 FROM issues i WHERE i.organization_id=$1 AND i.project_id=p.id AND i.actor_principal_id=$3 AND i.created_at<$4 ORDER BY i.created_at LIMIT 1
) i)`, s.organization, projectIDs, principal, before.UTC()).Scan(&recorded)
		return recorded, mapError(ctx, err)
	}
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE organization_id=$1 AND id=ANY($2::text[]) AND deleted_at IS NULL AND created_at<$3) OR EXISTS(SELECT 1 FROM unnest($2::text[]) p(id) CROSS JOIN LATERAL (
 SELECT 1 FROM issues i WHERE i.organization_id=$1 AND i.project_id=p.id AND i.created_at<$3 ORDER BY i.created_at LIMIT 1
) i)`, s.organization, projectIDs, before.UTC()).Scan(&recorded)
	return recorded, mapError(ctx, err)
}

func (s *session) HasPriorProjects(ctx context.Context, projects []tracker.ProjectID, before time.Time) (bool, error) {
	projectIDs, err := priorProjectIDs(projects)
	if err != nil {
		return false, err
	}
	var recorded bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE organization_id=$1 AND id=ANY($2::text[]) AND deleted_at IS NULL AND created_at<$3)`, s.organization, projectIDs, before.UTC()).Scan(&recorded)
	return recorded, mapError(ctx, err)
}

func priorProjectIDs(projects []tracker.ProjectID) ([]string, error) {
	if len(projects) == 0 || len(projects) > maxReportProjects {
		return nil, errors.New("invalid PostgreSQL report projects")
	}
	projectIDs := make([]string, len(projects))
	for i, project := range projects {
		projectIDs[i] = string(project)
	}
	return projectIDs, nil
}

func (s *session) ReadAnalyticsAttemptPage(ctx context.Context, query reportstore.AnalyticsAttemptQuery) (reportstore.AnalyticsAttemptPage, error) {
	out := reportstore.AnalyticsAttemptPage{Items: []reportstore.AnalyticsAttemptRow{}, Partial: query.After.ID != ""}
	if query.ProjectID == "" || len(query.RunnerIDs) > 32 || !query.From.Before(query.To) || query.Limit < 1 || query.Limit > reportstore.AnalyticsAttemptLimit || query.After.Submicro < 0 || query.After.Submicro > 999 || (query.After.ID == "") != query.After.StartedAt.IsZero() || query.After.StartedAt.Nanosecond()%1000 != 0 || query.After.ID == "" && query.After.Submicro != 0 {
		return out, corestore.ErrInvalidPage
	}
	args := []any{s.organization, query.ProjectID, query.To.UTC(), query.Limit + 1, query.From.UTC(), query.Principal, query.RunnerID}
	seek := ""
	direction, comparison := " DESC", "<"
	if query.OldestFirst {
		direction, comparison = "", ">"
	}
	if query.StartedInWindow {
		seek = " AND a.started_at>=$5"
	}
	if query.After.ID != "" {
		seek += " AND (a.started_at,a.started_at_submicro_ns,a.id)" + comparison + "($8,$9,$10)"
		args = append(args, query.After.StartedAt.UTC(), query.After.Submicro, query.After.ID)
	}
	runnerArg := len(args) + 1
	args = append(args, query.RunnerIDs)
	roleSelect, roleJoin := "", ""
	if query.StartedInWindow {
		roleSelect = ",latest_fact.role"
		roleJoin = `LEFT JOIN LATERAL(SELECT f.latest_event_id FROM report_attempt_facts f WHERE f.organization_id=$1 AND f.attempt_id=a.id LIMIT 1)latest_attempt ON true
 LEFT JOIN LATERAL(SELECT f.role FROM report_event_facts f WHERE f.organization_id=$1 AND f.event_id=latest_attempt.latest_event_id LIMIT 1)latest_fact ON true`
	}
	rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED (
 SELECT a.id,a.project_id,a.issue_id,a.lease_id,a.status,a.started_at,a.started_at_submicro_ns,a.updated_at
 FROM native_attempts a WHERE a.organization_id=$1 AND a.project_id=$2 AND a.started_at<$3`+seek+`
 ORDER BY a.started_at`+direction+`,a.started_at_submicro_ns`+direction+`,a.id`+direction+` LIMIT $4
) SELECT a.id,a.project_id,coalesce(i.native_id,''),a.status,a.started_at,a.started_at_submicro_ns,a.updated_at,
 i.id IS NOT NULL AND a.updated_at>=$5 AND ($6='' OR coalesce(i.actor_principal_id,'')=$6) AND ($7='' OR coalesce(la.runner_id,'')=$7) AND (coalesce(cardinality($`+strconv.Itoa(runnerArg)+`::text[]),0)=0 OR coalesce(la.runner_id,'')=ANY($`+strconv.Itoa(runnerArg)+`::text[])) AS eligible`+roleSelect+`
FROM examined a LEFT JOIN issues i ON i.organization_id=$1 AND i.project_id=a.project_id AND i.id=a.issue_id
LEFT JOIN runner_lease_assignments la ON la.organization_id=$1 AND la.lease_id=a.lease_id
`+roleJoin+`
ORDER BY a.started_at`+direction+`,a.started_at_submicro_ns`+direction+`,a.id`+direction, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	var last reportstore.AnalyticsAttemptCursor
	for rows.Next() {
		var row reportstore.AnalyticsAttemptRow
		var key reportstore.AnalyticsAttemptCursor
		var eligible bool
		values := []any{&row.AttemptID, &row.ProjectID, &row.WorkItemID, &row.Status, &key.StartedAt, &key.Submicro, &row.UpdatedAt, &eligible}
		if query.StartedInWindow {
			values = append(values, &row.Role)
		}
		if err := rows.Scan(values...); err != nil {
			return out, mapError(ctx, err)
		}
		out.Examined++
		if out.Examined > query.Limit {
			out.Next = last
			out.Partial = true
			break
		}
		key.ID = row.AttemptID
		last = key
		row.StartedAt = key.StartedAt.Add(time.Duration(key.Submicro))
		if eligible {
			out.Items = append(out.Items, row)
		}
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) ReadCohortWorkItems(ctx context.Context, q reportstore.OverviewQuery) ([]tracker.NativeWorkItemID, error) {
	if len(q.Projects) != 1 || q.Limit > 5000 {
		return nil, corestore.ErrInvalidPage
	}
	args, err := s.reportQueryArgs(q)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS MATERIALIZED(
 SELECT i.* FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) ORDER BY i.project_id,i.native_id LIMIT $11
 ) SELECT i.native_id,(`+reportIssueFilter+` AND i.created_at<$5) AS eligible FROM candidates i ORDER BY i.native_id`, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []tracker.NativeWorkItemID{}
	examined := 0
	for rows.Next() {
		var id tracker.NativeWorkItemID
		var eligible bool
		if err := rows.Scan(&id, &eligible); err != nil {
			return nil, mapError(ctx, err)
		}
		examined++
		if examined > q.Limit {
			return nil, reportstore.ErrPopulationExceeded
		}
		if eligible {
			out = append(out, id)
		}
	}
	return out, mapError(ctx, rows.Err())
}
