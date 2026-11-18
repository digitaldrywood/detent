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
WHERE i.organization_id=$1 AND i.batch_status IN ('discovering','importing') AND i.next_attempt_at<=$2 AND p.deleted_at IS NULL ORDER BY i.next_attempt_at,i.project_id LIMIT 1`, s.organization, now).Scan(&project)
	return project, mapError(ctx, err)
}

const webhookColumns = `delivery_id,repository,event_type,action,payload_digest,status,after_project,binding_project,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,received_at,expires_at,next_attempt_at,attempts`

func (s *session) readGitHubWebhook(ctx context.Context, predicate string, args ...any) (projectstore.GitHubWebhook, error) {
	var row projectstore.GitHubWebhook
	row.Payload.OrganizationID = s.organization
	err := s.db.QueryRow(ctx, `SELECT `+webhookColumns+` FROM github_webhook_receipts WHERE organization_id=$1 AND `+predicate, args...).Scan(&row.DeliveryID, &row.Repository, &row.EventType, &row.Action, &row.Digest, &row.Status, &row.AfterProject, &row.BindingProject, &row.Payload.PayloadObjectKey, &row.Payload.PayloadSHA256, &row.Payload.PayloadBytes, &row.Payload.PayloadContentBytes, &row.ReceivedAt, &row.ExpiresAt, &row.NextAttemptAt, &row.Attempts)
	return row, mapError(ctx, err)
}

func (s *session) GitHubWebhook(ctx context.Context, id string, project tracker.ProjectID) (projectstore.GitHubWebhook, error) {
	return s.readGitHubWebhook(ctx, `delivery_id=$2 AND binding_project=$3`, s.organization, id, project)
}

func (s *session) LatestGitHubWebhookReceipt(ctx context.Context) (time.Time, error) {
	var received time.Time
	err := s.db.QueryRow(ctx, `SELECT COALESCE(last_received_at,received_at) FROM github_webhook_receipts WHERE organization_id=$1 ORDER BY COALESCE(last_received_at,received_at) DESC,delivery_id DESC LIMIT 1`, s.organization).Scan(&received)
	return received, mapError(ctx, err)
}

func (s *session) NextGitHubWebhook(ctx context.Context, now time.Time) (projectstore.GitHubWebhook, error) {
	return s.readGitHubWebhook(ctx, `status='pending' AND next_attempt_at<=$2 ORDER BY next_attempt_at,delivery_id LIMIT 1`, s.organization, now)
}

func (s *session) GitHubWebhookProjects(ctx context.Context, receipt projectstore.GitHubWebhook, limit int) ([]tracker.ProjectID, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	query := `SELECT p.id FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
WHERE p.organization_id=$1 AND p.id>$3 AND p.deleted_at IS NULL AND p.profile='native' AND (lower(ps.repository)=lower($2) OR lower(ps.checkout_repository)=lower($2))`
	args := []any{s.organization, receipt.Repository, receipt.AfterProject, limit}
	if receipt.BindingProject != "" {
		query += ` AND p.id=$5 AND EXISTS (
			SELECT FROM github_webhook_receipts r JOIN hosted_tenant h ON h.organization_id=r.organization_id
			WHERE r.organization_id=p.organization_id AND r.delivery_id=$6 AND r.binding_project=p.id
			  AND r.binding_generation=h.allocation_generation AND r.binding_project_xid IS NOT DISTINCT FROM p.github_binding_xid
			  AND ((lower(ps.repository)=lower($2) AND r.binding_repository_xid IS NOT DISTINCT FROM ps.github_repository_xid)
			    OR (lower(ps.checkout_repository)=lower($2) AND r.binding_checkout_xid IS NOT DISTINCT FROM ps.github_checkout_xid)))`
		args = append(args, receipt.BindingProject, receipt.DeliveryID)
	}
	rows, err := s.db.Query(ctx, query+" ORDER BY p.id LIMIT $4", args...)
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

func (s *session) GitHubWebhookAuthorized(ctx context.Context, row projectstore.GitHubWebhook) (bool, error) {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return false, err
	}
	var allowed bool
	err := s.db.QueryRow(ctx, "SELECT public.hosted_github_webhook_authorized($1,$2,$3,$4,$5,$6)", row.DeliveryID, row.BindingProject, row.Repository, row.EventType, row.Digest, !s.readOnly).Scan(&allowed)
	return allowed, mapError(ctx, err)
}

func (s *session) CreateGitHubWebhook(ctx context.Context, row projectstore.GitHubWebhook) (bool, error) {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return false, err
	}
	if err := validRecordPayload(s.organization, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes); err != nil {
		return false, err
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO github_webhook_receipts(organization_id,delivery_id,repository,event_type,action,payload_digest,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,received_at,last_received_at,expires_at,next_attempt_at,binding_project,binding_generation,binding_project_xid,binding_repository_xid,binding_checkout_xid)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$11,$13,
 CASE WHEN $13='' THEN 0 ELSE (SELECT allocation_generation FROM hosted_tenant WHERE organization_id=$1) END,
 p.github_binding_xid,ps.github_repository_xid,ps.github_checkout_xid
 FROM (VALUES ($1::text,$13::text)) AS accepted(organization_id,project_id)
 LEFT JOIN projects p ON p.organization_id=accepted.organization_id AND p.id=accepted.project_id
 LEFT JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 ON CONFLICT(organization_id,delivery_id,binding_project) DO NOTHING`, s.organization, row.DeliveryID, row.Repository, row.EventType, row.Action, row.Digest, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes, row.ReceivedAt, row.ExpiresAt, row.BindingProject)
	return tag.RowsAffected() == 1, mapError(ctx, err)
}

