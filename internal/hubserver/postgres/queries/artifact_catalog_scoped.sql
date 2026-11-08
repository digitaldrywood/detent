-- name: CatalogServiceUploads :many
SELECT * FROM artifact_catalog_uploads WHERE organization_id=$1 AND service_id=$2 AND id=$3 ORDER BY project_id LIMIT 2;

-- name: CatalogServiceBinding :one
SELECT b.* FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=$1 AND b.id=$2 AND b.publisher_token_id=$3 ORDER BY b.project_id LIMIT 1;

-- name: ScopedCatalogUploads :many
SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND u.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR u.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=u.organization_id AND b.project_id=u.project_id AND b.id=u.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (u.project_id,u.id)>(sqlc.arg(after_project)::text,sqlc.arg(after_id)::text) ORDER BY u.project_id,u.id LIMIT sqlc.arg(page_limit);

-- name: ScopedPendingCatalogManifests :many
SELECT m.* FROM artifact_catalog_publications p JOIN artifact_catalog_manifests m ON (m.organization_id,m.project_id,m.service_id,m.id)=(p.organization_id,p.project_id,p.service_id,p.manifest_id)
JOIN artifact_catalog_uploads u ON (u.organization_id,u.project_id,u.service_id,u.id)=(m.organization_id,m.project_id,m.service_id,m.upload_id)
WHERE u.organization_id=sqlc.arg(organization_id) AND u.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR u.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=u.organization_id AND b.project_id=u.project_id AND b.id=u.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND p.outcome<>'committed' AND u.state NOT IN ('deleted','deletion_pending') AND u.expires_at>sqlc.arg(now)::timestamptz
AND (m.project_id,m.id)>(sqlc.arg(after_project)::text,sqlc.arg(after_id)::text) ORDER BY m.project_id,m.id LIMIT sqlc.arg(page_limit);

-- name: ScopedCatalogRetention :many
(SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND u.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR u.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=u.organization_id AND b.project_id=u.project_id AND b.id=u.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND sqlc.arg(category)::integer=0 AND u.state<>'deleted' AND u.expires_at<=sqlc.arg(now)::timestamptz AND (u.expires_at,u.project_id,u.id)>(sqlc.arg(after_time)::timestamptz,sqlc.arg(after_project)::text,sqlc.arg(after_id)::text) ORDER BY u.expires_at,u.project_id,u.id LIMIT sqlc.arg(page_limit))
UNION ALL
(SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND u.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR u.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=u.organization_id AND b.project_id=u.project_id AND b.id=u.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND sqlc.arg(category)::integer=1 AND u.state='uploading' AND u.upload_deadline<=sqlc.arg(now)::timestamptz AND (u.upload_deadline,u.project_id,u.id)>(sqlc.arg(after_time)::timestamptz,sqlc.arg(after_project)::text,sqlc.arg(after_id)::text) ORDER BY u.upload_deadline,u.project_id,u.id LIMIT sqlc.arg(page_limit))
UNION ALL
(SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND u.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR u.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=u.organization_id AND b.project_id=u.project_id AND b.id=u.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND sqlc.arg(category)::integer=2 AND u.state='deletion_pending' AND (u.expires_at,u.project_id,u.id)>(sqlc.arg(after_time)::timestamptz,sqlc.arg(after_project)::text,sqlc.arg(after_id)::text) ORDER BY u.expires_at,u.project_id,u.id LIMIT sqlc.arg(page_limit))
UNION ALL
(SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND u.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR u.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=u.organization_id AND b.project_id=u.project_id AND b.id=u.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND sqlc.arg(category)::integer=3 AND u.state='deleted' AND u.deleted_at<=sqlc.arg(before)::timestamptz AND EXISTS(SELECT 1 FROM artifact_services policy WHERE (policy.organization_id,policy.project_id,policy.id)=(u.organization_id,u.project_id,u.service_id) AND u.deleted_at<=sqlc.arg(now)::timestamptz-policy.catalog_deletion_seconds*interval '1 second') AND (u.deleted_at,u.project_id,u.id)>(sqlc.arg(after_time)::timestamptz,sqlc.arg(after_project)::text,sqlc.arg(after_id)::text) ORDER BY u.deleted_at,u.project_id,u.id LIMIT sqlc.arg(page_limit));

