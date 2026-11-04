-- name: WorkItemBlockers :many
WITH selected AS MATERIALIZED (
  SELECT d.blocker_project_id,d.blocker_issue_id FROM issue_dependencies d
  WHERE d.organization_id=sqlc.arg(organization_id) AND d.dependent_project_id=sqlc.arg(project_id)
    AND d.dependent_issue_id=(SELECT source.id FROM issues source WHERE source.organization_id=sqlc.arg(organization_id) AND source.project_id=sqlc.arg(project_id) AND source.native_id=sqlc.arg(native_id))
    AND (d.blocker_project_id,d.blocker_issue_id)>(sqlc.arg(after_project_id)::text,sqlc.arg(after_issue_id)::bigint)
  ORDER BY d.blocker_project_id,d.blocker_issue_id LIMIT sqlc.arg(page_limit)
)
SELECT selected.blocker_project_id,selected.blocker_issue_id,blocker.native_id,blocker.native_source_key FROM selected
CROSS JOIN LATERAL (
  SELECT b.native_id,b.native_source_key FROM issues b
  WHERE b.organization_id=sqlc.arg(organization_id) AND b.project_id=selected.blocker_project_id AND b.id=selected.blocker_issue_id OFFSET 0
) blocker
ORDER BY selected.blocker_project_id,selected.blocker_issue_id;

-- name: RelationshipPage :many
WITH candidates AS (
    SELECT 'dependency'::text AS relationship_type, 'blocked_by'::text AS direction, d.blocker_project_id AS related_project_id, d.blocker_issue_id AS related_issue_id, d.created_at
    FROM issue_dependencies d
    WHERE d.organization_id=sqlc.arg(organization_id) AND d.dependent_project_id=sqlc.arg(project_id) AND d.dependent_issue_id=(SELECT i.id FROM issues i WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=sqlc.arg(native_id))
    UNION ALL
    SELECT 'dependency'::text, 'blocks'::text, d.dependent_project_id, d.dependent_issue_id, d.created_at
    FROM issue_dependencies d
    WHERE d.organization_id=sqlc.arg(organization_id) AND d.blocker_project_id=sqlc.arg(project_id) AND d.blocker_issue_id=(SELECT i.id FROM issues i WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=sqlc.arg(native_id))
    UNION ALL
    SELECT 'related'::text, 'related'::text,
        CASE WHEN first_issue_id=source.id THEN second_project_id ELSE first_project_id END,
        CASE WHEN first_issue_id=source.id THEN second_issue_id ELSE first_issue_id END,
        r.created_at
    FROM issue_relationships r
    JOIN issues source ON source.organization_id=r.organization_id AND source.id IN (r.first_issue_id,r.second_issue_id)
    WHERE r.organization_id=sqlc.arg(organization_id) AND source.project_id=sqlc.arg(project_id) AND source.native_id=sqlc.arg(native_id)
)
SELECT c.relationship_type,c.direction,c.related_project_id,target.native_id,c.created_at,target.number,target.title,target.workflow_state_name,COALESCE(states.terminal,false),COALESCE(states.dispatchable,false),target.actor_principal_id,COALESCE(organizations.slug,'') AS organization_slug,projects.slug AS project_slug,
    COALESCE(closed.closed_at,'0001-01-01 00:00:00+00'::timestamptz) AS closed_at,
    COALESCE(started.started_at,'0001-01-01 00:00:00+00'::timestamptz) AS started_at
FROM candidates c
JOIN issues target ON target.organization_id=sqlc.arg(organization_id) AND target.project_id=c.related_project_id AND target.id=c.related_issue_id
JOIN projects ON projects.organization_id=target.organization_id AND projects.id=target.project_id
LEFT JOIN organizations ON organizations.organization_id=target.organization_id
LEFT JOIN project_workflow_states states ON states.organization_id=target.organization_id AND states.project_id=target.project_id AND states.name=target.workflow_state_name
LEFT JOIN LATERAL (
        SELECT e.recorded_at AS closed_at FROM collaboration_events e
        JOIN project_workflow_states destination ON destination.organization_id=e.organization_id AND destination.project_id=e.project_id AND destination.name=e.to_state AND destination.terminal
        JOIN project_workflow_states origin ON origin.organization_id=e.organization_id AND origin.project_id=e.project_id AND origin.name=e.from_state AND NOT origin.terminal
        WHERE e.organization_id=target.organization_id AND e.project_id=target.project_id AND e.work_item_id=target.native_id
        ORDER BY e.sequence DESC LIMIT 1
    ) closed(closed_at) ON states.terminal
