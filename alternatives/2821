-- name: ListSummaryProjects :many
SELECT p.id,p.name,p.slug
FROM hosted_project_grants g
JOIN projects p ON p.organization_id=g.organization_id AND p.id=g.project_id
WHERE g.organization_id=sqlc.arg(organization_id)
 AND g.user_id=sqlc.arg(user_id)
 AND p.profile='native' AND p.deleted_at IS NULL
ORDER BY p.id LIMIT sqlc.arg(page_limit);
