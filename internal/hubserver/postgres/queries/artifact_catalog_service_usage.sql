-- name: CatalogServiceUsage :one
SELECT * FROM artifact_catalog_service_usage WHERE organization_id=$1 AND service_id=$2;

-- name: CatalogServiceUsageActive :one
SELECT EXISTS(SELECT 1 FROM artifact_catalog_uploads WHERE organization_id=$1 AND ((sqlc.arg(service_id)::text='' AND binding_mode='hosted') OR service_id=sqlc.arg(service_id)) AND (reserved_bytes>0 OR retained_bytes>0));

-- name: EnsureCatalogServiceUsage :exec
INSERT INTO artifact_catalog_service_usage(organization_id,service_id) VALUES($1,$2) ON CONFLICT DO NOTHING;

-- name: AdjustCatalogServiceUsage :execrows
UPDATE artifact_catalog_service_usage SET reserved_bytes=reserved_bytes+$3,retained_bytes=retained_bytes+$4 WHERE organization_id=$1 AND service_id=$2;

-- name: PruneEmptyCatalogServiceUsage :exec
DELETE FROM artifact_catalog_service_usage q WHERE q.organization_id=$1 AND q.service_id=$2 AND q.reserved_bytes=0 AND q.retained_bytes=0
AND NOT EXISTS(SELECT 1 FROM artifact_catalog_uploads u WHERE u.organization_id=q.organization_id AND u.service_id=q.service_id);

-- name: CatalogOrganizationUsageActive :one
SELECT EXISTS(SELECT 1 FROM artifact_catalog_uploads WHERE organization_id=$1 AND (reserved_bytes>0 OR retained_bytes>0));