func (s *session) ObserveGitHubWebhookReceipt(ctx context.Context, row projectstore.GitHubWebhook) error {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE github_webhook_receipts SET last_received_at=GREATEST(COALESCE(last_received_at,received_at),$6) WHERE organization_id=$1 AND delivery_id=$2 AND payload_digest=$3 AND event_type=$4 AND repository=$5 AND binding_project=$7`, s.organization, row.DeliveryID, row.Digest, row.EventType, row.Repository, row.ReceivedAt, row.BindingProject)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) UpdateGitHubWebhook(ctx context.Context, row projectstore.GitHubWebhook, after tracker.ProjectID, attempts int64) error {
	tag, err := s.db.Exec(ctx, `UPDATE github_webhook_receipts SET status=$3,after_project=$4,attempts=$5,next_attempt_at=$6 WHERE organization_id=$1 AND delivery_id=$2 AND after_project=$7 AND attempts=$8 AND binding_project=$9 AND status='pending'`, s.organization, row.DeliveryID, row.Status, row.AfterProject, row.Attempts, row.NextAttemptAt, after, attempts, row.BindingProject)
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
	_, err := s.db.Exec(ctx, `UPDATE github_webhook_receipts SET payload_object_key='',payload_sha256='',payload_bytes=0,payload_content_bytes=0 WHERE organization_id=$1 AND (delivery_id,binding_project) IN (SELECT delivery_id,binding_project FROM github_webhook_receipts WHERE organization_id=$1 AND status='completed' AND expires_at<=$2 AND payload_object_key<>'' ORDER BY expires_at,delivery_id LIMIT $3)`, s.organization, now, limit)
	return mapError(ctx, err)
}

func (s *session) DeferGitHubIntake(ctx context.Context, project tracker.ProjectID, now time.Time, jitter float64) error {
	_, err := s.db.Exec(ctx, `UPDATE onboarding_issue_intake SET attempts=attempts+1,next_attempt_at=$3::timestamptz+(LEAST(256,power(2,LEAST(attempts+1,8)))*(0.5+0.5*GREATEST(0,LEAST(1,$4::double precision)))*interval '1 second') WHERE organization_id=$1 AND project_id=$2 AND batch_status IN ('discovering','importing')`, s.organization, project, now, jitter)
	return mapError(ctx, err)
}
