package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/policyfacts"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) PolicyCompatibility(ctx context.Context, project tracker.ProjectID, ids []string) (map[string]policystore.Compatibility, error) {
	return policyfacts.Policies(ctx, s.transactionHandle(), project, ids)
}

func (s *session) ReviewCompatibility(ctx context.Context, project tracker.ProjectID, ids []string) (map[string]policystore.ReviewCompatibility, error) {
	return policyfacts.Reviews(ctx, s.transactionHandle(), project, ids)
}
