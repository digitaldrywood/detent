-- name: GoTestReceiptAttestation :one
SELECT organization_id, project_id, id, credential_id, runner_id, created_at, expires_at
FROM go_test_receipt_attestations
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
  AND id=sqlc.arg(id) AND credential_id=sqlc.arg(credential_id);

-- name: GoTestReceipts :many
SELECT inputs, inputs_digest FROM go_test_receipts
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
  AND binary_key=sqlc.arg(binary_key) AND attestation_id <> '' AND recorded_at >= sqlc.arg(recorded_after)
ORDER BY recorded_at DESC, inputs_digest DESC LIMIT sqlc.arg(page_limit);

-- name: CreateGoTestReceiptAttestation :exec
INSERT INTO go_test_receipt_attestations (organization_id, project_id, id, credential_id, runner_id, created_at, expires_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(id), sqlc.arg(credential_id), sqlc.arg(runner_id), sqlc.arg(created_at), sqlc.arg(expires_at));

-- name: LockGoTestReceiptKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: PutGoTestReceipt :exec
INSERT INTO go_test_receipts (organization_id, project_id, binary_key, inputs_digest, inputs, package, runner_id, recorded_at, attestation_id)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(binary_key), sqlc.arg(inputs_digest), sqlc.arg(inputs), sqlc.arg(package), sqlc.arg(runner_id), sqlc.arg(recorded_at), sqlc.arg(attestation_id))
ON CONFLICT (organization_id, project_id, binary_key, inputs_digest)
DO UPDATE SET inputs=EXCLUDED.inputs, package=EXCLUDED.package, runner_id=EXCLUDED.runner_id,
              recorded_at=EXCLUDED.recorded_at, attestation_id=EXCLUDED.attestation_id;

-- name: EvictGoTestReceipts :exec
DELETE FROM go_test_receipts AS target
WHERE target.organization_id=sqlc.arg(organization_id) AND target.project_id=sqlc.arg(project_id) AND target.binary_key=sqlc.arg(binary_key)
  AND target.inputs_digest IN (
    SELECT receipt.inputs_digest FROM go_test_receipts AS receipt
    WHERE receipt.organization_id=sqlc.arg(organization_id) AND receipt.project_id=sqlc.arg(project_id) AND receipt.binary_key=sqlc.arg(binary_key)
    ORDER BY recorded_at DESC, inputs_digest DESC OFFSET sqlc.arg(keep_count)
  );

-- name: PruneGoTestReceipts :execrows
WITH expired AS (
    SELECT organization_id, project_id, binary_key, inputs_digest FROM go_test_receipts AS receipt
    WHERE receipt.organization_id=sqlc.arg(organization_id) AND receipt.recorded_at < sqlc.arg(receipt_cutoff)
    ORDER BY receipt.recorded_at, receipt.project_id, receipt.binary_key, receipt.inputs_digest
    LIMIT sqlc.arg(batch_limit) FOR UPDATE SKIP LOCKED
)
DELETE FROM go_test_receipts AS receipt USING expired
WHERE receipt.organization_id=expired.organization_id AND receipt.project_id=expired.project_id
  AND receipt.binary_key=expired.binary_key AND receipt.inputs_digest=expired.inputs_digest;

-- name: PruneGoTestReceiptAttestations :execrows
WITH expired AS (
    SELECT organization_id, id FROM go_test_receipt_attestations AS attestation
    WHERE attestation.organization_id=sqlc.arg(organization_id) AND attestation.expires_at <= sqlc.arg(attestation_cutoff)
    ORDER BY attestation.expires_at, attestation.id LIMIT sqlc.arg(batch_limit) FOR UPDATE SKIP LOCKED
)
DELETE FROM go_test_receipt_attestations AS attestation USING expired
WHERE attestation.organization_id=expired.organization_id AND attestation.id=expired.id;
