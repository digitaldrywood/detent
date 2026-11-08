-- name: SchedulerEventHeads :many
SELECT * FROM scheduler_event_heads
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id)
ORDER BY head_kind;
