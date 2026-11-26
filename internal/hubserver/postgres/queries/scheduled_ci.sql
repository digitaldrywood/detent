-- name: ReadScheduledDecision :one
SELECT * FROM scheduled_ci_decisions
WHERE organization_id=$1 AND project_id=$2 AND repository=$3 AND run_id=$4 AND run_attempt=$5 AND job_id=$6;

-- name: StoreScheduledDecision :execrows
INSERT INTO scheduled_ci_decisions(organization_id,project_id,repository,head_sha,run_id,run_attempt,job_id,outcome,target,reason,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING;

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

-- name: ScheduledLandedWorkItems :many
SELECT work_item_id FROM change_requests
WHERE organization_id=$1 AND project_id=$2 AND landed_merge_sha=$3 AND landed_base_ref='develop' LIMIT 2;
