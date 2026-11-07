-- name: GetWorkItemByNumber :one
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND number=sqlc.arg(number);

-- name: GetWorkItem :one
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND native_id=sqlc.arg(native_id);

-- name: GetWorkItemByNativeID :one
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND native_id=sqlc.arg(native_id);

-- name: GetWorkItemByID :one
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND id=sqlc.arg(id);

-- name: GetWorkItemsByIDs :many
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND id=ANY(sqlc.arg(ids)::bigint[]);

-- name: GetNativeWorkItemsByIDs :many
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND native_id=ANY(sqlc.arg(native_ids)::text[]);

-- name: CreateWorkItem :one
INSERT INTO issues (
    last_activity_at,
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
    issue_type,
    priority,
    body_object_key,
    issue_contract_object_key,
    report_context_object_key,
    actor_kind,
    actor_principal_id,
    provenance_provider,
    provenance_external_id,
    archived,
    workflow_state_name
)
VALUES (
    COALESCE(sqlc.narg(last_activity_at)::timestamptz, CURRENT_TIMESTAMP),
    public.allocate_core_record_id('public.issues_id_seq'::regclass, sqlc.arg(imported_id)::bigint),
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
    sqlc.arg(issue_type),
    sqlc.narg(priority),
    sqlc.arg(body_object_key),
    sqlc.arg(issue_contract_object_key),
    sqlc.arg(report_context_object_key),
    sqlc.arg(actor_kind),
    sqlc.arg(actor_principal_id),
    sqlc.arg(provenance_provider),
    sqlc.arg(provenance_external_id),
    sqlc.arg(archived),
    sqlc.narg(workflow_state_name)
) RETURNING *;

-- name: UpdateWorkItem :one
WITH previous AS MATERIALIZED (
    SELECT id,archived,workflow_state_name FROM issues
    WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
      AND native_id=sqlc.arg(native_id) AND revision=sqlc.arg(expected_revision)
    FOR UPDATE
), invalidated_admission AS (
    DELETE FROM monthly_budget_admissions a USING previous i
    WHERE a.organization_id=sqlc.arg(organization_id) AND a.issue_id=i.id
      AND (i.archived OR sqlc.arg(archived) OR EXISTS (
          SELECT 1 FROM project_workflow_states w
          WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id)
            AND w.name IN (i.workflow_state_name,sqlc.narg(workflow_state_name))
            AND (w.terminal OR lower(w.name) IN ('blocked','backlog','cancelled'))
      ))
)
UPDATE issues SET
    last_activity_at=greatest(issues.last_activity_at,COALESCE(sqlc.narg(last_activity_at)::timestamptz,issues.last_activity_at)),
    number=sqlc.arg(number),
    event_sequence=sqlc.arg(event_sequence),
    native_source_key=sqlc.arg(native_source_key),
    title=sqlc.arg(title),
    issue_type=sqlc.arg(issue_type),
    priority=sqlc.narg(priority),
    body_object_key=sqlc.arg(body_object_key),
    issue_contract_object_key=sqlc.arg(issue_contract_object_key),
    report_context_object_key=sqlc.arg(report_context_object_key),
    actor_kind=sqlc.arg(actor_kind),
    actor_principal_id=sqlc.arg(actor_principal_id),
    provenance_provider=sqlc.arg(provenance_provider),
    provenance_external_id=sqlc.arg(provenance_external_id),
    archived=sqlc.arg(archived),
    workflow_state_name=sqlc.narg(workflow_state_name),
    revision=issues.revision+1,
    updated_at=CURRENT_TIMESTAMP,
    native_updated_at=CURRENT_TIMESTAMP
FROM previous
WHERE issues.id=previous.id AND issues.organization_id=sqlc.arg(organization_id) AND issues.project_id=sqlc.arg(project_id) AND issues.native_id=sqlc.arg(native_id) AND issues.revision=sqlc.arg(expected_revision) RETURNING issues.*;

-- name: DeleteWorkItem :execrows
DELETE FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND native_id=sqlc.arg(native_id);

-- name: ListLegacyWorkItems :many
SELECT * FROM issues
WHERE organization_id=sqlc.arg(organization_id)
  AND project_id=sqlc.arg(project_id)
  AND number>sqlc.arg(after)
ORDER BY number LIMIT sqlc.arg(page_limit);

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

-- name: CompareAndSwapSourceRecovery :execrows
UPDATE issues SET recovery_runner_id=sqlc.arg(runner_id), recovery_version_id=sqlc.arg(version_id),
 revision=revision+1, updated_at=CURRENT_TIMESTAMP, native_updated_at=CURRENT_TIMESTAMP
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND native_id=sqlc.arg(native_id)
 AND revision=sqlc.arg(expected_revision) AND event_sequence=sqlc.arg(expected_event_sequence) AND NOT archived;
