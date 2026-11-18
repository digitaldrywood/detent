-- name: ListCoordinatorAttentionItems :many
SELECT i.native_id, i.project_id
FROM issues i
LEFT JOIN project_workflow_states ws ON ws.organization_id = i.organization_id AND ws.project_id = i.project_id AND ws.name = i.workflow_state_name
WHERE i.organization_id = sqlc.arg(organization_id)
  AND i.project_id = ANY(sqlc.arg(project_ids)::text[])
  AND COALESCE(ws.terminal, false) = false
ORDER BY i.native_updated_at DESC, i.native_id
LIMIT sqlc.arg(page_limit);
