-- name: ArtifactServiceGet :one
SELECT * FROM artifact_services WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: ArtifactServiceList :many
SELECT * FROM artifact_services WHERE organization_id=$1 AND project_id=$2 ORDER BY id LIMIT 16;

-- name: ArtifactServiceBind :execrows
INSERT INTO artifact_services (organization_id, project_id, id, origin, binding_mode, hosted_opt_in, publisher_token_id)
SELECT sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(id), sqlc.arg(origin), sqlc.arg(binding_mode), sqlc.arg(hosted_opt_in), sqlc.arg(publisher_token_id)
WHERE EXISTS (SELECT 1 FROM api_tokens t JOIN token_grants g ON g.organization_id=t.organization_id AND g.token_id=t.id
 WHERE t.organization_id=sqlc.arg(organization_id) AND t.id=sqlc.arg(publisher_token_id) AND t.revoked_at IS NULL AND g.project_id=sqlc.arg(project_id))
ON CONFLICT (organization_id, project_id, id) DO UPDATE SET origin=excluded.origin, binding_mode=excluded.binding_mode, hosted_opt_in=excluded.hosted_opt_in, publisher_token_id=excluded.publisher_token_id;
