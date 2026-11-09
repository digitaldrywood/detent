-- name: DeleteArtifactProjectReadGrants :exec
DELETE FROM artifact_grants WHERE organization_id=$1 AND project_id=$2;

-- name: DeleteArtifactProjectUploadAuthorizations :exec
DELETE FROM artifact_upload_authorizations WHERE organization_id=$1 AND project_id=$2;

-- name: DeleteArtifactProjectCatalogSettings :exec
DELETE FROM artifact_catalog_traffic_settings WHERE organization_id=$1 AND project_id=$2;

-- name: DeleteArtifactProjectBindings :exec
DELETE FROM artifact_services WHERE organization_id=$1 AND project_id=$2;