-- name: ScopedCatalogObjects :many
SELECT o.* FROM artifact_catalog_objects o WHERE o.organization_id=sqlc.arg(organization_id) AND o.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR o.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=o.organization_id AND b.project_id=o.project_id AND b.id=o.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (o.project_id,o.upload_id,o.sequence)>(sqlc.arg(after_project)::text,sqlc.arg(after_upload)::text,sqlc.arg(after_value)::integer) ORDER BY o.project_id,o.upload_id,o.sequence LIMIT sqlc.arg(page_limit);

-- name: ScopedCatalogManifests :many
SELECT m.* FROM artifact_catalog_manifests m WHERE m.organization_id=sqlc.arg(organization_id) AND m.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR m.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=m.organization_id AND b.project_id=m.project_id AND b.id=m.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (m.project_id,m.upload_id,m.revision)>(sqlc.arg(after_project)::text,sqlc.arg(after_upload)::text,sqlc.arg(after_value)::bigint) ORDER BY m.project_id,m.upload_id,m.revision LIMIT sqlc.arg(page_limit);

-- name: ScopedCatalogIntents :many
SELECT i.* FROM artifact_catalog_manifest_intents i WHERE i.organization_id=sqlc.arg(organization_id) AND i.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR i.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=i.organization_id AND b.project_id=i.project_id AND b.id=i.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (i.project_id,i.upload_id,i.id)>(sqlc.arg(after_project)::text,sqlc.arg(after_upload)::text,sqlc.arg(after_value)::text) ORDER BY i.project_id,i.upload_id,i.id LIMIT sqlc.arg(page_limit);

-- name: AddCatalogServiceTraffic :exec
INSERT INTO artifact_catalog_service_traffic(organization_id,service_id,minute,upload_bytes,download_bytes,requests) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(organization_id,service_id,minute) DO UPDATE SET upload_bytes=artifact_catalog_service_traffic.upload_bytes+EXCLUDED.upload_bytes,download_bytes=artifact_catalog_service_traffic.download_bytes+EXCLUDED.download_bytes,requests=artifact_catalog_service_traffic.requests+EXCLUDED.requests;

-- name: CatalogServiceTraffic :one
SELECT COALESCE(sum(upload_bytes+download_bytes),0)::bigint AS relay_bytes,COALESCE(sum(requests),0)::bigint AS requests FROM artifact_catalog_service_traffic WHERE organization_id=$1 AND service_id=$2 AND minute>=$3 AND minute<$4;


-- name: PruneScopedCatalogTraffic :exec
DELETE FROM artifact_catalog_traffic d WHERE (d.organization_id,d.project_id,d.service_id,d.minute) IN (SELECT s.organization_id,s.project_id,s.service_id,s.minute FROM artifact_catalog_traffic s WHERE s.organization_id=sqlc.arg(organization_id) AND s.service_id=sqlc.arg(service_id) AND s.minute<sqlc.arg(before)::timestamptz AND (sqlc.arg(project_id)::text='' OR s.project_id=sqlc.arg(project_id)) ORDER BY s.minute,s.organization_id,s.project_id,s.service_id,s.minute LIMIT sqlc.arg(page_limit));

-- name: PruneScopedCatalogTrafficReceipts :exec
DELETE FROM artifact_catalog_traffic_receipts d WHERE (d.organization_id,d.project_id,d.service_id,d.id) IN (SELECT s.organization_id,s.project_id,s.service_id,s.id FROM artifact_catalog_traffic_receipts s WHERE s.organization_id=sqlc.arg(organization_id) AND s.service_id=sqlc.arg(service_id) AND s.minute<sqlc.arg(before)::timestamptz AND (sqlc.arg(project_id)::text='' OR s.project_id=sqlc.arg(project_id)) ORDER BY s.minute,s.organization_id,s.project_id,s.service_id,s.id LIMIT sqlc.arg(page_limit));

-- name: PruneCatalogServiceTraffic :exec
DELETE FROM artifact_catalog_service_traffic d WHERE (d.organization_id,d.service_id,d.minute) IN (SELECT s.organization_id,s.service_id,s.minute FROM artifact_catalog_service_traffic s WHERE s.organization_id=sqlc.arg(organization_id) AND s.service_id=sqlc.arg(service_id) AND s.minute<sqlc.arg(before)::timestamptz ORDER BY s.minute,s.organization_id,s.service_id,s.minute LIMIT sqlc.arg(page_limit));

