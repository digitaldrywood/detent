package hubserver

import (
	"context"
)

const githubBatchMaintenanceLimit = 100

func (s *Service) maintainGitHubBatches(ctx context.Context) error {
	return ErrPostgresFeatureUnavailable
}

func (s *Service) completeRunnerGitHubBatch(ctx context.Context, scope nativeScope) error {
	return ErrPostgresFeatureUnavailable
}

func (s *Service) advanceGitHubBatch(ctx context.Context, scope nativeScope) error {
	return ErrPostgresFeatureUnavailable
}
