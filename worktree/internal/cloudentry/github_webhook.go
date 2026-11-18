package cloudentry

import "context"

func (s *Service) initializeGitHubRoutes(ctx context.Context) error {
	return s.githubWebhooks.Initialize(ctx)
}

func (s *Service) refreshGitHubRoutes(ctx context.Context, organization Organization) error {
	if len(s.config.GitHubWebhookSecret) == 0 {
		return nil
	}
	return s.githubWebhooks.Refresh(ctx, organization)
}
