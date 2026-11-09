-- name: SchedulerEventHeads :many
SELECT h.head_kind,h.sequence AS head_sequence,h.scheduler_outcome AS head_outcome,
h.actor_kind AS head_actor_kind,h.actor_principal_id AS head_actor_principal_id,sqlc.embed(e)
FROM scheduler_event_heads h
CROSS JOIN LATERAL (
 SELECT * FROM collaboration_events e
 WHERE e.organization_id=h.organization_id AND e.id>=h.id AND e.id<=h.id
 ORDER BY e.id LIMIT 1 OFFSET 0
) e
WHERE h.organization_id=sqlc.arg(organization_id) AND h.project_id=sqlc.arg(project_id) AND h.work_item_id=sqlc.arg(work_item_id)
ORDER BY h.head_kind
LIMIT 3;
