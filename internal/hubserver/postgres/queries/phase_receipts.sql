-- name: InsertNativePhaseReceipt :one
INSERT INTO native_phase_receipts (
    organization_id, project_id, issue_id, idempotency_key, lease_id, fencing_token,
    attempt_id, phase, branch, head_sha, tree_sha, policy_id, input_digest
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: NativePhaseReceiptByKey :one
SELECT * FROM native_phase_receipts
WHERE organization_id = $1 AND project_id = $2 AND issue_id = $3
  AND fencing_token = $4 AND idempotency_key = $5;

-- name: RecentNativePhaseReceipts :many
SELECT * FROM native_phase_receipts
WHERE organization_id = $1 AND project_id = $2 AND issue_id = $3
ORDER BY position DESC LIMIT 64;

-- name: NativePhaseContinuation :many
WITH last_pushed AS (
    SELECT source.* FROM native_phase_receipts source
    WHERE source.organization_id = $1 AND source.project_id = $2 AND source.issue_id = $3
    ORDER BY source.position DESC LIMIT 1
), required AS (
    SELECT p.phase, p.ordinal, (sqlc.arg(input_digests)::text[])[p.ordinal] AS input_digest
    FROM unnest(sqlc.arg(phases)::text[]) WITH ORDINALITY AS p(phase, ordinal)
)
SELECT 0::bigint AS ordinal, last_pushed.* FROM last_pushed
UNION ALL
SELECT required.ordinal, matching.*
FROM last_pushed CROSS JOIN required
CROSS JOIN LATERAL (
    SELECT receipt.* FROM native_phase_receipts receipt
    WHERE receipt.organization_id = last_pushed.organization_id
      AND receipt.project_id = last_pushed.project_id AND receipt.issue_id = last_pushed.issue_id
      AND receipt.branch = last_pushed.branch AND receipt.head_sha = last_pushed.head_sha AND receipt.tree_sha = last_pushed.tree_sha
      AND receipt.policy_id = sqlc.arg(policy_id) AND receipt.phase = required.phase AND receipt.input_digest = required.input_digest
    ORDER BY receipt.position DESC LIMIT 1
) matching
ORDER BY ordinal;
