-- name: ArtifactUploadAuthorizationPruneAttempt :exec
DELETE FROM artifact_upload_authorizations a WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND (a.service_id, a.idempotency_key) IN (
SELECT e.service_id, e.idempotency_key FROM artifact_upload_authorizations e WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id) AND e.attempt_id=sqlc.arg(attempt_id) AND e.expires_at<=sqlc.arg(now)::timestamptz
ORDER BY e.expires_at LIMIT sqlc.arg(batch_limit)::integer);

-- name: ArtifactUploadAuthorizationPruneExpired :execrows
DELETE FROM artifact_upload_authorizations a WHERE a.organization_id=sqlc.arg(organization_id) AND (a.project_id, a.service_id, a.idempotency_key) IN (
SELECT e.project_id, e.service_id, e.idempotency_key FROM artifact_upload_authorizations e WHERE e.organization_id=sqlc.arg(organization_id) AND e.expires_at<=sqlc.arg(now)::timestamptz
ORDER BY e.expires_at LIMIT sqlc.arg(batch_limit)::integer);

-- name: ArtifactUploadAuthorizationRefresh :execrows
UPDATE artifact_upload_authorizations SET expires_at=GREATEST(expires_at, sqlc.arg(expires_at)::timestamptz)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND service_id=sqlc.arg(service_id) AND idempotency_key=sqlc.arg(idempotency_key)
AND work_item_id=sqlc.arg(work_item_id) AND run_id=sqlc.arg(run_id) AND attempt_id=sqlc.arg(attempt_id) AND kind=sqlc.arg(kind) AND bytes=sqlc.arg(bytes);

-- name: ArtifactUploadAuthorizationLiveCount :one
SELECT count(*) FROM (SELECT 1 FROM artifact_upload_authorizations
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND attempt_id=sqlc.arg(attempt_id) AND expires_at>sqlc.arg(now)::timestamptz
LIMIT sqlc.arg(page_limit)::integer) live;

-- name: ArtifactUploadAuthorizationInsert :execrows
INSERT INTO artifact_upload_authorizations (organization_id,project_id,service_id,idempotency_key,work_item_id,run_id,attempt_id,kind,bytes,created_at,expires_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(service_id),sqlc.arg(idempotency_key),sqlc.arg(work_item_id),sqlc.arg(run_id),sqlc.arg(attempt_id),sqlc.arg(kind),sqlc.arg(bytes),sqlc.arg(created_at),sqlc.arg(expires_at))
ON CONFLICT (organization_id,project_id,service_id,idempotency_key) DO NOTHING;

-- name: ArtifactUploadAuthorizationConsume :execrows
DELETE FROM artifact_upload_authorizations a
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.service_id=sqlc.arg(service_id) AND a.idempotency_key=(
    SELECT u.idempotency_key FROM artifact_upload_authorizations u
    WHERE u.organization_id=sqlc.arg(organization_id) AND u.project_id=sqlc.arg(project_id) AND u.service_id=sqlc.arg(service_id) AND u.attempt_id=sqlc.arg(attempt_id) AND u.kind=sqlc.arg(kind)
    AND u.expires_at>sqlc.arg(now)::timestamptz AND u.work_item_id=sqlc.arg(work_item_id) AND u.run_id=sqlc.arg(run_id) AND u.bytes>=sqlc.arg(bytes)
    ORDER BY u.expires_at, u.idempotency_key LIMIT 1 FOR UPDATE
);

-- name: ArtifactReferencePublished :one
SELECT EXISTS (
    SELECT 1 FROM artifact_references
    WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND artifact_id=sqlc.arg(artifact_id)
    AND service_id=sqlc.arg(service_id) AND work_item_id=sqlc.arg(work_item_id) AND run_id=sqlc.arg(run_id) AND attempt_id=sqlc.arg(attempt_id) AND kind=sqlc.arg(kind)
);
