package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
)

func (s *session) EnqueueGitHubOutbound(ctx context.Context, row projectstore.GitHubOutbound) error {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return err
	}
	if err := validRecordPayload(s.organization, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO github_outbound(organization_id,project_id,work_item_id,command_key,repository,issue_number,kind,target_node_id,next_attempt_at,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11,$12,$13) ON CONFLICT(organization_id,command_key) DO NOTHING`, s.organization, row.ProjectID, row.WorkItemID, row.Key, row.Repository, row.IssueNumber, row.Kind, row.TargetNodeID, row.CreatedAt, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes)
	return mapError(ctx, err)
}

func (s *session) ClaimGitHubOutbound(ctx context.Context, now, until time.Time, lease string) (projectstore.GitHubOutbound, error) {
	var row projectstore.GitHubOutbound
	row.Payload.OrganizationID = s.organization
	err := s.db.QueryRow(ctx, `WITH candidate AS (SELECT id FROM github_outbound WHERE organization_id=$1 AND status<>'completed' AND next_attempt_at<=$2 ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
UPDATE github_outbound o SET status='processing',lease_id=$3,next_attempt_at=$4,attempts=attempts+1 FROM candidate c WHERE o.organization_id=$1 AND o.id=c.id
RETURNING o.id,o.project_id,o.work_item_id,o.command_key,o.repository,o.issue_number,o.kind,o.target_node_id,o.status,o.lease_id,o.attempts,o.created_at,o.next_attempt_at,o.payload_object_key,o.payload_sha256,o.payload_bytes,o.payload_content_bytes`, s.organization, now, lease, until).Scan(&row.ID, &row.ProjectID, &row.WorkItemID, &row.Key, &row.Repository, &row.IssueNumber, &row.Kind, &row.TargetNodeID, &row.Status, &row.LeaseID, &row.Attempts, &row.CreatedAt, &row.NextAttemptAt, &row.Payload.PayloadObjectKey, &row.Payload.PayloadSHA256, &row.Payload.PayloadBytes, &row.Payload.PayloadContentBytes)
	return row, mapError(ctx, err)
}

func (s *session) FinishGitHubOutbound(ctx context.Context, row projectstore.GitHubOutbound) error {
	tag, err := s.db.Exec(ctx, `UPDATE github_outbound SET status=$4,next_attempt_at=$5,lease_id='' WHERE organization_id=$1 AND id=$2 AND lease_id=$3 AND status='processing'`, s.organization, row.ID, row.LeaseID, row.Status, row.NextAttemptAt)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
