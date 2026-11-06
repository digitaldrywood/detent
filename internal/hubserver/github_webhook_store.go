package hubserver

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	webhookRetryDelay = time.Minute
	webhookTimeFormat = "2006-01-02T15:04:05.000000000Z"
)

type storedWebhook struct {
	InboxID       int64
	DeliveryID    string
	EventType     string
	Action        string
	Payload       []byte
	PayloadSHA256 string
	Status        string
}

type webhookProcessResult struct {
	Outcome      webhookOutcome
	RepositoryID *int64
}

func (d *database) recordWebhook(ctx context.Context, receipt webhookReceipt) (webhookReceiptResult, error) {
	return webhookReceiptResult{}, ErrPostgresFeatureUnavailable
}

func (s *Service) processWebhook(ctx context.Context, inboxID int64, now time.Time) error {
	return ErrPostgresFeatureUnavailable
}

func (d *database) markWebhookFailed(ctx context.Context, inboxID int64, now time.Time, processingErr error) error {
	return ErrPostgresFeatureUnavailable
}

func (d *database) webhookFailureRepository(ctx context.Context, inboxID int64) (int64, bool, error) {
	return 0, false, ErrPostgresFeatureUnavailable
}

func optionalWebhookNodeID(nodeID *string) any {
	if nodeID == nil || strings.TrimSpace(*nodeID) == "" {
		return nil
	}
	return strings.TrimSpace(*nodeID)
}

func (s *Service) processPendingWebhooks(ctx context.Context, now time.Time) error {
	inboxIDs, err := s.database.pendingWebhookIDs(ctx, now)
	if err != nil {
		return err
	}

	var processErr error
	for _, inboxID := range inboxIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(processErr, err)
		}
		if err := s.processWebhook(ctx, inboxID, now); err != nil {
			processErr = errors.Join(processErr, err)
		}
	}
	return processErr
}

func (d *database) pendingWebhookIDs(ctx context.Context, now time.Time) ([]int64, error) {
	return nil, ErrPostgresFeatureUnavailable
}

func (d *database) purgeWebhookPayloads(ctx context.Context, now time.Time, retention time.Duration) (int64, error) {
	return 0, ErrPostgresFeatureUnavailable
}

func formatWebhookTime(value time.Time) string {
	return value.UTC().Format(webhookTimeFormat)
}
