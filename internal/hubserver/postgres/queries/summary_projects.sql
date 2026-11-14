-- name: ListSummaryProjects :many
SELECT p.id,p.name,p.slug
FROM projects p
WHERE p.organization_id=sqlc.arg(organization_id)
 AND p.profile='native' AND p.deleted_at IS NULL
 AND ((SELECT hosted_administrator_access(sqlc.arg(organization_id),sqlc.arg(user_id),''))
  OR EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.user_id=sqlc.arg(user_id)))
ORDER BY p.id LIMIT sqlc.arg(page_limit);
