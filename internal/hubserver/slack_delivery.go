package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) slackDelivery() operatorcommands.SlackDeliveryDeps {
	deps := operatorcommands.SlackDeliveryDeps{
		AccessRead: s.accessRead, AccessWrite: s.accessWrite, IssuePath: s.readIssueBrowserPath,
		Now: s.config.Clock, BasePath: s.hostedBase, Keyring: s.config.SecretKeys,
		HTTPClient: s.config.SlackHTTPClient, BusyTimeout: s.config.BusyTimeout,
		PublicURL: func() string {
			if s.config.Hosted != nil {
				return s.config.Hosted.PublicURL
			}
			return ""
		},
	}
	if s.database != nil && s.database.postgres != nil {
		deps.Store = s.database.postgres
	}
	return deps
}

func (s *Service) readSlackWebhook(ctx context.Context, organization tracker.OrganizationID, actor string) ([]byte, error) {
	return s.slackDelivery().ReadWebhook(ctx, organization, actor)
}

func (s *Service) processSlackDelivery(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	return s.slackDelivery().Process(ctx, organization)
}
