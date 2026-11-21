package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ClaimCapacityUsage(ctx context.Context, machine, runner string, project tracker.ProjectID, now time.Time) (claimstore.CapacityUsage, error) {
	const statement = `SELECT count(*),count(*) FILTER (WHERE a.runner_id=$3),
 count(*) FILTER (WHERE a.runner_id=$3 AND i.project_id=$4)
 FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 LEFT JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
 WHERE l.organization_id=$1 AND l.machine_id=$2 AND l.released_at IS NULL AND l.expires_at>$5`
	var usage claimstore.CapacityUsage
	err := s.db.QueryRow(ctx, statement, s.organization, machine, runner, project, now).Scan(&usage.Host, &usage.Runner, &usage.Project)
	return usage, mapError(ctx, err)
}
