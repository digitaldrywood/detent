-- name: RunnerClaimSlots :many
SELECT l.lease_id, COALESCE(a.runner_id,'')::text AS runner_id
FROM leases l LEFT JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
WHERE l.organization_id=sqlc.arg(organization_id) AND l.machine_id=sqlc.arg(machine_id)
AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)
ORDER BY l.lease_id LIMIT 10001;

-- name: RunnerReleasedAttemptCandidates :many
WITH page AS MATERIALIZED (
SELECT n.id,n.issue_id,n.lease_id,n.status,n.cleanup_acknowledged_at,n.worker_cleanup_required,n.runtime_summary_sequence,n.sequence,n.fencing_token,n.worker_pid FROM native_attempts n
WHERE n.organization_id=sqlc.arg(organization_id) AND n.project_id=sqlc.arg(project_id) AND n.id>sqlc.arg(after_id)
AND n.cleanup_acknowledged_at IS NULL AND n.status IN ('running','interrupted','cancelled') AND (n.worker_cleanup_required IS DISTINCT FROM FALSE OR n.runtime_summary_sequence IS DISTINCT FROM n.sequence)
ORDER BY n.id LIMIT sqlc.arg(page_size)
)
SELECT page.id AS scanned_id,
CASE WHEN i.project_id=sqlc.arg(project_id) AND a.runner_id=sqlc.arg(runner_id)
AND l.machine_id=sqlc.arg(machine_id) AND l.issue_id=page.issue_id AND l.fencing_token=page.fencing_token
AND page.worker_pid IS NULL AND (page.worker_cleanup_required IS DISTINCT FROM FALSE OR page.runtime_summary_sequence IS DISTINCT FROM page.sequence) AND l.released_at IS NOT NULL AND page.cleanup_acknowledged_at IS NULL AND page.status IN ('running','interrupted','cancelled')
THEN i.native_id ELSE '' END::text AS work_item_id,
COALESCE(page.worker_cleanup_required AND page.runtime_summary_sequence=page.sequence,false)::boolean AS cleanup_known
FROM page
JOIN leases l ON l.organization_id=sqlc.arg(organization_id) AND l.lease_id=page.lease_id
JOIN issues i ON i.organization_id=sqlc.arg(organization_id) AND i.id=page.issue_id
LEFT JOIN runner_lease_assignments a ON a.organization_id=sqlc.arg(organization_id) AND a.lease_id=page.lease_id
ORDER BY page.id;

-- name: RemoveRunnerIdentity :execrows
UPDATE runner_identities SET removed_at=sqlc.arg(removed_at),revision=revision+1
WHERE organization_id=sqlc.arg(organization_id) AND runner_id=sqlc.arg(runner_id) AND removed_at IS NULL;

-- name: RunnerAttemptWorkers :many
WITH page AS MATERIALIZED (
 SELECT n.id,n.issue_id,n.lease_id,n.fencing_token,n.worker_pid,n.worker_pgid,n.worker_started_at,n.worker_start_key
 FROM native_attempts n
 WHERE n.organization_id=sqlc.arg(organization_id) AND n.project_id=sqlc.arg(project_id) AND n.worker_runner_id=sqlc.arg(runner_id)
 AND n.worker_pid IS NOT NULL AND n.cleanup_acknowledged_at IS NULL AND n.id>sqlc.arg(after_id)
 ORDER BY n.id LIMIT sqlc.arg(page_size)
)
SELECT page.id,i.native_id AS work_item_id,page.lease_id,page.fencing_token,l.machine_id,l.expires_at,
 page.worker_pid,page.worker_pgid,page.worker_started_at,page.worker_start_key,
 CASE WHEN l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz THEN 'live'
 WHEN EXISTS (SELECT 1 FROM leases newer WHERE newer.organization_id=l.organization_id AND newer.issue_id=l.issue_id AND newer.fencing_token>l.fencing_token) THEN 'replaced'
 WHEN l.released_at IS NULL OR l.release_reason='expired' THEN 'expired'
 ELSE 'finished' END::text AS lease_state
FROM page
JOIN leases l ON l.organization_id=sqlc.arg(organization_id) AND l.lease_id=page.lease_id AND l.issue_id=page.issue_id AND l.fencing_token=page.fencing_token
JOIN work_item_identities i ON i.organization_id=l.organization_id AND i.id=page.issue_id AND i.project_id=sqlc.arg(project_id)
JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id AND a.runner_id=sqlc.arg(runner_id)
ORDER BY page.id;
