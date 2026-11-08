-- name: CatalogManifestIntent :one
SELECT * FROM artifact_catalog_manifest_intents WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id=$5;

-- name: CatalogManifestIntentByKey :one
SELECT * FROM artifact_catalog_manifest_intents WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND body_object_key=$5;

-- name: CatalogManifestIntents :many
SELECT * FROM artifact_catalog_manifest_intents
WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id>$5
AND (NOT sqlc.arg(pending_only)::boolean OR state IN ('pending','discarding')) ORDER BY id LIMIT $6;

-- name: CreateCatalogManifestIntent :one
INSERT INTO artifact_catalog_manifest_intents(organization_id,project_id,service_id,upload_id,id,revision,body_object_key,sha256,body_bytes,created_at,manifest_state)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING *;

-- name: AccountCatalogManifestIntent :one
UPDATE artifact_catalog_uploads SET manifest_reserved_bytes=manifest_reserved_bytes+$5 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4 RETURNING *;

-- name: VerifyCatalogManifestIntent :one
UPDATE artifact_catalog_manifest_intents SET storage_version=$6 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id=$5 AND state='pending' AND storage_version IN ('',$6) RETURNING *;

-- name: BeginCatalogManifestDiscard :one
UPDATE artifact_catalog_manifest_intents SET state='discarding' WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id=$5 AND state IN ('pending','discarding') RETURNING *;

-- name: FinishCatalogManifestDiscard :one
UPDATE artifact_catalog_manifest_intents SET state='discarded',receipt_object_key=$6 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id=$5 AND state='discarding' RETURNING *;

-- name: CommitCatalogManifestIntent :one
UPDATE artifact_catalog_manifest_intents SET state='committed' WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id=$5 AND state='pending' AND storage_version<>'' RETURNING *;

-- name: ActiveCatalogManifestIntent :one
SELECT * FROM artifact_catalog_manifest_intents WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND revision=$5 AND sha256=$6 AND state IN ('pending','discarding');
