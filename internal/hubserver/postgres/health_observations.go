package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthObservationLimit(limit int) error {
	if limit < 1 || limit > 10001 {
		return corestore.ErrInvalidPage
	}
	return nil
}

func (s *session) HealthUsage(ctx context.Context, project tracker.ProjectID, baseline, now time.Time, limit int) ([]healthstore.UsageObservation, error) {
	if err := healthObservationLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `WITH usage AS (
 SELECT project_id,attempt_id,period,cost_estimate FROM report_usage_current WHERE organization_id=$1 AND project_id=$2
 UNION ALL SELECT u.project_id,u.attempt_id,u.period,u.cost_estimate FROM attempt_usage u WHERE u.organization_id=$1 AND u.project_id=$2 AND upper(u.currency)='USD' AND NOT EXISTS(SELECT 1 FROM report_usage_current c WHERE c.organization_id=u.organization_id AND c.attempt_id=u.attempt_id)
 ) SELECT i.native_id,u.attempt_id,sum(u.cost_estimate),coalesce(sum(u.cost_estimate) FILTER(WHERE u.period<=$3),0),count(*) FILTER(WHERE u.period<=$3)>0 FROM usage u JOIN native_attempts a ON a.organization_id=$1 AND a.id=u.attempt_id JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=u.project_id WHERE u.period<=$4 GROUP BY i.native_id,u.attempt_id ORDER BY i.native_id,u.attempt_id LIMIT $5`, s.organization, project, baseline, now, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.UsageObservation
	for rows.Next() {
		var row healthstore.UsageObservation
		if err := rows.Scan(&row.Item, &row.AttemptID, &row.Cost, &row.BaselineCost, &row.BaselineKnown); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthRunners(ctx context.Context, after string, now time.Time, limit int) ([]healthstore.RunnerObservation, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT r.runner_id,r.machine_id,r.state,r.created_at,r.last_heartbeat_at,
 (SELECT count(*) FROM leases l JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id WHERE l.organization_id=r.organization_id AND l.machine_id=r.machine_id AND a.runner_id=r.runner_id AND l.released_at IS NULL),
 greatest(0,least(least(r.capacity_limit,r.reported_capacity)-(SELECT count(*) FROM leases l JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id WHERE l.organization_id=r.organization_id AND l.machine_id=r.machine_id AND a.runner_id=r.runner_id AND l.released_at IS NULL AND l.expires_at>$3),m.capacity-(SELECT count(*) FROM leases l WHERE l.organization_id=r.organization_id AND l.machine_id=r.machine_id AND l.released_at IS NULL AND l.expires_at>$3)))
 FROM runner_identities r JOIN machines m ON m.organization_id=r.organization_id AND m.id=r.machine_id JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
 WHERE r.organization_id=$1 AND r.runner_id>$2 AND r.removed_at IS NULL AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$3)
 ORDER BY r.runner_id LIMIT $4`, s.organization, after, now, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.RunnerObservation
	for rows.Next() {
		var row healthstore.RunnerObservation
		if err := rows.Scan(&row.ID, &row.MachineID, &row.State, &row.CreatedAt, &row.Heartbeat, &row.Leases, &row.FreeSlots); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthRunnerLeasedProjects(ctx context.Context, runner string, limit int) ([]tracker.ProjectID, error) {
	if err := healthObservationLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT i.project_id FROM runner_lease_assignments a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE a.organization_id=$1 AND a.runner_id=$2 AND l.released_at IS NULL ORDER BY i.project_id LIMIT $3`, s.organization, runner, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []tracker.ProjectID
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, project)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthDependencies(ctx context.Context, project tracker.ProjectID, limit int) ([]healthstore.DependencyObservation, error) {
	if err := healthObservationLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT d.native_id,b.native_id,b.project_id=d.project_id,bws.dispatchable,
 greatest(dep.created_at,coalesce((SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=d.organization_id AND e.project_id=d.project_id AND e.work_item_id=d.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence DESC LIMIT 1),d.created_at),coalesce((SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=b.organization_id AND e.project_id=b.project_id AND e.work_item_id=b.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence DESC LIMIT 1),b.created_at))
 FROM issue_dependencies dep JOIN issues d ON d.organization_id=dep.organization_id AND d.project_id=dep.dependent_project_id AND d.id=dep.dependent_issue_id
 JOIN projects p ON p.organization_id=d.organization_id AND p.id=d.project_id AND p.require_dependencies
 JOIN project_workflow_states dws ON dws.organization_id=d.organization_id AND dws.project_id=d.project_id AND dws.name=d.workflow_state_name
 JOIN issues b ON b.organization_id=dep.organization_id AND b.project_id=dep.blocker_project_id AND b.id=dep.blocker_issue_id
 JOIN project_workflow_states bws ON bws.organization_id=b.organization_id AND bws.project_id=b.project_id AND bws.name=b.workflow_state_name
 WHERE dep.organization_id=$1 AND dep.dependent_project_id=$2 AND NOT d.archived AND dws.dispatchable AND NOT dws.terminal AND NOT bws.terminal
 ORDER BY d.native_id,b.native_id LIMIT $3`, s.organization, project, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.DependencyObservation
	for rows.Next() {
		var row healthstore.DependencyObservation
		if err := rows.Scan(&row.Dependent, &row.Blocker, &row.SameProject, &row.Dispatchable, &row.Since); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthOpenItems(ctx context.Context, limit int) ([]healthstore.OpenItemObservation, error) {
	if err := healthObservationLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT i.project_id,i.native_id,i.revision,coalesce(a.id,''),coalesce(a.status,''),coalesce(a.updated_at,'0001-01-01'::timestamptz)
 FROM issues i JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name
 LEFT JOIN LATERAL (SELECT id,status,updated_at FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id ORDER BY a.fencing_token DESC LIMIT 1) a ON true
 WHERE i.organization_id=$1 AND NOT i.archived AND NOT ws.terminal ORDER BY i.id LIMIT $2`, s.organization, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.OpenItemObservation
	for rows.Next() {
		var row healthstore.OpenItemObservation
		if err := rows.Scan(&row.Project, &row.Item, &row.Revision, &row.AttemptID, &row.AttemptStatus, &row.AttemptUpdatedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthQuestions(ctx context.Context, now time.Time, limit int) ([]healthstore.QuestionObservation, error) {
	if err := healthObservationLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT c.project_id,coalesce(c.work_item_id,''),q.attempt_id,q.created_at FROM conversation_questions q JOIN conversations c ON c.organization_id=q.organization_id AND c.id=q.conversation_id WHERE c.organization_id=$1 AND c.status='active' AND q.status IN ('pending','sending','sent','unknown') AND (q.expires_at IS NULL OR q.expires_at>$2) ORDER BY q.created_at,q.id LIMIT $3`, s.organization, now, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.QuestionObservation
	for rows.Next() {
		var row healthstore.QuestionObservation
		if err := rows.Scan(&row.Project, &row.Item, &row.AttemptID, &row.Since); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthSchedulerEvents(ctx context.Context, from, timeTo time.Time, limit int) ([]eventstore.Event, error) {
	if err := healthObservationLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,event_revision,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version FROM collaboration_events WHERE organization_id=$1 AND type='scheduler.decision' AND recorded_at>=$2 AND recorded_at<=$3 ORDER BY recorded_at,id LIMIT $4`, s.organization, from, timeTo, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []eventstore.Event
	for rows.Next() {
		var row eventstore.Event
		if err := rows.Scan(&row.OrganizationID, &row.ProjectID, &row.WorkItemID, &row.ID, &row.Sequence, &row.Type, &row.SchemaVersion, &row.ActorKind, &row.ActorPrincipalID, &row.EventRevision, &row.RecordedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes, &row.PayloadFormatVersion); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
