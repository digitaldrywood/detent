package postgres

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AuthorizeRecordedRecovery(ctx context.Context, recovery leasestore.RecordedRecovery) error {
	attempt := recovery.Attempt
	if attempt.OrganizationID != s.organization || attempt.IssueID <= 0 || attempt.ID == "" || recovery.Now.IsZero() || len(recovery.DependencyNumbers) > corestore.MaxPageSize || len(recovery.MergedDependencyNumbers) > corestore.MaxPageSize {
		return corestore.ErrRevisionConflict
	}
	var current, history, linked, ready bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM native_attempts a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
 WHERE a.organization_id=$1 AND a.issue_id=$3 AND a.id=$4 AND a.sequence=$5 AND a.status=$19 AND a.status IN ('succeeded','failed','interrupted','cancelled')
 AND a.payload_object_key=$6 AND a.checkpoint_object_key IS NOT DISTINCT FROM $7::text
 AND a.lease_id=$8 AND a.fencing_token=$9 AND l.issue_id=a.issue_id AND l.fencing_token=a.fencing_token
 AND ($21::bigint=0 OR EXISTS (SELECT 1 FROM issues item
  WHERE item.organization_id=$1 AND item.project_id=$2 AND item.id=$3 AND item.native_id=$11
  AND item.revision=$21 AND NOT item.archived AND lower(item.workflow_state_name)='blocked'))
 AND NOT EXISTS (SELECT 1 FROM native_attempts newer WHERE newer.organization_id=$1 AND newer.issue_id=$3 AND newer.fencing_token>a.fencing_token)
 AND NOT EXISTS (SELECT 1 FROM leases active WHERE active.organization_id=$1 AND active.issue_id=$3 AND active.released_at IS NULL AND active.expires_at>$10)
),
EXISTS (
 SELECT 1 FROM collaboration_events report JOIN collaboration_events lane
 ON lane.organization_id=report.organization_id AND lane.project_id=report.project_id AND lane.work_item_id=report.work_item_id
 WHERE report.organization_id=$1 AND report.project_id=$2 AND report.work_item_id=$11
 AND report.id=$12 AND report.sequence=$13 AND report.type='run.finished' AND report.attempt_id=$4
 AND lane.id=$14 AND lane.sequence=$15 AND lane.payload_object_key=$16 AND lane.type='workflow.transitioned'
 AND NOT EXISTS (SELECT 1 FROM collaboration_events newer
  WHERE newer.organization_id=$1 AND newer.project_id=$2 AND newer.work_item_id=$11
  AND newer.type='workflow.transitioned' AND newer.sequence>$15)
 AND NOT EXISTS (SELECT 1 FROM collaboration_events changed
  WHERE changed.organization_id=$1 AND changed.project_id=$2 AND changed.work_item_id=$11
  AND changed.sequence>$13 AND changed.sequence<>$15 AND changed.type<>'scheduler.decision'
  AND changed.event_revision>$17)
 AND NOT EXISTS (
  SELECT 1 FROM unnest(ARRAY['comment.created','comment.edited']) kind(type)
  WHERE (SELECT count(*)>100 OR coalesce(bool_or(comment.actor_kind='human'),false)
   FROM (SELECT actor_kind FROM collaboration_events
    WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$11 AND type=kind.type AND sequence>$13
    ORDER BY sequence DESC LIMIT 101) comment)
 )
),
(SELECT count(DISTINCT reported.number) FROM unnest($18::bigint[]) reported(number)
 CROSS JOIN LATERAL (
  SELECT b.id FROM issues b
  WHERE b.organization_id=$1 AND b.project_id=$2 AND b.number=reported.number OFFSET 0
 ) b CROSS JOIN LATERAL (
  SELECT d.blocker_issue_id FROM issue_dependencies d
  WHERE d.organization_id=$1 AND d.dependent_project_id=$2 AND d.dependent_issue_id=$3
  AND d.blocker_project_id=$2 AND d.blocker_issue_id=b.id OFFSET 0
 ) linked)=cardinality($18::bigint[]),
NOT EXISTS (
 SELECT 1 FROM (
  SELECT d.blocker_project_id,d.blocker_issue_id FROM issue_dependencies d
  WHERE d.organization_id=$1 AND d.dependent_project_id=$2 AND d.dependent_issue_id=$3
  ORDER BY d.blocker_project_id,d.blocker_issue_id OFFSET 0
 ) d
 LEFT JOIN LATERAL (
  SELECT b.workflow_state_name FROM issues b
  WHERE b.organization_id=$1 AND b.project_id=d.blocker_project_id AND b.id=d.blocker_issue_id OFFSET 0
 ) b ON true
 LEFT JOIN LATERAL (
  SELECT state.terminal FROM project_workflow_states state
  WHERE state.organization_id=$1 AND state.project_id=d.blocker_project_id AND state.name=b.workflow_state_name OFFSET 0
 ) state ON true
 WHERE state.terminal IS DISTINCT FROM true
)
AND NOT EXISTS (
 SELECT 1 FROM unnest($20::bigint[]) merged(number)
 WHERE NOT EXISTS (
  SELECT 1 FROM issues b
  JOIN change_requests c ON c.organization_id=b.organization_id AND c.project_id=b.project_id AND c.work_item_id=b.native_id
  JOIN change_versions v ON v.organization_id=c.organization_id AND v.project_id=c.project_id
   AND v.change_id=c.id AND v.id=c.landed_version_id AND v.landed_merge_sha=c.landed_merge_sha
  WHERE b.organization_id=$1 AND b.project_id=$2 AND b.number=merged.number AND v.landing_history_known AND v.landed_at IS NOT NULL
 )
)`, s.organization, recovery.ProjectID, attempt.IssueID, attempt.ID, attempt.Sequence, attempt.PayloadObjectKey, attempt.CheckpointObjectKey, attempt.LeaseID, attempt.FencingToken, recovery.Now, recovery.WorkItemID, recovery.ReportEventID, recovery.ReportSequence, recovery.WorkflowEventID, recovery.WorkflowSequence, recovery.WorkflowObjectKey, recovery.ReportRevision, recovery.DependencyNumbers, attempt.Status, recovery.MergedDependencyNumbers, recovery.ExpectedRevision).Scan(&current, &history, &linked, &ready)
	if err != nil {
		return mapError(ctx, err)
	}
	if !current {
		return tracker.ErrStaleFencingToken
	}
	if !history {
		return fmt.Errorf("verify recorded recovery history: %w", corestore.ErrRevisionConflict)
	}
	if !linked {
		return fmt.Errorf("verify recorded recovery dependencies: %w", corestore.ErrRevisionConflict)
	}
	if !ready {
		return fmt.Errorf("verify recorded recovery readiness: %w", corestore.ErrRevisionConflict)
	}
	return nil
}
