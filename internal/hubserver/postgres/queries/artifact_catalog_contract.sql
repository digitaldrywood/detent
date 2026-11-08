-- name: CatalogUsage :one
SELECT * FROM artifact_catalog_usage WHERE organization_id=$1;

-- name: EnsureCatalogUsage :exec
INSERT INTO artifact_catalog_usage(organization_id) VALUES($1) ON CONFLICT DO NOTHING;

-- name: LockCatalogUsage :one
SELECT * FROM artifact_catalog_usage WHERE organization_id=$1 FOR UPDATE;

-- name: AdjustCatalogUsage :exec
UPDATE artifact_catalog_usage SET reserved_bytes=reserved_bytes+$2,retained_bytes=retained_bytes+$3 WHERE organization_id=$1;

-- name: CatalogUpload :one
SELECT * FROM artifact_catalog_uploads WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4;

-- name: LockCatalogUpload :one
SELECT * FROM artifact_catalog_uploads WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4 FOR UPDATE;

-- name: CatalogUploadByKey :one
SELECT * FROM artifact_catalog_uploads WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND idempotency_key=$4;

-- name: CreateCatalogUpload :one
INSERT INTO artifact_catalog_uploads(organization_id,project_id,service_id,id,idempotency_key,request_hash,work_item_id,run_id,attempt_id,version_id,kind,lease_id,fencing_token,requested_bytes,part_limit,reserved_bytes,created_at,upload_deadline,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$14,$16,$17,$18) RETURNING *;

-- name: CatalogObjects :many
SELECT * FROM artifact_catalog_objects WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND sequence>=$5 ORDER BY sequence LIMIT $6;

-- name: CatalogObject :one
SELECT * FROM artifact_catalog_objects WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND sequence=$5;

-- name: CreateCatalogObject :one
INSERT INTO artifact_catalog_objects(organization_id,project_id,service_id,upload_id,sequence,id,media_type,size,sha256,path,side,byte_offset) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING *;

-- name: VerifyCatalogObject :one
UPDATE artifact_catalog_objects SET storage_version=$6,verified_at=$7 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND sequence=$5 AND verified_at IS NULL AND discarded_at IS NULL RETURNING *;

-- name: AccountCatalogObject :one
UPDATE artifact_catalog_uploads SET reserved_bytes=reserved_bytes-$5,retained_bytes=retained_bytes+$5 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4 AND state='uploading' AND reserved_bytes>=$5 RETURNING *;

-- name: CatalogManifest :one
SELECT * FROM artifact_catalog_manifests WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND revision=$5;

-- name: CreateCatalogManifest :one
INSERT INTO artifact_catalog_manifests(organization_id,project_id,service_id,upload_id,revision,id,state,sha256,body_object_key,body_bytes,total_bytes,objects,retention_policy_id,capture_base,capture_head,capture_merge_base,capture_context_lines,capture_file_context,capture_working_tree,created_at,expires_at,observed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22) RETURNING *;

-- name: CatalogObjectTotals :one
SELECT count(*) FILTER(WHERE verified_at IS NOT NULL)::bigint AS verified_count,COALESCE(sum(size) FILTER(WHERE verified_at IS NOT NULL),0)::bigint AS verified_bytes,count(*) FILTER(WHERE verified_at IS NULL AND discarded_at IS NULL)::bigint AS pending_count FROM artifact_catalog_objects WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4;

-- name: SealCatalogUpload :one
UPDATE artifact_catalog_uploads SET manifest_revision=sqlc.arg(revision),state=CASE WHEN sqlc.arg(state)::text='partial' THEN 'uploading' ELSE sqlc.arg(state) END,retained_bytes=retained_bytes+sqlc.arg(body_bytes),reserved_bytes=CASE WHEN sqlc.arg(state)::text='partial' THEN reserved_bytes-sqlc.arg(body_bytes) ELSE 0 END
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND service_id=sqlc.arg(service_id) AND id=sqlc.arg(id) AND state='uploading' AND manifest_revision=sqlc.arg(revision)-1 AND reserved_bytes>=sqlc.arg(body_bytes) RETURNING *;

