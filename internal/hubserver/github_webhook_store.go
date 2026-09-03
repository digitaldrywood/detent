package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return webhookReceiptResult{}, fmt.Errorf("begin GitHub webhook receipt: %w", err)
	}
	defer tx.Rollback()

	receivedAt := formatWebhookTime(receipt.ReceivedAt)
	var inboxID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO github_webhook_inbox (
			delivery_id,
			event_type,
			action,
			headers_json,
			payload_sha256,
			payload_bytes,
			status,
			signature_verified_at,
			received_at,
			last_received_at,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?)
		ON CONFLICT(delivery_id) DO NOTHING
		RETURNING id
	`,
		receipt.DeliveryID,
		receipt.EventType,
		receipt.Action,
		receipt.HeadersJSON,
		receipt.PayloadSHA256,
		len(receipt.Payload),
		receivedAt,
		receivedAt,
		receivedAt,
		receivedAt,
		receivedAt,
	).Scan(&inboxID)
	inserted := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return webhookReceiptResult{}, fmt.Errorf("insert GitHub webhook receipt: %w", err)
	}

	if !inserted {
		var eventType string
		var payloadSHA256 string
		var payloadBytes int
		if err := tx.QueryRowContext(ctx, `
			SELECT id, event_type, payload_sha256, payload_bytes
			FROM github_webhook_inbox
			WHERE delivery_id = ?
		`, receipt.DeliveryID).Scan(&inboxID, &eventType, &payloadSHA256, &payloadBytes); err != nil {
			return webhookReceiptResult{}, fmt.Errorf("read duplicate GitHub webhook receipt: %w", err)
		}
		if eventType != receipt.EventType || payloadSHA256 != receipt.PayloadSHA256 || payloadBytes != len(receipt.Payload) {
			return webhookReceiptResult{}, ErrWebhookDeliveryConflict
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE github_webhook_inbox
			SET redelivery_count = redelivery_count + 1,
				last_received_at = ?,
				updated_at = ?
			WHERE id = ?
		`, receivedAt, receivedAt, inboxID); err != nil {
			return webhookReceiptResult{}, fmt.Errorf("record GitHub webhook redelivery: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO github_webhook_payloads (inbox_id, body, expires_at, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(inbox_id) DO UPDATE SET
			body = excluded.body,
			expires_at = excluded.expires_at,
			created_at = excluded.created_at
	`, inboxID, receipt.Payload, formatWebhookTime(receipt.PayloadExpiresAt), receivedAt); err != nil {
		return webhookReceiptResult{}, fmt.Errorf("store GitHub webhook payload: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return webhookReceiptResult{}, fmt.Errorf("commit GitHub webhook receipt: %w", err)
	}
	return webhookReceiptResult{InboxID: inboxID, Duplicate: !inserted}, nil
}

func (d *database) processWebhook(ctx context.Context, inboxID int64, now time.Time) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin GitHub webhook processing: %w", err)
	}

	var delivery storedWebhook
	err = tx.QueryRowContext(ctx, `
		SELECT i.id, i.delivery_id, i.event_type, COALESCE(i.action, ''), p.body, i.payload_sha256, i.status
		FROM github_webhook_inbox i
		LEFT JOIN github_webhook_payloads p ON p.inbox_id = i.id
		WHERE i.id = ?
	`, inboxID).Scan(
		&delivery.InboxID,
		&delivery.DeliveryID,
		&delivery.EventType,
		&delivery.Action,
		&delivery.Payload,
		&delivery.PayloadSHA256,
		&delivery.Status,
	)
	if err != nil {
		rollbackErr := tx.Rollback()
		return errors.Join(fmt.Errorf("read GitHub webhook delivery: %w", err), rollbackErr)
	}
	if delivery.Status == "processed" || delivery.Status == "ignored" {
		return tx.Rollback()
	}
	if len(delivery.Payload) == 0 {
		processingErr := errors.New("GitHub webhook payload is unavailable")
		rollbackErr := tx.Rollback()
		failureErr := d.markWebhookFailed(ctx, inboxID, now, processingErr)
		return errors.Join(processingErr, rollbackErr, failureErr)
	}

	processedAt := formatWebhookTime(now)
	if _, err := tx.ExecContext(ctx, `
		UPDATE github_webhook_inbox
		SET status = 'processing',
			processing_started_at = ?,
			last_error = NULL,
			updated_at = ?
		WHERE id = ?
	`, processedAt, processedAt, inboxID); err != nil {
		rollbackErr := tx.Rollback()
		return errors.Join(fmt.Errorf("mark GitHub webhook processing: %w", err), rollbackErr)
	}

	result, err := applyWebhook(ctx, tx, delivery, now)
	if err != nil {
		rollbackErr := tx.Rollback()
		failureErr := d.markWebhookFailed(ctx, inboxID, now, err)
		return errors.Join(err, rollbackErr, failureErr)
	}
	status := "processed"
	if result.Outcome == webhookOutcomeIgnored {
		status = "ignored"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE github_webhook_inbox
		SET repository_id = COALESCE(?, repository_id),
			status = ?,
			outcome = ?,
			attempts = attempts + 1,
			next_retry_at = NULL,
			processed_at = ?,
			last_error = NULL,
			updated_at = ?
		WHERE id = ?
	`, result.RepositoryID, status, result.Outcome, processedAt, processedAt, inboxID); err != nil {
		rollbackErr := tx.Rollback()
		failureErr := d.markWebhookFailed(ctx, inboxID, now, err)
		return errors.Join(fmt.Errorf("complete GitHub webhook processing: %w", err), rollbackErr, failureErr)
	}
	if err := tx.Commit(); err != nil {
		failureErr := d.markWebhookFailed(ctx, inboxID, now, err)
		return errors.Join(fmt.Errorf("commit GitHub webhook processing: %w", err), failureErr)
	}
	return nil
}

