-- name: GetComment :one
SELECT * FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND id=sqlc.arg(id);

-- name: CreateComment :one
INSERT INTO native_comments (
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
    deleted
)
VALUES (
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
    sqlc.arg(deleted)
) RETURNING *;

-- name: UpdateComment :one
UPDATE native_comments SET
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
    revision=revision+1,
    updated_at=CURRENT_TIMESTAMP
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND id=sqlc.arg(id) AND revision=sqlc.arg(expected_revision) RETURNING *;

-- name: DeleteComment :execrows
DELETE FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND id=sqlc.arg(id);

-- name: ListComments :many
SELECT * FROM native_comments
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND work_item_id=sqlc.arg(work_item_id)
  AND sequence>sqlc.arg(after)
ORDER BY sequence LIMIT sqlc.arg(page_limit);
