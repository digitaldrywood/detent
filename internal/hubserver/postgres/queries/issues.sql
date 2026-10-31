-- name: GetWorkItem :one
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND native_id=sqlc.arg(native_id);

-- name: CreateWorkItem :one
INSERT INTO issues (
    organization_id,
    native_id,
    project_id,
    number,
    native_source_key,
    title,
    body_object_key,
    issue_contract_object_key,
    report_context_object_key,
    actor_kind,
    actor_principal_id,
    provenance_provider,
    provenance_external_id,
    archived
)
VALUES (
    sqlc.arg(organization_id),
    sqlc.arg(native_id),
    sqlc.arg(project_id),
    sqlc.arg(number),
    sqlc.arg(native_source_key),
    sqlc.arg(title),
    sqlc.arg(body_object_key),
    sqlc.arg(issue_contract_object_key),
    sqlc.arg(report_context_object_key),
    sqlc.arg(actor_kind),
    sqlc.arg(actor_principal_id),
    sqlc.arg(provenance_provider),
    sqlc.arg(provenance_external_id),
    sqlc.arg(archived)
) RETURNING *;

-- name: UpdateWorkItem :one
UPDATE issues SET
    number=sqlc.arg(number),
    event_sequence=sqlc.arg(event_sequence),
    native_source_key=sqlc.arg(native_source_key),
    title=sqlc.arg(title),
    body_object_key=sqlc.arg(body_object_key),
    issue_contract_object_key=sqlc.arg(issue_contract_object_key),
    report_context_object_key=sqlc.arg(report_context_object_key),
    actor_kind=sqlc.arg(actor_kind),
    actor_principal_id=sqlc.arg(actor_principal_id),
    provenance_provider=sqlc.arg(provenance_provider),
    provenance_external_id=sqlc.arg(provenance_external_id),
    archived=sqlc.arg(archived),
    revision=revision+1,
    updated_at=CURRENT_TIMESTAMP,
    native_updated_at=CURRENT_TIMESTAMP
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND native_id=sqlc.arg(native_id) AND revision=sqlc.arg(expected_revision) RETURNING *;

-- name: DeleteWorkItem :execrows
DELETE FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND native_id=sqlc.arg(native_id);

-- name: ListWorkItems :many
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND number>sqlc.arg(after)
ORDER BY number LIMIT sqlc.arg(page_limit);
