package postgres

import (
	"context"
	"slices"
	"strings"
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

func (s *session) HealthArchivedWorkItems(ctx context.Context, project tracker.ProjectID, items []tracker.NativeWorkItemID) (map[tracker.NativeWorkItemID]bool, error) {
	if project == "" || len(items) > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	result := map[tracker.NativeWorkItemID]bool{}
	if len(items) == 0 {
		return result, nil
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = string(item)
	}
	rows, err := s.db.Query(ctx, `SELECT native_id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=ANY($3::text[]) AND archived`, s.organization, project, ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.NativeWorkItemID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		result[id] = true
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthUsage(ctx context.Context, project tracker.ProjectID, baseline, now time.Time, limit, sourceLimit int) (healthstore.UsagePage, error) {
	if err := healthObservationLimit(limit); err != nil {
		return healthstore.UsagePage{}, err
	}
	if sourceLimit < 0 || sourceLimit > 10000 {
		return healthstore.UsagePage{}, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `WITH current_input AS MATERIALIZED (
 SELECT attempt_id,period,cost_estimate FROM report_usage_current WHERE organization_id=$1 AND project_id=$2 AND period<=$4 ORDER BY period,source_key LIMIT $5
 ), legacy_input AS MATERIALIZED (
 SELECT attempt_id,period,cost_estimate FROM attempt_usage WHERE organization_id=$1 AND project_id=$2 AND upper(currency)='USD' AND period<=$4 ORDER BY period,attempt_id LIMIT $5
 ), overflow AS (
 SELECT (SELECT count(*) FROM current_input)+(SELECT count(*) FROM legacy_input) AS source_rows
 ), usage AS (
 SELECT attempt_id,period,cost_estimate FROM current_input
 UNION ALL SELECT u.attempt_id,u.period,u.cost_estimate FROM legacy_input u WHERE NOT EXISTS(SELECT 1 FROM report_usage_current c WHERE c.organization_id=$1 AND c.attempt_id=u.attempt_id)
 ) SELECT i.native_id,u.attempt_id,u.cost_estimate,u.period<=$3,false,0::bigint FROM usage u JOIN native_attempts a ON a.organization_id=$1 AND a.id=u.attempt_id JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=$2 CROSS JOIN overflow WHERE overflow.source_rows<=$6
 UNION ALL SELECT '','',0,false,true,source_rows FROM overflow`, s.organization, project, baseline, now, sourceLimit+1, sourceLimit)
	if err != nil {
		return healthstore.UsagePage{}, mapError(ctx, err)
	}
	defer rows.Close()
	type key struct{ item, attempt string }
	values := map[key]healthstore.UsageObservation{}
	page := healthstore.UsagePage{}
	for rows.Next() {
		var id key
		var cost float64
		var baselineKnown, metadata bool
		var sourceRows int
		if err := rows.Scan(&id.item, &id.attempt, &cost, &baselineKnown, &metadata, &sourceRows); err != nil {
			return healthstore.UsagePage{}, mapError(ctx, err)
		}
		if metadata {
			page.ReadRows, page.Complete = sourceRows, sourceRows <= sourceLimit
			continue
		}
		if id.item == "" || id.attempt == "" {
			return healthstore.UsagePage{}, corestore.ErrInvalidRelationship
		}
		value := values[id]
		value.Item, value.AttemptID = id.item, id.attempt
		value.Cost += cost
		if baselineKnown {
			value.BaselineKnown = true
			value.BaselineCost += cost
		}
		values[id] = value
	}
	if err := rows.Err(); err != nil {
		return healthstore.UsagePage{}, mapError(ctx, err)
	}
	result := make([]healthstore.UsageObservation, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	slices.SortFunc(result, func(a, b healthstore.UsageObservation) int {
		if order := strings.Compare(a.Item, b.Item); order != 0 {
			return order
		}
		return strings.Compare(a.AttemptID, b.AttemptID)
	})
	page.Values = result[:min(len(result), limit)]
	page.Complete = page.Complete && len(result) <= limit
	return page, nil
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

func (s *session) HealthRunnerProjects(ctx context.Context, runners []string, limit int) (map[string][]tracker.ProjectID, error) {
	if len(runners) < 1 || len(runners) > 200 || limit < 1 || limit > 101 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT r.runner_id,p.project_id FROM runner_identities r CROSS JOIN LATERAL (
 SELECT project_id FROM (
  (SELECT p.id AS project_id FROM projects p WHERE p.organization_id=r.organization_id AND p.deleted_at IS NULL AND (r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id)) ORDER BY p.id LIMIT $3)
  UNION
  (SELECT DISTINCT i.project_id FROM runner_lease_assignments a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE a.organization_id=r.organization_id AND a.runner_id=r.runner_id AND l.released_at IS NULL ORDER BY i.project_id LIMIT $3)
 ) links ORDER BY project_id LIMIT $3
) p WHERE r.organization_id=$1 AND r.runner_id=ANY($2::text[]) ORDER BY r.runner_id,p.project_id`, s.organization, runners, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := map[string][]tracker.ProjectID{}
	for rows.Next() {
		var runner string
		var project tracker.ProjectID
		if err := rows.Scan(&runner, &project); err != nil {
			return nil, mapError(ctx, err)
		}
		result[runner] = append(result[runner], project)
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
