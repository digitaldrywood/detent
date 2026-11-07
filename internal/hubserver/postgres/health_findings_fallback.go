package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) LegacyHealthFindings(ctx context.Context, project tracker.ProjectID, state string, since *time.Time, after int64, reporter string, limit int) (healthstore.Page, error) {
	return s.healthFindings(ctx, project, state, since, after, reporter, limit, false)
}