LEFT JOIN LATERAL (
        SELECT attempts.started_at FROM native_attempts attempts
        JOIN leases ON leases.organization_id=attempts.organization_id AND leases.lease_id=attempts.lease_id AND leases.fencing_token=attempts.fencing_token
        WHERE attempts.organization_id=target.organization_id AND attempts.issue_id=target.id AND attempts.status='running' AND leases.released_at IS NULL AND leases.expires_at>CURRENT_TIMESTAMP
        ORDER BY attempts.fencing_token DESC LIMIT 1
    ) started ON true
WHERE (c.relationship_type,c.direction,c.related_project_id,target.native_id) > (sqlc.arg(after_type)::text,sqlc.arg(after_direction)::text,sqlc.arg(after_project_id)::text,sqlc.arg(after_native_id)::text)
ORDER BY c.relationship_type,c.direction,c.related_project_id,target.native_id
LIMIT sqlc.arg(page_limit);

-- name: RelationshipBatch :many
WITH requested AS (
    SELECT DISTINCT unnest(sqlc.arg(work_item_ids)::text[]) AS native_id
), candidates AS (
    SELECT source.native_id AS work_item_id, 'dependency'::text AS relationship_type, 'blocked_by'::text AS direction, d.blocker_project_id AS related_project_id, d.blocker_issue_id AS related_issue_id, d.created_at
    FROM requested
    JOIN issues source ON source.organization_id=sqlc.arg(organization_id) AND source.project_id=sqlc.arg(project_id) AND source.native_id=requested.native_id
    JOIN issue_dependencies d ON d.organization_id=source.organization_id AND d.dependent_project_id=source.project_id AND d.dependent_issue_id=source.id
    UNION ALL
    SELECT source.native_id, 'dependency'::text, 'blocks'::text, d.dependent_project_id, d.dependent_issue_id, d.created_at
    FROM requested
    JOIN issues source ON source.organization_id=sqlc.arg(organization_id) AND source.project_id=sqlc.arg(project_id) AND source.native_id=requested.native_id
    JOIN issue_dependencies d ON d.organization_id=source.organization_id AND d.blocker_project_id=source.project_id AND d.blocker_issue_id=source.id
    UNION ALL
    SELECT source.native_id, 'related'::text, 'related'::text,
        CASE WHEN r.first_issue_id=source.id THEN r.second_project_id ELSE r.first_project_id END,
        CASE WHEN r.first_issue_id=source.id THEN r.second_issue_id ELSE r.first_issue_id END,
        r.created_at
    FROM requested
    JOIN issues source ON source.organization_id=sqlc.arg(organization_id) AND source.project_id=sqlc.arg(project_id) AND source.native_id=requested.native_id
    JOIN issue_relationships r ON r.organization_id=source.organization_id AND source.id IN (r.first_issue_id,r.second_issue_id)
)
SELECT c.work_item_id,c.relationship_type,c.direction,c.related_project_id,target.native_id,c.created_at,target.number,target.title,target.workflow_state_name,COALESCE(states.terminal,false),COALESCE(states.dispatchable,false),target.actor_principal_id,COALESCE(organizations.slug,'') AS organization_slug,projects.slug AS project_slug,
    COALESCE(closed.closed_at,'0001-01-01 00:00:00+00'::timestamptz) AS closed_at,
    COALESCE(started.started_at,'0001-01-01 00:00:00+00'::timestamptz) AS started_at,
    (projects.deleted_at IS NULL AND projects.profile='native') AS related_project_visible
FROM candidates c
JOIN issues target ON target.organization_id=sqlc.arg(organization_id) AND target.project_id=c.related_project_id AND target.id=c.related_issue_id
JOIN projects ON projects.organization_id=target.organization_id AND projects.id=target.project_id
LEFT JOIN organizations ON organizations.organization_id=target.organization_id
LEFT JOIN project_workflow_states states ON states.organization_id=target.organization_id AND states.project_id=target.project_id AND states.name=target.workflow_state_name
LEFT JOIN LATERAL (
        SELECT e.recorded_at AS closed_at FROM collaboration_events e
        JOIN project_workflow_states destination ON destination.organization_id=e.organization_id AND destination.project_id=e.project_id AND destination.name=e.to_state AND destination.terminal
        JOIN project_workflow_states origin ON origin.organization_id=e.organization_id AND origin.project_id=e.project_id AND origin.name=e.from_state AND NOT origin.terminal
        WHERE e.organization_id=target.organization_id AND e.project_id=target.project_id AND e.work_item_id=target.native_id
        ORDER BY e.sequence DESC LIMIT 1
    ) closed(closed_at) ON states.terminal
