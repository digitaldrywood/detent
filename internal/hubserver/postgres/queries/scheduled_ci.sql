-- name: ReadScheduledDecision :one
SELECT d.head_sha, d.outcome, d.target, d.reason, EXISTS (
    SELECT 1 FROM scheduled_ci_classification_claims c
    WHERE c.organization_id=d.organization_id AND c.project_id=d.project_id
      AND c.job_key=sqlc.arg(job_key) AND c.expires_at>sqlc.arg(as_of)
) AS pending
FROM scheduled_ci_decisions d
WHERE d.organization_id=sqlc.arg(organization_id) AND d.project_id=sqlc.arg(project_id)
  AND d.repository=sqlc.arg(repository) AND d.run_id=sqlc.arg(run_id)
  AND d.run_attempt=sqlc.arg(run_attempt) AND d.job_id=sqlc.arg(job_id);

-- name: StoreScheduledDecision :execrows
INSERT INTO scheduled_ci_decisions(organization_id,project_id,repository,head_sha,run_id,run_attempt,job_id,outcome,target,reason,created_at,collaboration_bytes)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING;

-- name: UpdateScheduledDecision :one
WITH previous AS (
    SELECT s.organization_id,s.project_id,s.repository,s.run_id,s.run_attempt,s.job_id,s.collaboration_bytes
    FROM scheduled_ci_decisions s
    WHERE s.organization_id=sqlc.arg(organization_id) AND s.project_id=sqlc.arg(project_id) AND s.repository=sqlc.arg(repository)
      AND s.run_id=sqlc.arg(run_id) AND s.run_attempt=sqlc.arg(run_attempt) AND s.job_id=sqlc.arg(job_id) AND s.head_sha=sqlc.arg(head_sha)
    FOR UPDATE
)
UPDATE scheduled_ci_decisions d
SET outcome=sqlc.arg(outcome),target=sqlc.arg(target),reason=sqlc.arg(reason),collaboration_bytes=sqlc.arg(collaboration_bytes)
FROM previous p
WHERE (d.organization_id,d.project_id,d.repository,d.run_id,d.run_attempt,d.job_id)
    = (p.organization_id,p.project_id,p.repository,p.run_id,p.run_attempt,p.job_id)
RETURNING p.collaboration_bytes;

-- name: ClaimScheduledClassification :execrows
INSERT INTO scheduled_ci_classification_claims(organization_id,project_id,job_key,receipt_id,expires_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (organization_id,project_id,job_key) DO UPDATE
SET receipt_id=EXCLUDED.receipt_id, expires_at=EXCLUDED.expires_at
WHERE scheduled_ci_classification_claims.expires_at <= sqlc.arg(as_of);

-- name: ReleaseScheduledClassification :execrows
DELETE FROM scheduled_ci_classification_claims
WHERE organization_id=$1 AND project_id=$2 AND job_key=$3 AND receipt_id=$4
 AND (sqlc.narg(valid_at)::timestamptz IS NULL OR expires_at > sqlc.narg(valid_at));

-- name: LockScheduledClassification :one
SELECT true FROM scheduled_ci_classification_claims
WHERE organization_id=$1 AND project_id=$2 AND job_key=$3 AND receipt_id=$4
  AND expires_at > sqlc.arg(as_of)
FOR UPDATE;

-- name: ScheduledLandedWorkItems :many
SELECT work_item_id FROM change_requests
WHERE organization_id=$1 AND project_id=$2 AND landed_merge_sha=$3 AND landed_base_ref='develop' LIMIT 2;
