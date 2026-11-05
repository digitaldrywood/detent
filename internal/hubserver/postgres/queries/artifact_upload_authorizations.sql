-- name: ArtifactUploadAuthorizationInsert :execrows
INSERT INTO artifact_upload_authorizations (organization_id,project_id,service_id,idempotency_key,work_item_id,run_id,attempt_id,kind,bytes,created_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(service_id),sqlc.arg(idempotency_key),sqlc.arg(work_item_id),sqlc.arg(run_id),sqlc.arg(attempt_id),sqlc.arg(kind),sqlc.arg(bytes),sqlc.arg(created_at))
ON CONFLICT (organization_id,project_id,service_id,idempotency_key) DO NOTHING;

-- name: ArtifactUploadAuthorizationMatches :one
SELECT EXISTS (
    SELECT 1 FROM artifact_upload_authorizations
    WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND service_id=sqlc.arg(service_id) AND idempotency_key=sqlc.arg(idempotency_key)
    AND work_item_id=sqlc.arg(work_item_id) AND run_id=sqlc.arg(run_id) AND attempt_id=sqlc.arg(attempt_id) AND kind=sqlc.arg(kind) AND bytes=sqlc.arg(bytes)
);

-- name: ArtifactUploadAuthorizationBound :one
SELECT EXISTS (
    SELECT 1 FROM artifact_upload_authorizations
    WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND artifact_id=sqlc.arg(artifact_id)
    AND service_id=sqlc.arg(service_id) AND work_item_id=sqlc.arg(work_item_id) AND run_id=sqlc.arg(run_id) AND attempt_id=sqlc.arg(attempt_id) AND kind=sqlc.arg(kind) AND bytes>=sqlc.arg(bytes)
);

-- name: ArtifactUploadAuthorizationBind :execrows
UPDATE artifact_upload_authorizations a SET artifact_id=sqlc.arg(artifact_id)
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.service_id=sqlc.arg(service_id) AND a.idempotency_key=(
    SELECT u.idempotency_key FROM artifact_upload_authorizations u
    WHERE u.organization_id=sqlc.arg(organization_id) AND u.project_id=sqlc.arg(project_id) AND u.service_id=sqlc.arg(service_id) AND u.attempt_id=sqlc.arg(attempt_id) AND u.kind=sqlc.arg(kind)
    AND u.artifact_id IS NULL AND u.work_item_id=sqlc.arg(work_item_id) AND u.run_id=sqlc.arg(run_id) AND u.bytes>=sqlc.arg(bytes)
    ORDER BY u.created_at, u.idempotency_key LIMIT 1 FOR UPDATE
)
AND NOT EXISTS (SELECT 1 FROM artifact_upload_authorizations b WHERE b.organization_id=sqlc.arg(organization_id) AND b.project_id=sqlc.arg(project_id) AND b.artifact_id=sqlc.arg(artifact_id));
