-- name: ListGitHubImportRecords :many
SELECT * FROM github_import_records WHERE organization_id=$1 AND project_id=$2 AND import_id=$3 AND sequence>$4 ORDER BY sequence LIMIT $5;

-- name: ReadGitHubImportEditSource :one
SELECT * FROM github_import_records WHERE organization_id=$1 AND project_id=$2 AND import_id=$3 AND sequence>$4 AND kind IN ('issue','comment') ORDER BY sequence LIMIT 1;

-- name: CreateGitHubImportRecord :exec
INSERT INTO github_import_records (organization_id,project_id,import_id,source_key,kind,current_dependency,dependency_id,provenance_external_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);
