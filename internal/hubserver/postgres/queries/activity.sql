-- name: ActivityLatestRunEvents :many
SELECT latest.*
FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
CROSS JOIN LATERAL (
 SELECT * FROM collaboration_events e
 WHERE e.organization_id=a.organization_id AND e.attempt_id=a.id AND e.attempt_id<>''
 ORDER BY e.sequence DESC LIMIT 1
) latest
WHERE a.organization_id=sqlc.arg(organization_id) AND a.id=ANY(sqlc.arg(attempt_ids)::text[]) AND latest.project_id=i.project_id;

-- name: ActivityIssueLinks :many
SELECT i.id,wi.workspace_id,coalesce(linked.change_id,'')::text AS change_id,coalesce(v.external_url,'')::text AS publication_url
FROM (SELECT DISTINCT unnest(sqlc.arg(issue_ids)::bigint[]) AS id) requested
CROSS JOIN LATERAL (
 SELECT i.organization_id,i.project_id,i.native_id,i.id FROM issues i WHERE i.organization_id=sqlc.arg(organization_id) AND i.id=requested.id LIMIT 1
) i
LEFT JOIN workspace_items wi ON wi.organization_id=i.organization_id AND wi.project_id=i.project_id AND wi.work_item_id=i.native_id
LEFT JOIN LATERAL (
 SELECT change_id FROM change_issue_links l
 WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id
 ORDER BY l.created_at DESC,l.change_id DESC LIMIT 1
) linked ON true
LEFT JOIN LATERAL (
 SELECT c.organization_id,c.current_version_id FROM change_requests c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.id=linked.change_id LIMIT 1
) c ON true
LEFT JOIN change_versions v ON v.organization_id=c.organization_id AND v.id=c.current_version_id;