-- name: EnqueueCatalogPublication :exec
INSERT INTO artifact_catalog_publications(organization_id,project_id,service_id,manifest_id,updated_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING;

-- name: PendingCatalogPublications :many
SELECT p.* FROM artifact_catalog_publications p JOIN artifact_catalog_manifests m ON m.organization_id=p.organization_id AND m.project_id=p.project_id AND m.service_id=p.service_id AND m.id=p.manifest_id
JOIN artifact_catalog_uploads u ON u.organization_id=m.organization_id AND u.project_id=m.project_id AND u.service_id=m.service_id AND u.id=m.upload_id
WHERE p.organization_id=$1 AND p.outcome<>'committed' AND u.state<>'deleted' ORDER BY p.updated_at,p.project_id,p.service_id,p.manifest_id LIMIT $2;

-- name: CatalogPublication :one
SELECT * FROM artifact_catalog_publications WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND manifest_id=$4;

-- name: RecordCatalogPublication :one
UPDATE artifact_catalog_publications SET outcome=$5,external_receipt=$6,updated_at=$7 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND manifest_id=$4 AND outcome<>'committed' RETURNING *;

-- name: CatalogRetentionCandidates :many
WITH due AS (
 (SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND state<>'deleted' AND expires_at<=sqlc.arg(now)::timestamptz ORDER BY expires_at,project_id,service_id,id LIMIT sqlc.arg(page_limit))
 UNION
 (SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND state='uploading' AND upload_deadline<=sqlc.arg(now)::timestamptz ORDER BY upload_deadline,project_id,service_id,id LIMIT sqlc.arg(page_limit))
 UNION
 (SELECT u.* FROM artifact_catalog_uploads u WHERE u.organization_id=sqlc.arg(organization_id) AND state='deletion_pending' ORDER BY expires_at,project_id,service_id,id LIMIT sqlc.arg(page_limit))
)
SELECT * FROM due ORDER BY expires_at,project_id,service_id,id LIMIT sqlc.arg(page_limit);

-- name: BeginCatalogDeletion :one
UPDATE artifact_catalog_uploads SET state='deletion_pending' WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4 AND state<>'deleted' RETURNING *;

-- name: FinishCatalogDeletion :one
UPDATE artifact_catalog_uploads SET state='deleted',reserved_bytes=0,retained_bytes=0,deleted_at=$5,deletion_receipt_object_key=$6 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4 AND state='deletion_pending' RETURNING *;

-- name: PruneCatalogUpload :execrows
DELETE FROM artifact_catalog_uploads WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4 AND state='deleted' AND deleted_at<=$5;

-- name: CatalogTrafficSettings :one
SELECT * FROM artifact_catalog_traffic_settings WHERE organization_id=$1 AND project_id=$2 AND service_id=$3;

-- name: PutCatalogTrafficSettings :one
INSERT INTO artifact_catalog_traffic_settings(organization_id,project_id,service_id,window_seconds,retention_seconds) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,project_id,service_id) DO UPDATE SET window_seconds=EXCLUDED.window_seconds,retention_seconds=EXCLUDED.retention_seconds RETURNING *;

-- name: CatalogTraffic :many
SELECT * FROM artifact_catalog_traffic WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND minute>=$4 AND minute<$5 ORDER BY minute LIMIT $6;

-- name: CatalogTrafficReceipt :one
SELECT * FROM artifact_catalog_traffic_receipts WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND id=$4;

-- name: CreateCatalogTrafficReceipt :exec
INSERT INTO artifact_catalog_traffic_receipts(organization_id,project_id,service_id,id,minute,upload_bytes,download_bytes,requests) VALUES($1,$2,$3,$4,$5,$6,$7,$8);

-- name: AddCatalogTraffic :exec
INSERT INTO artifact_catalog_traffic(organization_id,project_id,service_id,minute,upload_bytes,download_bytes,requests) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,project_id,service_id,minute) DO UPDATE SET upload_bytes=artifact_catalog_traffic.upload_bytes+EXCLUDED.upload_bytes,download_bytes=artifact_catalog_traffic.download_bytes+EXCLUDED.download_bytes,requests=artifact_catalog_traffic.requests+EXCLUDED.requests;

-- name: PruneCatalogTraffic :execrows
DELETE FROM artifact_catalog_traffic d WHERE (d.organization_id,d.project_id,d.service_id,d.minute) IN (SELECT s.organization_id,s.project_id,s.service_id,s.minute FROM artifact_catalog_traffic s WHERE s.organization_id=$1 AND s.minute<$2 ORDER BY s.minute,s.project_id,s.service_id LIMIT $3);

-- name: PruneCatalogTrafficReceipts :execrows
DELETE FROM artifact_catalog_traffic_receipts d WHERE (d.organization_id,d.project_id,d.service_id,d.id) IN (SELECT s.organization_id,s.project_id,s.service_id,s.id FROM artifact_catalog_traffic_receipts s WHERE s.organization_id=$1 AND s.minute<$2 ORDER BY s.minute,s.project_id,s.service_id,s.id LIMIT $3);

-- name: DiscardCatalogObject :one
UPDATE artifact_catalog_objects SET discarded_at=$6,discard_receipt_object_key=$7 WHERE organization_id=$1 AND project_id=$2 AND service_id=$3 AND upload_id=$4 AND sequence=$5 AND verified_at IS NULL AND discarded_at IS NULL RETURNING *;
