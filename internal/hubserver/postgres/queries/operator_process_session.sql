-- name: ReplaceSessionWorkerProcess :one
UPDATE runner_process_receipts p SET
 owner_generation=p.owner_generation+1,
 pid=sqlc.arg(pid),group_id=sqlc.arg(group_id),started_at=sqlc.arg(started_at),started_at_ns=sqlc.arg(started_at_ns),
 cleanup_root=sqlc.arg(cleanup_root),cleanup_path=sqlc.arg(cleanup_path),history_revision=sqlc.arg(history_revision),
 registration_object_key=sqlc.arg(registration_object_key),registration_sha256=sqlc.arg(registration_sha256),registration_bytes=sqlc.arg(registration_bytes),registration_content_bytes=sqlc.arg(registration_content_bytes),updated_at=sqlc.arg(at)
WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id) AND p.session_id=sqlc.arg(session_id) AND p.role='agent'
AND p.runner_id=sqlc.arg(runner_id) AND p.enrollment_id=sqlc.arg(enrollment_id) AND p.machine_id=sqlc.arg(machine_id)
AND p.lease_id=sqlc.arg(lease_id) AND p.fencing_token=sqlc.arg(fencing_token) AND p.authority_hash=sqlc.arg(authority_hash)
AND p.reaped_at IS NULL AND p.history_revision<sqlc.arg(history_revision)
AND EXISTS (
 SELECT 1 FROM runner_sessions s
 JOIN runner_session_history h ON h.organization_id=s.organization_id AND h.session_id=s.id AND h.revision=sqlc.arg(history_revision)
 JOIN native_attempts a ON a.organization_id=s.organization_id AND a.id=s.attempt_id
 JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
 JOIN runner_lease_assignments ra ON ra.organization_id=l.organization_id AND ra.lease_id=l.lease_id
 JOIN runner_identities r ON r.organization_id=ra.organization_id AND r.runner_id=ra.runner_id
 WHERE s.organization_id=p.organization_id AND s.project_id=p.project_id AND s.id=p.session_id
 AND s.revision=sqlc.arg(history_revision) AND s.completed_at IS NULL
 AND s.worker_pid=sqlc.arg(pid) AND s.worker_group_id=sqlc.arg(group_id) AND s.worker_started_at=sqlc.arg(started_at)
 AND h.payload_object_key=sqlc.arg(registration_object_key) AND h.payload_sha256=sqlc.arg(registration_sha256)
 AND h.payload_bytes=sqlc.arg(registration_bytes) AND h.payload_content_bytes=sqlc.arg(registration_content_bytes)
 AND l.lease_id=p.lease_id AND l.fencing_token=p.fencing_token AND l.machine_id=p.machine_id
 AND l.released_at IS NULL AND l.expires_at>sqlc.arg(at) AND a.status='running'
 AND ra.runner_id=p.runner_id AND r.enrollment_id=p.enrollment_id AND r.machine_id=p.machine_id
) RETURNING p.*;
