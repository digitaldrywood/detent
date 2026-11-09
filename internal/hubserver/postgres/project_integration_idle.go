package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RequireProjectIntegrationIdle(ctx context.Context, project tracker.ProjectID, _ time.Time) error {
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: project}); err != nil {
		return mapError(ctx, err)
	}
	now, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return mapError(ctx, err)
	}
	var active bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 WHERE l.organization_id=$1 AND i.project_id=$2 AND l.released_at IS NULL AND l.expires_at>$3
 ) OR EXISTS (
 SELECT 1 FROM github_outbound o WHERE o.organization_id=$1 AND o.project_id=$2 AND o.status='processing'
 )`, s.organization, project, now).Scan(&active)
	if err != nil {
		return mapError(ctx, err)
	}
	if active {
		return projectstore.ErrIntegrationActiveWork
	}
	return nil
}
