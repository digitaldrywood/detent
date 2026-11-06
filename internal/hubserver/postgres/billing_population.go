package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) BillingPopulation(ctx context.Context, metric string, now time.Time, connected time.Duration) (int64, bool, error) {
	const limit = 10001
	statements := map[string]string{
		"members":            `SELECT count(*) FROM(SELECT user_id FROM hosted_members WHERE organization_id=$1 AND active UNION ALL SELECT email FROM hosted_member_reservations WHERE organization_id=$1 AND expires_at>$2 LIMIT $3) population`,
		"projects":           `SELECT count(*) FROM(SELECT id FROM projects WHERE organization_id=$1 AND deleted_at IS NULL AND $2::timestamptz IS NOT NULL LIMIT $3) population`,
		"repositories":       `SELECT count(*) FROM(SELECT DISTINCT s.repository FROM project_settings s JOIN projects p ON p.organization_id=s.organization_id AND p.id=s.project_id WHERE s.organization_id=$1 AND s.repository_enabled AND s.repository<>'' AND p.deleted_at IS NULL AND $2::timestamptz IS NOT NULL LIMIT $3) population`,
		"registered_runners": `SELECT count(*) FROM(SELECT r.runner_id FROM runner_identities r JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id WHERE r.organization_id=$1 AND r.removed_at IS NULL AND t.revoked_at IS NULL AND $2::timestamptz IS NOT NULL LIMIT $3) population`,
		"connected_runners":  `SELECT count(*) FROM(SELECT r.runner_id FROM runner_identities r JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id WHERE r.organization_id=$1 AND r.removed_at IS NULL AND t.revoked_at IS NULL AND r.last_heartbeat_at>$2 LIMIT $3) population`,
		"concurrent_work":    `SELECT count(*) FROM(SELECT lease_id FROM leases WHERE organization_id=$1 AND released_at IS NULL AND renewed_at<=$2 AND expires_at>$2 LIMIT $3) population`,
		"unarchived_issues":  `SELECT count(*) FROM(SELECT i.id FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id LEFT JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name WHERE i.organization_id=$1 AND NOT i.archived AND p.deleted_at IS NULL AND p.profile='native' AND NOT coalesce(w.terminal,false) AND $2::timestamptz IS NOT NULL LIMIT $3) population`,
	}
	statement, ok := statements[metric]
	if !ok {
		return 0, false, corestore.ErrInvalidPage
	}
	after := now
	if metric == "connected_runners" {
		if connected <= 0 {
			return 0, false, corestore.ErrInvalidPage
		}
		after = now.Add(-connected)
	}
	var count int64
	err := s.db.QueryRow(ctx, statement, s.organization, after, limit).Scan(&count)
	return count, count < limit, mapError(ctx, err)
}
