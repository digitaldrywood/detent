-- name: CatalogServiceUsage :one
SELECT COALESCE(q.reserved_bytes,0)::bigint AS reserved_bytes,COALESCE(q.retained_bytes,0)::bigint AS retained_bytes,
 (q.organization_id IS NOT NULL)::boolean AS has_counter,
 EXISTS(SELECT 1 FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND ((sqlc.arg(service_id)::text='' AND u.binding_mode='hosted') OR u.service_id=sqlc.arg(service_id)) AND (u.reserved_bytes>0 OR u.retained_bytes>0)) AS active
FROM (SELECT 1) seed LEFT JOIN artifact_catalog_service_usage q ON q.organization_id=sqlc.arg(organization_id) AND q.service_id=sqlc.arg(service_id);

-- name: EnsureCatalogServiceUsage :exec
INSERT INTO artifact_catalog_service_usage(organization_id,service_id) VALUES($1,$2) ON CONFLICT DO NOTHING;

-- name: AdjustCatalogServiceUsage :execrows
UPDATE artifact_catalog_service_usage SET reserved_bytes=reserved_bytes+$3,retained_bytes=retained_bytes+$4 WHERE organization_id=$1 AND service_id=$2;

-- name: PruneEmptyCatalogServiceUsage :exec
DELETE FROM artifact_catalog_service_usage q WHERE q.organization_id=$1 AND q.service_id=$2 AND q.reserved_bytes=0 AND q.retained_bytes=0
AND NOT EXISTS(SELECT 1 FROM artifact_catalog_uploads u WHERE u.organization_id=q.organization_id AND u.service_id=q.service_id);
