-- name: SetCatalogObjectWrite :one
UPDATE artifact_catalog_objects SET write_state=$6,write_id=$7,storage_version=$8
WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND sequence=$5 RETURNING *;

-- name: SetCatalogManifestWrite :one
UPDATE artifact_catalog_manifest_intents SET write_state=$6,write_id=$7,storage_version=$8
WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND id=$5 RETURNING *;

-- name: ReplaceRejectedCatalogObject :one
UPDATE artifact_catalog_objects SET id=$6,media_type=$7,size=$8,sha256=$9,path=$10,side=$11,byte_offset=$12,
 write_state='ready',write_id='',storage_version='',discarded_at=NULL,discard_receipt_object_key=NULL
WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND sequence=$5 AND write_state='rejected' AND verified_at IS NULL RETURNING *;

-- name: CatalogUnsettledWrites :one
SELECT (EXISTS(SELECT 1 FROM artifact_catalog_objects o WHERE o.organization_id=sqlc.arg(organization_id) AND o.project_id=sqlc.arg(project_id) AND o.service_id=sqlc.arg(service_id) AND o.upload_id=sqlc.arg(upload_id) AND o.write_state NOT IN ('written','rejected'))
 OR EXISTS(SELECT 1 FROM artifact_catalog_manifest_intents i WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.service_id=sqlc.arg(service_id) AND i.upload_id=sqlc.arg(upload_id) AND (i.write_state NOT IN ('written','rejected') OR i.state IN ('pending','discarding'))))::boolean AS unsettled;
