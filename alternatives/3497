-- name: ReadSummaryAttention :many
WITH candidates AS MATERIALIZED (
 SELECT i.* FROM issues i
 WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
 AND NOT i.archived AND i.created_at<sqlc.arg(window_to)
 AND (sqlc.arg(principal)::text='' OR i.actor_principal_id=sqlc.arg(principal))
 AND (i.workflow_state_name IN ('Blocked','Human Review','In Progress')
 OR (i.workflow_state_name='Done' AND i.actor_principal_id=sqlc.arg(recipient)
 AND NOT EXISTS (SELECT 1 FROM native_board_items b WHERE b.organization_id=i.organization_id
 AND b.project_id=i.project_id AND b.work_item_id=i.native_id AND b.source_revision=i.revision
 AND b.source_event_sequence=i.event_sequence AND b.state=i.workflow_state_name AND b.verification_state<>'pending'))
 OR EXISTS (SELECT 1 FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
 AND e.type='workflow.transitioned' AND e.to_state='Rework'
 AND e.recorded_at>=sqlc.arg(window_from) AND e.recorded_at<sqlc.arg(window_to) LIMIT 1 OFFSET 1))
 ORDER BY i.number,i.native_id LIMIT sqlc.arg(page_limit)
)
SELECT i.native_id,
 coalesce((SELECT e.recorded_at FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
 AND e.type='workflow.transitioned' AND e.from_state<>e.to_state AND e.recorded_at<sqlc.arg(window_to)
 ORDER BY e.sequence DESC LIMIT 1),i.native_created_at)::timestamptz AS attention_at,
 (SELECT count(*) FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<sqlc.arg(window_to))::bigint AS attempts,
 (SELECT count(*) FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
 AND e.type='workflow.transitioned' AND e.to_state='Rework' AND e.recorded_at>=sqlc.arg(window_from) AND e.recorded_at<sqlc.arg(window_to))::bigint AS reworks
FROM candidates i ORDER BY i.number,i.native_id;