func (d *database) markWebhookFailed(ctx context.Context, inboxID int64, now time.Time, processingErr error) error {
	timestamp := formatWebhookTime(now)
	_, err := d.db.ExecContext(ctx, `
		UPDATE github_webhook_inbox
		SET status = 'failed',
			attempts = attempts + 1,
			next_retry_at = ?,
			processing_started_at = ?,
			last_error = ?,
			updated_at = ?
		WHERE id = ? AND status NOT IN ('processed', 'ignored')
	`, formatWebhookTime(now.Add(webhookRetryDelay)), timestamp, processingErr.Error(), timestamp, inboxID)
	if err != nil {
		return fmt.Errorf("record GitHub webhook failure: %w", err)
	}
	return nil
}

func (d *database) processPendingWebhooks(ctx context.Context, now time.Time) error {
	inboxIDs, err := d.pendingWebhookIDs(ctx, now)
	if err != nil {
		return err
	}

	var processErr error
	for _, inboxID := range inboxIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(processErr, err)
		}
		if err := d.processWebhook(ctx, inboxID, now); err != nil {
			processErr = errors.Join(processErr, err)
		}
	}
	return processErr
}

func (d *database) pendingWebhookIDs(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id
		FROM github_webhook_inbox
		WHERE status IN ('pending', 'processing', 'failed')
			AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY received_at, id
	`, formatWebhookTime(now))
	if err != nil {
		return nil, fmt.Errorf("list pending GitHub webhooks: %w", err)
	}
	defer rows.Close()
	var inboxIDs []int64
	for rows.Next() {
		var inboxID int64
		if err := rows.Scan(&inboxID); err != nil {
			return nil, fmt.Errorf("scan pending GitHub webhook: %w", err)
		}
		inboxIDs = append(inboxIDs, inboxID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending GitHub webhooks: %w", err)
	}
	return inboxIDs, nil
}

func (d *database) purgeWebhookPayloads(ctx context.Context, now time.Time) (int64, error) {
	result, err := d.db.ExecContext(ctx, `
		DELETE FROM github_webhook_payloads
		WHERE expires_at <= ?
			AND inbox_id IN (
				SELECT id
				FROM github_webhook_inbox
				WHERE status IN ('processed', 'ignored')
			)
	`, formatWebhookTime(now))
	if err != nil {
		return 0, fmt.Errorf("purge GitHub webhook payloads: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count purged GitHub webhook payloads: %w", err)
	}
	return deleted, nil
}

func formatWebhookTime(value time.Time) string {
	return value.UTC().Format(webhookTimeFormat)
}
