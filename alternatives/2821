-- name: ExportWorkItems :many
SELECT i.*
FROM issues i
LEFT JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND (sqlc.narg(archived)::boolean IS NULL OR i.archived=sqlc.narg(archived)::boolean)
AND (sqlc.narg(terminal)::boolean IS NULL OR w.terminal=sqlc.narg(terminal)::boolean)
AND (NOT sqlc.arg(reporter)::boolean OR (sqlc.arg(principal_id)::text<>'' AND i.actor_principal_id=sqlc.arg(principal_id)::text))
AND (cardinality(sqlc.arg(states)::text[])=0 OR i.workflow_state_name=ANY(sqlc.arg(states)::text[]))
AND (cardinality(sqlc.arg(labels)::text[])=0 OR EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY(sqlc.arg(labels)::text[])))
AND NOT EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label='detent:workspace')
AND i.number>sqlc.arg(after_number)::bigint
ORDER BY i.number LIMIT sqlc.arg(page_limit)::integer;

-- name: CountExportWorkItems :one
SELECT COUNT(*)
FROM issues i
LEFT JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND (sqlc.narg(archived)::boolean IS NULL OR i.archived=sqlc.narg(archived)::boolean)
AND (sqlc.narg(terminal)::boolean IS NULL OR w.terminal=sqlc.narg(terminal)::boolean)
AND (NOT sqlc.arg(reporter)::boolean OR (sqlc.arg(principal_id)::text<>'' AND i.actor_principal_id=sqlc.arg(principal_id)::text))
AND (cardinality(sqlc.arg(states)::text[])=0 OR i.workflow_state_name=ANY(sqlc.arg(states)::text[]))
AND (cardinality(sqlc.arg(labels)::text[])=0 OR EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY(sqlc.arg(labels)::text[])))
AND NOT EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label='detent:workspace');
