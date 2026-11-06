-- name: RecordGetReleaseDelivery :one
SELECT * FROM release_deliveries
WHERE organization_id = $1 AND project_id = $2 AND work_item_id = $3;

-- name: PutRuntimeReleaseDelivery :execrows
INSERT INTO release_deliveries (organization_id,project_id,work_item_id,source_issue_id,source_change_id,source_merge_sha,repository_id,repository,pr_number,head_ref,head_sha,base_ref,base_sha,frozen_sha,change_id,version_id,principal_id,actor_kind,via_key,associated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (organization_id,work_item_id) DO UPDATE SET
head_sha=excluded.head_sha,base_sha=excluded.base_sha,version_id=excluded.version_id,principal_id=excluded.principal_id,actor_kind=excluded.actor_kind,via_key=excluded.via_key,associated_at=excluded.associated_at
WHERE release_deliveries.project_id=excluded.project_id
AND release_deliveries.source_issue_id=excluded.source_issue_id
AND release_deliveries.source_change_id=excluded.source_change_id
AND release_deliveries.source_merge_sha=excluded.source_merge_sha
AND release_deliveries.repository_id IS NOT DISTINCT FROM excluded.repository_id
AND release_deliveries.repository=excluded.repository
AND release_deliveries.pr_number=excluded.pr_number
AND release_deliveries.head_ref=excluded.head_ref
AND release_deliveries.base_ref=excluded.base_ref
AND release_deliveries.frozen_sha=excluded.frozen_sha
AND release_deliveries.change_id=excluded.change_id
AND ((release_deliveries.head_sha=excluded.head_sha AND release_deliveries.base_sha=excluded.base_sha AND release_deliveries.version_id IS NOT DISTINCT FROM excluded.version_id)
OR (release_deliveries.head_sha<>excluded.head_sha AND release_deliveries.base_sha<>excluded.base_sha AND excluded.version_id IS NULL));
