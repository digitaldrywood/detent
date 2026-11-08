-- name: OperatorOrphanScanThrough :one
WITH candidates AS MATERIALIZED (
 SELECT started_at,id FROM runner_sessions
 WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
 AND completed_at IS NULL AND final_state='running'
 AND (provider_thread_id<>'' OR provider_session_id<>'')
 AND (started_at,id)<(sqlc.arg(before_time)::timestamptz,sqlc.arg(before_id)::bigint)
 ORDER BY started_at DESC,id DESC LIMIT sqlc.arg(page_limit)::integer
)
SELECT count(*) OVER()::bigint AS scanned,started_at,id
FROM candidates ORDER BY started_at,id LIMIT 1;

-- name: OperatorOrphanSessions :many
WITH candidates AS MATERIALIZED (
 SELECT rs.* FROM runner_sessions rs
 WHERE rs.organization_id=sqlc.arg(organization_id) AND rs.project_id=sqlc.arg(project_id)
 AND rs.completed_at IS NULL AND rs.final_state='running'
 AND (rs.provider_thread_id<>'' OR rs.provider_session_id<>'')
 AND (rs.started_at,rs.id)<(sqlc.arg(before_time)::timestamptz,sqlc.arg(before_id)::bigint)
 AND (rs.started_at,rs.id)>=(sqlc.arg(through_time)::timestamptz,sqlc.arg(through_id)::bigint)
 ORDER BY rs.started_at DESC,rs.id DESC LIMIT sqlc.arg(page_limit)::integer
)
SELECT s.*,a.status AS attempt_status,l.lease_id,l.fencing_token,l.expires_at,l.released_at
FROM candidates s
JOIN native_attempts a ON a.organization_id=s.organization_id AND a.id=s.attempt_id
JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
JOIN runner_lease_assignments ra ON ra.organization_id=l.organization_id AND ra.lease_id=l.lease_id
WHERE ra.runner_id=sqlc.arg(runner_id) AND l.machine_id=sqlc.arg(machine_id)
AND (l.released_at IS NOT NULL OR l.expires_at<=sqlc.arg(now)::timestamptz)
ORDER BY s.started_at DESC,s.id DESC LIMIT sqlc.arg(page_limit)::integer;