-- name: ScopedCatalogPublications :many
SELECT r.* FROM artifact_catalog_publications r WHERE r.organization_id=sqlc.arg(organization_id) AND r.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR r.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.id=r.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (r.project_id,r.manifest_id)>(sqlc.arg(after_0)::text,sqlc.arg(after_1)::text) ORDER BY r.project_id,r.manifest_id LIMIT sqlc.arg(page_limit);

-- name: ScopedCatalogTrafficBuckets :many
SELECT r.* FROM artifact_catalog_traffic r WHERE r.organization_id=sqlc.arg(organization_id) AND r.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR r.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.id=r.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (r.project_id,r.minute)>(sqlc.arg(after_0)::text,sqlc.arg(after_1)::timestamptz) ORDER BY r.project_id,r.minute LIMIT sqlc.arg(page_limit);

-- name: ScopedCatalogTrafficReceipts :many
SELECT r.* FROM artifact_catalog_traffic_receipts r WHERE r.organization_id=sqlc.arg(organization_id) AND r.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR r.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.id=r.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND (r.project_id,r.id)>(sqlc.arg(after_0)::text,sqlc.arg(after_1)::text) ORDER BY r.project_id,r.id LIMIT sqlc.arg(page_limit);

-- name: ScopedCatalogSettings :many
SELECT r.* FROM artifact_catalog_traffic_settings r WHERE r.organization_id=sqlc.arg(organization_id) AND r.service_id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR r.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.id=r.service_id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND r.project_id>sqlc.arg(after_0)::text ORDER BY r.project_id LIMIT sqlc.arg(page_limit);

-- name: CatalogExecutionAuthority :one
SELECT true AS authorized FROM native_attempts a JOIN leases l ON (l.organization_id,l.lease_id,l.fencing_token)=(a.organization_id,a.lease_id,a.fencing_token)
JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
JOIN artifact_upload_authorizations g ON g.organization_id=i.organization_id AND g.project_id=i.project_id AND g.attempt_id=a.id
WHERE a.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=sqlc.arg(work_item_id)
AND a.id=sqlc.arg(attempt_id) AND a.run_id=sqlc.arg(run_id) AND a.lease_id=sqlc.arg(lease_id) AND a.fencing_token=sqlc.arg(fencing_token)
AND a.status='running' AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz AND NOT i.archived
AND g.service_id=sqlc.arg(service_id) AND g.idempotency_key=sqlc.arg(idempotency_key) AND g.work_item_id=i.native_id AND g.run_id=a.run_id
AND g.kind=sqlc.arg(kind) AND g.bytes=sqlc.arg(bytes) AND g.expires_at>sqlc.arg(now)::timestamptz
FOR SHARE OF a,l,i,g;

-- name: SetCatalogPolicy :one
UPDATE artifact_services SET catalog_policy_id=sqlc.arg(policy_id),catalog_retained_limit=sqlc.arg(retained_limit),catalog_reserved_limit=sqlc.arg(reserved_limit),catalog_artifact_limit=sqlc.arg(artifact_limit),catalog_upload_limit=sqlc.arg(upload_limit),catalog_retention_seconds=sqlc.arg(retention_seconds),catalog_abandoned_seconds=sqlc.arg(abandoned_seconds),catalog_deletion_seconds=sqlc.arg(deletion_seconds),catalog_backup_seconds=sqlc.arg(backup_seconds)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(service_id) RETURNING *;

-- name: CatalogPolicy :one
SELECT * FROM artifact_services WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: LockCatalogPolicy :one
SELECT * FROM artifact_services WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR SHARE;

-- name: ScopedCatalogPolicies :many
SELECT r.* FROM artifact_services r WHERE r.organization_id=sqlc.arg(organization_id) AND r.id=sqlc.arg(service_id) AND (sqlc.arg(project_id)::text='' OR r.project_id=sqlc.arg(project_id)) AND (sqlc.arg(publisher_id)::text='' OR EXISTS(SELECT 1 FROM artifact_services b JOIN token_grants g ON (g.organization_id,g.project_id,g.token_id)=(b.organization_id,b.project_id,b.publisher_token_id) WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.id=r.id AND b.publisher_token_id=sqlc.arg(publisher_id))) AND r.project_id>sqlc.arg(after_0)::text ORDER BY r.project_id LIMIT sqlc.arg(page_limit);
