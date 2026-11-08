package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
)

func (s *session) EnqueueGitHubOutbound(ctx context.Context, row projectstore.GitHubOutbound) error {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return err
	}
	if err := validRecordPayload(s.organization, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO github_outbound(organization_id,project_id,work_item_id,command_key,repository,issue_number,kind,target_node_id,next_attempt_at,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,outcome)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11,$12,$13,'unattempted') ON CONFLICT(organization_id,command_key) DO NOTHING`, s.organization, row.ProjectID, row.WorkItemID, row.Key, row.Repository, row.IssueNumber, row.Kind, row.TargetNodeID, row.CreatedAt, row.Payload.PayloadObjectKey, row.Payload.PayloadSHA256, row.Payload.PayloadBytes, row.Payload.PayloadContentBytes)
	return mapError(ctx, err)
}

const gitHubOutboundColumns = `o.id,o.project_id,o.work_item_id,o.command_key,o.repository,o.issue_number,o.kind,o.target_node_id,o.status,o.lease_id,o.attempts,o.created_at,o.next_attempt_at,o.payload_object_key,o.payload_sha256,o.payload_bytes,o.payload_content_bytes,o.outcome,o.phase,o.page_cursor,o.target_id,o.fallback_id,o.verification,o.page_sequence,o.source_updated_at,
EXISTS(SELECT 1 FROM github_outbound_observations b WHERE b.organization_id=o.organization_id AND b.outbound_id=o.id AND b.verification=o.verification AND NOT b.ready)`

func (s *session) scanGitHubOutbound(result pgx.Row, ctx context.Context) (projectstore.GitHubOutbound, error) {
	var row projectstore.GitHubOutbound
	row.Payload.OrganizationID = s.organization
	err := result.Scan(&row.ID, &row.ProjectID, &row.WorkItemID, &row.Key, &row.Repository, &row.IssueNumber, &row.Kind, &row.TargetNodeID, &row.Status, &row.LeaseID, &row.Attempts, &row.CreatedAt, &row.NextAttemptAt, &row.Payload.PayloadObjectKey, &row.Payload.PayloadSHA256, &row.Payload.PayloadBytes, &row.Payload.PayloadContentBytes, &row.Outcome, &row.Progress.Stage, &row.Progress.Cursor, &row.Progress.TargetID, &row.Progress.FallbackID, &row.Progress.Verification, &row.Progress.Page, &row.Progress.SourceUpdatedAt, &row.Blocked)
	return row, mapError(ctx, err)
}

func (s *session) GitHubOutbound(ctx context.Context, key string) (projectstore.GitHubOutbound, error) {
	return s.scanGitHubOutbound(s.db.QueryRow(ctx, `SELECT `+gitHubOutboundColumns+` FROM github_outbound o WHERE o.organization_id=$1 AND o.command_key=$2`, s.organization, key), ctx)
}

func (s *session) ClaimGitHubOutbound(ctx context.Context, now, until time.Time, lease string) (projectstore.GitHubOutbound, error) {
	if lease == "" || !until.After(now) {
		return projectstore.GitHubOutbound{}, eventstore.ErrInvalidRecord
	}
	return s.scanGitHubOutbound(s.db.QueryRow(ctx, `WITH candidate AS (SELECT id FROM github_outbound WHERE organization_id=$1 AND status<>'completed' AND next_attempt_at<=$2 ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
UPDATE github_outbound o SET status='processing',lease_id=$3,next_attempt_at=$4,attempts=attempts+1 FROM candidate c WHERE o.organization_id=$1 AND o.id=c.id
RETURNING `+gitHubOutboundColumns, s.organization, now, lease, until), ctx)
}

func (s *session) RenewGitHubOutbound(ctx context.Context, id int64, lease string, now, until time.Time, writing bool) error {
	if !until.After(now) {
		return eventstore.ErrInvalidRecord
	}
	tag, err := s.db.Exec(ctx, `UPDATE github_outbound SET next_attempt_at=GREATEST(next_attempt_at,$5),outcome=CASE WHEN $6 THEN 'uncertain' ELSE outcome END,
phase=CASE WHEN $6 THEN '' ELSE phase END,page_cursor=CASE WHEN $6 THEN '' ELSE page_cursor END,target_id=CASE WHEN $6 THEN 0 ELSE target_id END,fallback_id=CASE WHEN $6 THEN 0 ELSE fallback_id END,page_sequence=CASE WHEN $6 THEN 0 ELSE page_sequence END
WHERE organization_id=$1 AND id=$2 AND lease_id=$3 AND status='processing' AND next_attempt_at>$4`, s.organization, id, lease, now, until, writing)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) FinishGitHubOutbound(ctx context.Context, row projectstore.GitHubOutbound, now time.Time) error {
	if len(row.Observations) > 100 || row.Progress.Verification < 0 || row.Progress.Page < 0 || len(row.Progress.Cursor) > 4096 {
		return eventstore.ErrInvalidRecord
	}
	if row.Outcome != "unattempted" && row.Outcome != "rejected" && row.Outcome != "uncertain" && row.Outcome != "committed" {
		return eventstore.ErrInvalidRecord
	}
	if row.Status == "completed" && row.Outcome != "committed" {
		return eventstore.ErrInvalidRecord
	}
	if row.Status != "pending" && row.Status != "retrying" && row.Status != "completed" {
		return eventstore.ErrInvalidRecord
	}
	tag, err := s.db.Exec(ctx, `UPDATE github_outbound SET status=$4,next_attempt_at=$5,lease_id='',outcome=$7,phase=$8,page_cursor=$9,target_id=$10,fallback_id=$11,verification=$12,page_sequence=$13,source_updated_at=$14,attempts=$15 WHERE organization_id=$1 AND id=$2 AND lease_id=$3 AND status='processing' AND next_attempt_at>$6`, s.organization, row.ID, row.LeaseID, row.Status, row.NextAttemptAt, now, row.Outcome, row.Progress.Stage, row.Progress.Cursor, row.Progress.TargetID, row.Progress.FallbackID, row.Progress.Verification, row.Progress.Page, row.Progress.SourceUpdatedAt, row.Attempts)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	if len(row.Observations) > 0 {
		kinds, keys, sequences, ready := make([]string, 0, len(row.Observations)), make([]string, 0, len(row.Observations)), make([]int64, 0, len(row.Observations)), make([]bool, 0, len(row.Observations))
		for _, observation := range row.Observations {
			if observation.Kind == "" || observation.Key == "" || len(observation.Key) > 512 {
				return eventstore.ErrInvalidRecord
			}
			kinds = append(kinds, observation.Kind)
			keys = append(keys, observation.Key)
			sequences = append(sequences, observation.Sequence)
			ready = append(ready, observation.Ready)
		}
		_, err = s.db.Exec(ctx, `INSERT INTO github_outbound_observations(organization_id,outbound_id,verification,kind,source_key,sequence,ready)
SELECT $1,$2,$3,kind,source_key,sequence,ready FROM (SELECT DISTINCT ON(kind,source_key) kind,source_key,sequence,ready FROM unnest($4::text[],$5::text[],$6::bigint[],$7::boolean[]) AS page(kind,source_key,sequence,ready) ORDER BY kind,source_key,sequence DESC,ready) latest
ON CONFLICT(organization_id,outbound_id,kind,source_key) DO UPDATE SET verification=EXCLUDED.verification,sequence=EXCLUDED.sequence,ready=EXCLUDED.ready
WHERE github_outbound_observations.verification<EXCLUDED.verification OR (github_outbound_observations.verification=EXCLUDED.verification AND github_outbound_observations.sequence<=EXCLUDED.sequence)`, s.organization, row.ID, row.Progress.Verification, kinds, keys, sequences, ready)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
