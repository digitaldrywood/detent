-- name: GetWorkItem :one
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND native_id=sqlc.arg(native_id);

-- name: CreateWorkItem :one
INSERT INTO issues (
    id,
    created_at,
    updated_at,
    revision,
    native_created_at,
    native_updated_at,
    event_sequence,
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
    COALESCE(NULLIF(sqlc.arg(imported_id)::bigint, 0), nextval('issues_id_seq')),
    COALESCE(sqlc.narg(created_at)::timestamptz, CURRENT_TIMESTAMP),
    COALESCE(sqlc.narg(updated_at)::timestamptz, CURRENT_TIMESTAMP),
    COALESCE(NULLIF(sqlc.arg(revision)::bigint, 0), 1),
    COALESCE(sqlc.narg(native_created_at)::timestamptz, CURRENT_TIMESTAMP),
    COALESCE(sqlc.narg(native_updated_at)::timestamptz, CURRENT_TIMESTAMP),
    sqlc.arg(event_sequence),
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

-- name: GetWorkItemLegacyCursor :one
SELECT created_at, id FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND number=sqlc.arg(after);

-- name: GetWorkItemCursor :one
SELECT created_at, id FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND id=sqlc.arg(id);

-- name: PageWorkItems :many
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND (created_at, id) > (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::bigint)
ORDER BY created_at, id LIMIT sqlc.arg(page_limit);

-- name: ScanWorkItems :many
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND id>sqlc.arg(after_id) AND id<=sqlc.arg(through_id)
ORDER BY id LIMIT sqlc.arg(page_limit);

-- name: WorkItemScanThrough :one
SELECT id FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
ORDER BY id DESC LIMIT 1;
