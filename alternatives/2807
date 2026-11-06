-- name: GetProject :one
SELECT * FROM projects
WHERE organization_id=sqlc.arg(organization_id)
  AND id=sqlc.arg(id);

-- name: CreateProject :one
INSERT INTO projects (
    organization_id,
    id,
    name,
    slug,
    profile,
    require_dependencies,
    scheduling_rank,
    content_settings_revision,
    workflow_source,
    workflow_source_revision,
    workflow_object_key,
    states_object_key,
    model_selection_object_key
)
VALUES (
    sqlc.arg(organization_id),
    sqlc.arg(id),
    sqlc.arg(name),
    sqlc.arg(slug),
    sqlc.arg(profile),
    sqlc.arg(require_dependencies),
    sqlc.arg(scheduling_rank),
    sqlc.arg(content_settings_revision),
    sqlc.arg(workflow_source),
    sqlc.arg(workflow_source_revision),
    sqlc.arg(workflow_object_key),
    sqlc.arg(states_object_key),
    sqlc.arg(model_selection_object_key)
) RETURNING *;

-- name: UpdateProject :one
UPDATE projects SET
    name=sqlc.arg(name),
    slug=sqlc.arg(slug),
    profile=sqlc.arg(profile),
    require_dependencies=sqlc.arg(require_dependencies),
    scheduling_rank=sqlc.arg(scheduling_rank),
    workflow_source=sqlc.arg(workflow_source),
    workflow_source_revision=sqlc.arg(workflow_source_revision),
    workflow_object_key=sqlc.arg(workflow_object_key),
    states_object_key=sqlc.arg(states_object_key),
    model_selection_object_key=sqlc.arg(model_selection_object_key),
    deleted_at=sqlc.arg(deleted_at),
    content_settings_revision=content_settings_revision+1
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND content_settings_revision=sqlc.arg(expected_revision) RETURNING *;

-- name: DeleteProject :execrows
DELETE FROM projects
WHERE organization_id=sqlc.arg(organization_id)
  AND id=sqlc.arg(id);

-- name: ListProjects :many
SELECT * FROM projects
WHERE organization_id=sqlc.arg(organization_id)
  AND id>sqlc.arg(after)
ORDER BY id LIMIT sqlc.arg(page_limit);
