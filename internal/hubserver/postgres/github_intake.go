package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) NextGitHubIntake(ctx context.Context, now time.Time) (tracker.ProjectID, error) {
	var project tracker.ProjectID
	err := s.db.QueryRow(ctx, `SELECT i.project_id FROM onboarding_issue_intake i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
WHERE i.organization_id=$1 AND i.hub_owned AND i.batch_status IN ('discovering','importing') AND i.next_attempt_at<=$2 AND p.deleted_at IS NULL ORDER BY i.next_attempt_at,i.project_id LIMIT 1`, s.organization, now).Scan(&project)
	return project, mapError(ctx, err)
}

const webhookColumns = `delivery_id,repository,event_type,action,payload_digest,status,after_project,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,received_at,expires_at,next_attempt_at,attempts`

func (s *session) readGitHubWebhook(ctx context.Context, predicate string, args ...any) (projectstore.GitHubWebhook, error) {
	var row projectstore.GitHubWebhook
	row.Payload.OrganizationID = s.organization
	err := s.db.QueryRow(ctx, `SELECT `+webhookColumns+` FROM github_webhook_receipts WHERE organization_id=$1 AND `+predicate, args...).Scan(&row.DeliveryID, &row.Repository, &row.EventType, &row.Action, &row.Digest, &row.Status, &row.AfterProject, &row.Payload.PayloadObjectKey, &row.Payload.PayloadSHA256, &row.Payload.PayloadBytes, &row.Payload.PayloadContentBytes, &row.ReceivedAt, &row.ExpiresAt, &row.NextAttemptAt, &row.Attempts)
	return row, mapError(ctx, err)
}

func (s *session) GitHubWebhook(ctx context.Context, id string) (projectstore.GitHubWebhook, error) {
	return s.readGitHubWebhook(ctx, `delivery_id=$2`, s.organization, id)
}

func (s *session) NextGitHubWebhook(ctx context.Context, now time.Time) (projectstore.GitHubWebhook, error) {
	return s.readGitHubWebhook(ctx, `status='pending' AND next_attempt_at<=$2 ORDER BY next_attempt_at,delivery_id LIMIT 1`, s.organization, now)
}

func (s *session) GitHubWebhookProjects(ctx context.Context, repository string, after tracker.ProjectID, limit int) ([]tracker.ProjectID, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT p.id FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
WHERE p.organization_id=$1 AND p.id>$3 AND p.deleted_at IS NULL AND p.profile='native' AND (lower(ps.repository)=lower($2) OR lower(ps.checkout_repository)=lower($2)) ORDER BY p.id LIMIT $4`, s.organization, repository, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []tracker.ProjectID
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, id)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) CreateGitHubWebhook(ctx context.Context, row projectstore.GitHubWebhook) (bool, error) {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return false, err
	}
	if err := validRecordPayload(s.organization, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes); err != nil {
		return false, err
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO github_webhook_receipts(organization_id,delivery_id,repository,event_type,action,payload_digest,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,received_at,expires_at,next_attempt_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$11) ON CONFLICT(organization_id,delivery_id) DO NOTHING`, s.organization, row.DeliveryID, row.Repository, row.EventType, row.Action, row.Digest, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes, row.ReceivedAt, row.ExpiresAt)
	return tag.RowsAffected() == 1, mapError(ctx, err)
}

func (s *session) UpdateGitHubWebhook(ctx context.Context, row projectstore.GitHubWebhook, after tracker.ProjectID, attempts int64) error {
	tag, err := s.db.Exec(ctx, `UPDATE github_webhook_receipts SET status=$3,after_project=$4,attempts=$5,next_attempt_at=$6 WHERE organization_id=$1 AND delivery_id=$2 AND after_project=$7 AND attempts=$8 AND status='pending'`, s.organization, row.DeliveryID, row.Status, row.AfterProject, row.Attempts, row.NextAttemptAt, after, attempts)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) PurgeGitHubWebhookPayloads(ctx context.Context, now time.Time, limit int) error {
	if err := checkPage(limit); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE github_webhook_receipts SET payload_object_key='',payload_sha256='',payload_bytes=0,payload_content_bytes=0 WHERE organization_id=$1 AND delivery_id IN (SELECT delivery_id FROM github_webhook_receipts WHERE organization_id=$1 AND status='completed' AND expires_at<=$2 AND payload_object_key<>'' ORDER BY expires_at,delivery_id LIMIT $3)`, s.organization, now, limit)
	return mapError(ctx, err)
}

func (s *session) DeferGitHubIntake(ctx context.Context, project tracker.ProjectID, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE onboarding_issue_intake SET attempts=attempts+1,next_attempt_at=$3+(LEAST(256,power(2,LEAST(attempts+1,8)))*interval '1 second') WHERE organization_id=$1 AND project_id=$2 AND hub_owned AND batch_status IN ('discovering','importing')`, s.organization, project, now)
	return mapError(ctx, err)
}