LEFT JOIN LATERAL (
        SELECT attempts.started_at FROM native_attempts attempts
        JOIN leases ON leases.organization_id=attempts.organization_id AND leases.lease_id=attempts.lease_id AND leases.fencing_token=attempts.fencing_token
        WHERE attempts.organization_id=target.organization_id AND attempts.issue_id=target.id AND attempts.status='running' AND leases.released_at IS NULL AND leases.expires_at>CURRENT_TIMESTAMP
        ORDER BY attempts.fencing_token DESC LIMIT 1
    ) started ON true
ORDER BY c.work_item_id,c.relationship_type,c.direction,c.related_project_id,target.native_id;

-- name: LockRelationshipGraph :one
SELECT organization_id FROM organizations
WHERE organization_id=sqlc.arg(organization_id)
FOR NO KEY UPDATE;

-- name: InsertDependency :exec
INSERT INTO issue_dependencies (organization_id,dependent_project_id,dependent_issue_id,blocker_project_id,blocker_issue_id,provenance,created_at,updated_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(dependent_project_id),sqlc.arg(dependent_issue_id),sqlc.arg(blocker_project_id),sqlc.arg(blocker_issue_id),sqlc.arg(provenance),sqlc.arg(created_at),sqlc.arg(updated_at))
ON CONFLICT DO NOTHING;

-- name: ImportDependency :exec
INSERT INTO issue_dependencies (organization_id,dependent_project_id,dependent_issue_id,blocker_project_id,blocker_issue_id,provenance,source_version,created_at,updated_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(dependent_project_id),sqlc.arg(dependent_issue_id),sqlc.arg(blocker_project_id),sqlc.arg(blocker_issue_id),sqlc.arg(provenance),sqlc.narg(source_version),sqlc.arg(created_at),sqlc.arg(updated_at))
ON CONFLICT DO NOTHING;

-- name: GetDependency :one
SELECT provenance,source_version,created_at,updated_at FROM issue_dependencies
WHERE organization_id=sqlc.arg(organization_id) AND dependent_project_id=sqlc.arg(dependent_project_id) AND dependent_issue_id=sqlc.arg(dependent_issue_id) AND blocker_project_id=sqlc.arg(blocker_project_id) AND blocker_issue_id=sqlc.arg(blocker_issue_id);

-- name: DeleteDependency :execrows
DELETE FROM issue_dependencies
WHERE organization_id=sqlc.arg(organization_id) AND dependent_project_id=sqlc.arg(dependent_project_id) AND dependent_issue_id=sqlc.arg(dependent_issue_id) AND blocker_project_id=sqlc.arg(blocker_project_id) AND blocker_issue_id=sqlc.arg(blocker_issue_id);

-- name: InsertRelated :exec
INSERT INTO issue_relationships (organization_id,first_project_id,first_issue_id,second_project_id,second_issue_id,relationship_type,created_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(first_project_id),sqlc.arg(first_issue_id),sqlc.arg(second_project_id),sqlc.arg(second_issue_id),'related',sqlc.arg(created_at))
ON CONFLICT DO NOTHING;

-- name: DeleteRelated :execrows
DELETE FROM issue_relationships
WHERE organization_id=sqlc.arg(organization_id) AND first_project_id=sqlc.arg(first_project_id) AND first_issue_id=sqlc.arg(first_issue_id) AND second_project_id=sqlc.arg(second_project_id) AND second_issue_id=sqlc.arg(second_issue_id) AND relationship_type='related';

-- name: LabelNamePage :many
SELECT DISTINCT label FROM issue_labels
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND label>sqlc.arg(after_label)
ORDER BY label LIMIT sqlc.arg(page_limit);

-- name: WorkItemLabelPage :many
SELECT label FROM issue_labels
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND label>sqlc.arg(after_label)
ORDER BY label LIMIT sqlc.arg(page_limit);

-- name: DeleteWorkItemLabels :exec
DELETE FROM issue_labels WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id);

-- name: InsertWorkItemLabel :exec
INSERT INTO issue_labels (organization_id,project_id,work_item_id,label)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(work_item_id),sqlc.arg(label));

-- name: LockWorkItemBatch :many
WITH requested AS MATERIALIZED (
 SELECT requested.project_id,requested.native_id FROM ROWS FROM(unnest(sqlc.arg(project_ids)::text[]),unnest(sqlc.arg(native_ids)::text[])) AS requested(project_id,native_id)
)
SELECT i.* FROM requested r JOIN issues i ON i.organization_id=sqlc.arg(organization_id) AND i.project_id=r.project_id AND i.native_id=r.native_id
ORDER BY i.project_id COLLATE "C",i.native_id COLLATE "C" FOR UPDATE OF i;
