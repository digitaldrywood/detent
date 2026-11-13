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
WITH published_source AS MATERIALIZED (
 SELECT v.revision AS source_revision,v.source_scheduled,v.source_observed_at FROM collaboration_versions v
 WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND v.work_item_id=sqlc.arg(native_id) AND v.record_id=sqlc.arg(native_id) AND v.revision=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) AND v.source_known AND sqlc.arg(source_revision)::bigint<>COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1)
)
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
    workflow_state_name,
    terminal_entered_at,
    archive_restored_at,
    archive_restore_known,
    source_revision,
    source_scheduled,
    source_observed_at
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
    sqlc.narg(workflow_state_name),
    CASE WHEN sqlc.arg(current_write)::boolean THEN
        CASE WHEN EXISTS (SELECT 1 FROM project_workflow_states w WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id) AND w.name=sqlc.narg(workflow_state_name) AND w.terminal)
        THEN COALESCE(sqlc.narg(terminal_clock)::timestamptz,CURRENT_TIMESTAMP) ELSE NULL END
    ELSE sqlc.narg(terminal_entered_at)::timestamptz END,
    CASE WHEN sqlc.arg(current_write)::boolean THEN NULL ELSE sqlc.narg(archive_restored_at)::timestamptz END,
    CASE WHEN sqlc.arg(current_write)::boolean THEN true ELSE sqlc.arg(archive_restore_known)::boolean END,
    CASE WHEN sqlc.arg(source_revision)::bigint=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) THEN sqlc.arg(source_revision)::bigint ELSE COALESCE((SELECT published_source.source_revision FROM published_source),0) END,
    CASE WHEN sqlc.arg(source_revision)::bigint=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) THEN sqlc.arg(source_scheduled)::boolean ELSE COALESCE((SELECT published_source.source_scheduled FROM published_source),false) END,
    CASE WHEN sqlc.arg(source_revision)::bigint=COALESCE(NULLIF(sqlc.arg(revision)::bigint,0),1) THEN sqlc.narg(source_observed_at)::timestamptz ELSE COALESCE((SELECT published_source.source_observed_at FROM published_source),NULL::timestamptz) END
) RETURNING *;

-- name: UpdateWorkItem :one
WITH published_source AS MATERIALIZED (
 SELECT v.revision AS source_revision,v.source_scheduled,v.source_observed_at FROM collaboration_versions v
 WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND v.work_item_id=sqlc.arg(native_id) AND v.record_id=sqlc.arg(native_id) AND v.revision=sqlc.arg(expected_revision)::bigint+1 AND v.source_known AND sqlc.arg(source_revision)::bigint<>sqlc.arg(expected_revision)::bigint+1
),
previous AS MATERIALIZED (
    SELECT id,archived,workflow_state_name FROM issues
    WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
      AND native_id=sqlc.arg(native_id) AND revision=sqlc.arg(expected_revision)
    FOR NO KEY UPDATE
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
    source_revision=CASE WHEN sqlc.arg(source_revision)::bigint=sqlc.arg(expected_revision)::bigint+1 THEN sqlc.arg(source_revision)::bigint ELSE COALESCE((SELECT published_source.source_revision FROM published_source),issues.source_revision) END,
    source_scheduled=CASE WHEN sqlc.arg(source_revision)::bigint=sqlc.arg(expected_revision)::bigint+1 THEN sqlc.arg(source_scheduled)::boolean ELSE COALESCE((SELECT published_source.source_scheduled FROM published_source),issues.source_scheduled) END,
    source_observed_at=CASE WHEN sqlc.arg(source_revision)::bigint=sqlc.arg(expected_revision)::bigint+1 THEN sqlc.narg(source_observed_at)::timestamptz ELSE COALESCE((SELECT published_source.source_observed_at FROM published_source),issues.source_observed_at) END,
    last_activity_at=greatest(issues.last_activity_at,COALESCE(sqlc.narg(last_activity_at)::timestamptz,issues.last_activity_at),CASE WHEN (issues.title,issues.body_object_key) IS DISTINCT FROM (sqlc.arg(title),sqlc.arg(body_object_key)) THEN COALESCE(sqlc.narg(activity_clock)::timestamptz,sqlc.narg(terminal_clock)::timestamptz,CURRENT_TIMESTAMP) ELSE issues.last_activity_at END),
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
    archive_restored_at=CASE WHEN issues.archived AND NOT sqlc.arg(archived) THEN COALESCE(sqlc.narg(terminal_clock)::timestamptz,CURRENT_TIMESTAMP) ELSE issues.archive_restored_at END,
    archive_restore_known=CASE WHEN issues.archived AND NOT sqlc.arg(archived) THEN true ELSE issues.archive_restore_known END,
    workflow_state_name=sqlc.narg(workflow_state_name),
    terminal_entered_at=CASE
        WHEN NOT EXISTS (SELECT 1 FROM project_workflow_states w WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id) AND w.name=sqlc.narg(workflow_state_name) AND w.terminal) THEN NULL
        WHEN EXISTS (SELECT 1 FROM project_workflow_states w WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id) AND w.name=issues.workflow_state_name AND w.terminal) THEN issues.terminal_entered_at
        ELSE COALESCE(sqlc.narg(terminal_clock)::timestamptz,CURRENT_TIMESTAMP)
    END,
    revision=issues.revision+1,
    updated_at=COALESCE(sqlc.narg(terminal_clock)::timestamptz,CURRENT_TIMESTAMP),
    native_updated_at=COALESCE(sqlc.narg(terminal_clock)::timestamptz,CURRENT_TIMESTAMP)
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
