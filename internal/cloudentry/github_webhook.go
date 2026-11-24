package cloudentry

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/cloudentry/webhooks"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *Service) initializeGitHubRoutes(ctx context.Context) error {
	return s.githubWebhooks.Initialize(ctx)
}

func (s *Service) refreshGitHubRoutes(ctx context.Context, organization Organization) error {
	if len(s.config.GitHubWebhookSecret) == 0 {
		return nil
	}
	return s.githubWebhooks.Refresh(ctx, organization)
}

func (s *Service) acceptGitHubWebhook(ctx context.Context, delivery webhooks.Delivery, payload []byte) (bool, error) {
	digest := artifact.Digest(payload)
	record := platformstore.GitHubIngress{DeliveryID: delivery.ID, EventType: delivery.Event, Repository: delivery.Repository, Digest: digest, ObjectKey: webhooks.PayloadKey(delivery.ID, digest) + "/" + uuid.NewString()}
	store := s.registry.store.db.postgres
	now := s.config.Now()
	if s.attachments == nil {
		return false, artifact.ErrStorage
	}
	if duplicate, err := store.BeginGitHubIngress(ctx, &record, now); duplicate || err != nil {
		return duplicate, err
	}
	if err := webhooks.StorePayload(ctx, s.attachments, delivery.ID, digest, record.ObjectKey, payload); err != nil {
		return false, err
	}
	created, err := store.AcceptGitHubIngress(ctx, record, s.config.Now())
	if !created && (err == nil || errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrNotFound)) {
		released, checkErr := store.GitHubIngressReleased(ctx, delivery.ID, digest, record.ObjectKey)
		if released {
			checkErr = s.deleteGitHubWebhookPayload(ctx, delivery.ID, digest, record.ObjectKey)
		}
		err = errors.Join(err, checkErr)
	}
	return !created, err
}

func (s *Service) githubWebhookPayload(ctx context.Context, deliveryID, digest, key string) ([]byte, error) {
	return webhooks.ReadPayload(ctx, s.attachments, deliveryID, digest, key)
}

func (s *Service) deleteGitHubWebhookPayload(ctx context.Context, deliveryID, digest, key string) error {
	if s.attachments == nil || !webhooks.ValidPayloadKey(deliveryID, digest, key) {
		return artifact.ErrIntegrity
	}
	return s.attachments.Delete(ctx, key)
}
