package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const recordedRecoveryCandidate = `(p.profile='native' AND lower(ws.name)='blocked' AND EXISTS (
 SELECT 1 FROM (SELECT ev.actor_kind, ev.event_revision FROM collaboration_events ev
  WHERE ev.organization_id=i.organization_id AND ev.project_id=i.project_id AND ev.work_item_id=i.native_id
  AND ev.type='workflow.transitioned' ORDER BY ev.sequence DESC LIMIT 1) lane
 WHERE lane.actor_kind='runner' AND (lane.event_revision=0 OR lane.event_revision=i.revision)))`

func (s *session) RunnerRecoveryEligibility(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (bool, error) {
	now, err := s.leaseTime(ctx)
	if err != nil {
		return false, err
	}
	var eligible bool
	err = s.db.QueryRow(ctx, `SELECT NOT i.archived
 AND COALESCE(NOT ws.terminal,false)
 AND (ws.dispatchable OR `+recordedRecoveryCandidate+` OR EXISTS (
  SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id
  AND l.released_at IS NULL AND l.expires_at>$4))
 AND lower(trim(COALESCE(ws.name,'')))<>'cancelled'
 AND (NOT p.require_dependencies OR NOT EXISTS (
  SELECT 1 FROM issue_dependencies d
  JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id
  LEFT JOIN project_workflow_states blocker_state ON blocker_state.organization_id=blocker.organization_id AND blocker_state.project_id=blocker.project_id AND blocker_state.name=blocker.workflow_state_name
  WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id
  AND (blocker_state.name IS NULL OR NOT blocker_state.terminal)))
 FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 LEFT JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name
 WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND (p.profile='native' OR (p.profile='github_compatible' AND EXISTS(SELECT 1 FROM project_settings settings JOIN issue_claim_facts facts ON facts.organization_id=settings.organization_id AND facts.project_id=settings.project_id AND facts.work_item_id=i.native_id WHERE settings.organization_id=p.organization_id AND settings.project_id=p.id AND settings.repository_enabled AND lower(facts.repository)=lower(settings.repository)))) AND p.deleted_at IS NULL`, s.organization, project, item, now).Scan(&eligible)
	return eligible, mapError(ctx, err)
}
