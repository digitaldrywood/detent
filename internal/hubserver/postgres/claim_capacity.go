package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ClaimCapacityUsage(ctx context.Context, machine, runner string, project tracker.ProjectID, batch string, now time.Time) (claimstore.CapacityUsage, error) {
	const statement = `WITH active AS (
 SELECT i.project_id,COALESCE(a.runner_id,'') AS runner_id,
 CASE WHEN lower(trim(i.workflow_state_name))='merging' AND p.profile='native'
 AND policy.gate_github_pull_request IS FALSE AND l.session_id ~ '^landing/[^/]+/[^/]+$'
 THEN split_part(l.session_id,'/',2) ELSE '' END AS batch,
 l.lease_id
 FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 LEFT JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
 LEFT JOIN LATERAL (
  SELECT c.current_version_id,c.id FROM change_issue_links link
  JOIN change_requests c ON c.organization_id=link.organization_id AND c.project_id=link.project_id AND c.id=link.change_id
  WHERE link.organization_id=i.organization_id AND link.project_id=i.project_id AND link.work_item_id=i.native_id
  ORDER BY link.created_at DESC,link.change_id DESC LIMIT 1
 ) change ON true
 LEFT JOIN change_versions v ON v.organization_id=l.organization_id AND v.change_id=change.id AND v.id=change.current_version_id
 LEFT JOIN policy_revisions policy ON policy.organization_id=l.organization_id AND policy.project_id=i.project_id AND policy.policy_id=v.policy_revision_id
 WHERE l.organization_id=$1 AND l.machine_id=$2 AND l.released_at IS NULL AND l.expires_at>$6
), slots AS (
 SELECT DISTINCT project_id,runner_id,CASE WHEN batch<>'' THEN 'batch/'||batch ELSE 'lease/'||lease_id END AS slot
 FROM active WHERE NOT ($5<>'' AND batch=$5 AND runner_id=$3 AND project_id=$4)
)
SELECT count(*),count(*) FILTER (WHERE runner_id=$3),count(*) FILTER (WHERE runner_id=$3 AND project_id=$4) FROM slots`
	var usage claimstore.CapacityUsage
	err := s.db.QueryRow(ctx, statement, s.organization, machine, runner, project, batch, now).Scan(&usage.Host, &usage.Runner, &usage.Project)
	return usage, mapError(ctx, err)
}
