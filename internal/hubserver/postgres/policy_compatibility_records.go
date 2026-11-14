package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
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

func (s *session) ReviewPolicyRevision(ctx context.Context, project tracker.ProjectID, id string) (eventstore.ReviewPolicy, error) {
	return policyfacts.ReviewRevision(ctx, s.transactionHandle(), project, id)
}

func (s *session) BackfillPolicyCompatibility(ctx context.Context, row policystore.Revision) error {
	return policyfacts.BackfillPolicy(ctx, s.transactionHandle(), row)
}

func (s *session) BackfillReviewCompatibility(ctx context.Context, row eventstore.ReviewPolicy) error {
	return policyfacts.BackfillReview(ctx, s.transactionHandle(), row)
}
