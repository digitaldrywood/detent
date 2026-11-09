-- name: DeleteArtifactProjectReadGrants :execrows
DELETE FROM artifact_grants g WHERE g.organization_id=sqlc.arg(organization_id) AND g.project_id=sqlc.arg(project_id) AND g.token_hash IN (
SELECT e.token_hash FROM artifact_grants e WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id) ORDER BY e.token_hash LIMIT sqlc.arg(batch_limit)::integer);

-- name: DeleteArtifactProjectExpiredUploadAuthorizations :execrows
DELETE FROM artifact_upload_authorizations a WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND (a.service_id,a.attempt_id,a.idempotency_key) IN (
SELECT e.service_id,e.attempt_id,e.idempotency_key FROM artifact_upload_authorizations e WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id) AND e.expires_at<=sqlc.arg(now)::timestamptz ORDER BY e.service_id,e.attempt_id,e.idempotency_key LIMIT sqlc.arg(batch_limit)::integer);

-- name: ArtifactProjectHasAuthorizations :one
SELECT EXISTS(SELECT 1 FROM artifact_grants g WHERE g.organization_id=$1 AND g.project_id=$2)
OR EXISTS(SELECT 1 FROM artifact_upload_authorizations a WHERE a.organization_id=$1 AND a.project_id=$2);

-- name: ArtifactProjectBindingIDs :many
SELECT id FROM artifact_services WHERE organization_id=$1 AND project_id=$2 ORDER BY id LIMIT sqlc.arg(batch_limit)::integer;

-- name: ArtifactProjectHasMetadata :one
SELECT EXISTS(SELECT 1 FROM artifact_grants g WHERE g.organization_id=$1 AND g.project_id=$2)
OR EXISTS(SELECT 1 FROM artifact_upload_authorizations a WHERE a.organization_id=$1 AND a.project_id=$2)
OR EXISTS(SELECT 1 FROM artifact_services s WHERE s.organization_id=$1 AND s.project_id=$2);

-- name: DeleteArtifactProjectCatalogSettings :exec
DELETE FROM artifact_catalog_traffic_settings WHERE organization_id=$1 AND project_id=$2 AND service_id=ANY(sqlc.arg(service_ids)::text[]);

-- name: DeleteArtifactProjectBindings :exec
DELETE FROM artifact_services WHERE organization_id=$1 AND project_id=$2 AND id=ANY(sqlc.arg(service_ids)::text[]);
