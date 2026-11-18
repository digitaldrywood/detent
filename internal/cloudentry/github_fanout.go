package cloudentry

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/cloudentry/webhooks"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
)

func (s *Service) gitHubFanoutFeature() hubmaintenance.Feature {
	return webhooks.FanoutFeature(s.registry.store.db.postgres, s.config.Now, s.deliverGitHubFanout, s.githubWebhookPayload, s.deleteGitHubWebhookPayload)
}

func (s *Service) deliverGitHubFanout(ctx context.Context, selected hubmaintenance.Tenant, page *platformstore.GitHubFanoutPage) error {
	ctx = entryBackground(ctx)
	org, err := s.readyOrganization(ctx, selected.Organization)
	if errors.Is(err, ErrOrganizationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if org.Generation != selected.Generation {
		return ErrOrganizationConflict
	}
	raw, err := s.githubWebhookPayload(ctx, page.DeliveryID, page.Digest, page.ObjectKey)
	if err != nil {
		return err
	}
	return s.githubWebhooks.Deliver(ctx, org, webhooks.Delivery{
		ID: page.DeliveryID, Event: page.EventType, Repository: page.Repository, BindingProject: page.BindingProject,
	}, raw)
}
