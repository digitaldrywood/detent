-- name: GetComment :one
SELECT * FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND id=sqlc.arg(id);

-- name: CreateComment :one
WITH published_source AS MATERIALIZED (
 SELECT v.revision AS source_revision,v.source_scheduled,v.source_observed_at FROM collaboration_versions v
 WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND v.work_item_id=sqlc.arg(work_item_id) AND v.record_id=sqlc.arg(id) AND v.revision=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) AND v.source_known AND sqlc.arg(source_revision)::bigint<>COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1)
)
INSERT INTO native_comments (
    record_id,
    created_at,
    updated_at,
    revision,
    organization_id,
    id,
    project_id,
    work_item_id,
    sequence,
    body_object_key,
    actor_kind,
    actor_principal_id,
    edited_by_kind,
    edited_by_principal_id,
    provenance_provider,
    provenance_external_id,
    source_key,
    deleted,
    source_revision,
    source_scheduled,
    source_observed_at
)
VALUES (
    public.allocate_core_record_id('public.native_comments_record_id_seq'::regclass, sqlc.arg(imported_id)::bigint),
    COALESCE(sqlc.narg(created_at)::timestamptz, CURRENT_TIMESTAMP),
    COALESCE(sqlc.narg(updated_at)::timestamptz, CURRENT_TIMESTAMP),
    COALESCE(NULLIF(sqlc.arg(revision)::bigint, 0), 1),
    sqlc.arg(organization_id),
    sqlc.arg(id),
    sqlc.arg(project_id),
    sqlc.arg(work_item_id),
    sqlc.arg(sequence),
    sqlc.arg(body_object_key),
    sqlc.arg(actor_kind),
    sqlc.arg(actor_principal_id),
    sqlc.arg(edited_by_kind),
    sqlc.arg(edited_by_principal_id),
    sqlc.arg(provenance_provider),
    sqlc.arg(provenance_external_id),
    sqlc.arg(source_key),
    sqlc.arg(deleted),
    CASE WHEN sqlc.arg(source_revision)::bigint=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) THEN sqlc.arg(source_revision)::bigint ELSE COALESCE((SELECT published_source.source_revision FROM published_source),0) END,
    CASE WHEN sqlc.arg(source_revision)::bigint=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) THEN sqlc.arg(source_scheduled)::boolean ELSE COALESCE((SELECT published_source.source_scheduled FROM published_source),false) END,
    CASE WHEN sqlc.arg(source_revision)::bigint=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) THEN sqlc.narg(source_observed_at)::timestamptz ELSE COALESCE((SELECT published_source.source_observed_at FROM published_source),NULL::timestamptz) END
) RETURNING *;

-- name: UpdateComment :one
WITH published_source AS MATERIALIZED (
 SELECT v.revision AS source_revision,v.source_scheduled,v.source_observed_at FROM collaboration_versions v
 WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND v.work_item_id=sqlc.arg(work_item_id) AND v.record_id=sqlc.arg(id) AND v.revision=sqlc.arg(expected_revision)::bigint+1 AND v.source_known AND sqlc.arg(source_revision)::bigint<>sqlc.arg(expected_revision)::bigint+1
)
UPDATE native_comments SET
    source_revision=CASE WHEN sqlc.arg(source_revision)::bigint=sqlc.arg(expected_revision)::bigint+1 THEN sqlc.arg(source_revision)::bigint ELSE COALESCE((SELECT published_source.source_revision FROM published_source),native_comments.source_revision) END,
    source_scheduled=CASE WHEN sqlc.arg(source_revision)::bigint=sqlc.arg(expected_revision)::bigint+1 THEN sqlc.arg(source_scheduled)::boolean ELSE COALESCE((SELECT published_source.source_scheduled FROM published_source),native_comments.source_scheduled) END,
    source_observed_at=CASE WHEN sqlc.arg(source_revision)::bigint=sqlc.arg(expected_revision)::bigint+1 THEN sqlc.narg(source_observed_at)::timestamptz ELSE COALESCE((SELECT published_source.source_observed_at FROM published_source),native_comments.source_observed_at) END,
    sequence=sqlc.arg(sequence),
    body_object_key=sqlc.arg(body_object_key),
    actor_kind=sqlc.arg(actor_kind),
    actor_principal_id=sqlc.arg(actor_principal_id),
    edited_by_kind=sqlc.arg(edited_by_kind),
    edited_by_principal_id=sqlc.arg(edited_by_principal_id),
    provenance_provider=sqlc.arg(provenance_provider),
    provenance_external_id=sqlc.arg(provenance_external_id),
    source_key=sqlc.arg(source_key),
    deleted=sqlc.arg(deleted),
    revision=native_comments.revision+1,
    updated_at=CURRENT_TIMESTAMP
WHERE native_comments.organization_id=sqlc.arg(organization_id) AND native_comments.project_id=sqlc.arg(project_id) AND native_comments.work_item_id=sqlc.arg(work_item_id) AND native_comments.id=sqlc.arg(id) AND native_comments.revision=sqlc.arg(expected_revision) RETURNING native_comments.*;

-- name: DeleteComment :execrows
DELETE FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND id=sqlc.arg(id);

-- name: ListLegacyComments :many
SELECT * FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND sequence>sqlc.arg(after)
ORDER BY sequence LIMIT sqlc.arg(page_limit);

-- name: GetCommentCursor :one
SELECT created_at, record_id FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND record_id=sqlc.arg(id);

-- name: PageComments :many
SELECT * FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND (created_at, record_id) > (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::bigint)
ORDER BY created_at, record_id LIMIT sqlc.arg(page_limit);

-- name: ScanComments :many
SELECT * FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND record_id>sqlc.arg(after_id) AND record_id<=sqlc.arg(through_id)
ORDER BY record_id LIMIT sqlc.arg(page_limit);

-- name: CommentScanThrough :one
SELECT record_id FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
ORDER BY record_id DESC LIMIT 1;
