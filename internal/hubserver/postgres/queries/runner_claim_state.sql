-- name: RunnerClaimSlots :many
SELECT l.lease_id, COALESCE(a.runner_id,'')::text AS runner_id
FROM leases l LEFT JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
WHERE l.organization_id=sqlc.arg(organization_id) AND l.machine_id=sqlc.arg(machine_id)
AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)
ORDER BY l.lease_id LIMIT 10001;

-- name: RunnerReleasedAttemptCandidates :many
WITH page AS MATERIALIZED (
SELECT n.id,n.issue_id,n.lease_id,n.status FROM native_attempts n
WHERE n.organization_id=sqlc.arg(organization_id) AND n.id>sqlc.arg(after_id)
ORDER BY n.id LIMIT sqlc.arg(page_size)
)
SELECT page.id AS scanned_id,
CASE WHEN i.project_id=sqlc.arg(project_id) AND a.runner_id=sqlc.arg(runner_id)
AND l.machine_id=sqlc.arg(machine_id) AND l.released_at IS NOT NULL AND page.status IN ('running','interrupted','cancelled')
THEN i.native_id ELSE '' END::text AS work_item_id
FROM page
JOIN leases l ON l.organization_id=sqlc.arg(organization_id) AND l.lease_id=page.lease_id
JOIN issues i ON i.organization_id=sqlc.arg(organization_id) AND i.id=page.issue_id
LEFT JOIN runner_lease_assignments a ON a.organization_id=sqlc.arg(organization_id) AND a.lease_id=page.lease_id
ORDER BY page.id;
