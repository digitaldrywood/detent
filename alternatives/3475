-- name: OperatorWorkerProcess :one
SELECT * FROM runner_process_receipts WHERE organization_id=$1 AND project_id=$2 AND session_id=$3 AND role=$4;

-- name: OperatorWorkerProcesses :many
SELECT p.*,s.issue_id,s.attempt_id,s.final_state,s.completed_at, (s.completed_at IS NOT NULL OR a.status<>'running' OR l.released_at IS NOT NULL OR l.expires_at<=sqlc.arg(now)::timestamptz) AS safe_to_reap
FROM runner_process_receipts p JOIN runner_sessions s ON s.organization_id=p.organization_id AND s.project_id=p.project_id AND s.id=p.session_id
JOIN native_attempts a ON a.organization_id=s.organization_id AND a.id=s.attempt_id JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id) AND p.runner_id=sqlc.arg(runner_id) AND p.enrollment_id=sqlc.arg(enrollment_id) AND p.machine_id=sqlc.arg(machine_id) AND p.reaped_at IS NULL AND (p.session_id,p.role)>(sqlc.arg(after_session)::bigint,sqlc.arg(after_role)::text)
ORDER BY p.session_id,p.role LIMIT sqlc.arg(page_limit);

-- name: ValidateWorkerProcessSource :one
SELECT s.id,s.worker_pid,s.worker_group_id,s.worker_started_at,s.completed_at,l.released_at,l.expires_at,a.status,r.enrollment_id,l.machine_id,ra.runner_id,l.lease_id,l.fencing_token
FROM runner_sessions s JOIN runner_session_history h ON h.organization_id=s.organization_id AND h.session_id=s.id AND h.revision=sqlc.arg(history_revision)
JOIN native_attempts a ON a.organization_id=s.organization_id AND a.id=s.attempt_id JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
JOIN runner_lease_assignments ra ON ra.organization_id=l.organization_id AND ra.lease_id=l.lease_id JOIN runner_identities r ON r.organization_id=ra.organization_id AND r.runner_id=ra.runner_id
WHERE s.organization_id=sqlc.arg(organization_id) AND s.project_id=sqlc.arg(project_id) AND s.id=sqlc.arg(session_id) AND h.payload_object_key=sqlc.arg(registration_object_key) AND h.payload_sha256=sqlc.arg(registration_sha256) AND h.payload_bytes=sqlc.arg(registration_bytes) AND h.payload_content_bytes=sqlc.arg(registration_content_bytes) FOR UPDATE OF s;

-- name: RegisterWorkerProcess :one
INSERT INTO runner_process_receipts(organization_id,project_id,session_id,role,runner_id,enrollment_id,machine_id,lease_id,fencing_token,authority_hash,pid,group_id,started_at,started_at_ns,cleanup_root,cleanup_path,history_revision,registration_object_key,registration_sha256,registration_bytes,registration_content_bytes,updated_at)
VALUES(sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(session_id),sqlc.arg(role),sqlc.arg(runner_id),sqlc.arg(enrollment_id),sqlc.arg(machine_id),sqlc.arg(lease_id),sqlc.arg(fencing_token),sqlc.arg(authority_hash),sqlc.arg(pid),sqlc.arg(group_id),sqlc.arg(started_at),sqlc.arg(started_at_ns),sqlc.arg(cleanup_root),sqlc.arg(cleanup_path),sqlc.arg(history_revision),sqlc.arg(registration_object_key),sqlc.arg(registration_sha256),sqlc.arg(registration_bytes),sqlc.arg(registration_content_bytes),sqlc.arg(updated_at))
ON CONFLICT(organization_id,project_id,session_id,role) DO UPDATE SET runner_id=EXCLUDED.runner_id,enrollment_id=EXCLUDED.enrollment_id,machine_id=EXCLUDED.machine_id,lease_id=EXCLUDED.lease_id,fencing_token=EXCLUDED.fencing_token,owner_generation=runner_process_receipts.owner_generation+1,authority_hash=EXCLUDED.authority_hash,pid=EXCLUDED.pid,group_id=EXCLUDED.group_id,started_at=EXCLUDED.started_at,started_at_ns=EXCLUDED.started_at_ns,cleanup_root=EXCLUDED.cleanup_root,cleanup_path=EXCLUDED.cleanup_path,history_revision=EXCLUDED.history_revision,registration_object_key=EXCLUDED.registration_object_key,registration_sha256=EXCLUDED.registration_sha256,registration_bytes=EXCLUDED.registration_bytes,registration_content_bytes=EXCLUDED.registration_content_bytes,reap_class='registered',reaped_at=NULL,reap_outcome='',reap_reason='',reap_object_key=NULL,reap_sha256='',reap_bytes=0,reap_content_bytes=0,updated_at=EXCLUDED.updated_at
WHERE runner_process_receipts.reaped_at IS NOT NULL RETURNING *;

-- name: MarkWorkerProcessReaped :one
UPDATE runner_process_receipts p SET reap_class=sqlc.arg(reap_class),reaped_at=CASE WHEN sqlc.arg(reap_class)::text='committed' THEN sqlc.arg(at)::timestamptz ELSE NULL END,reap_outcome=sqlc.arg(reap_outcome),reap_reason=sqlc.arg(reap_reason),reap_object_key=sqlc.narg(reap_object_key),reap_sha256=sqlc.arg(reap_sha256),reap_bytes=sqlc.arg(reap_bytes),reap_content_bytes=sqlc.arg(reap_content_bytes),updated_at=sqlc.arg(at)
WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id) AND p.session_id=sqlc.arg(session_id) AND p.role=sqlc.arg(role) AND p.owner_generation=sqlc.arg(owner_generation) AND p.authority_hash=sqlc.arg(authority_hash) AND p.runner_id=sqlc.arg(runner_id) AND p.enrollment_id=sqlc.arg(enrollment_id) AND p.machine_id=sqlc.arg(machine_id) AND p.lease_id=sqlc.arg(lease_id) AND p.fencing_token=sqlc.arg(fencing_token) AND p.pid=sqlc.arg(pid) AND p.group_id=sqlc.arg(group_id) AND p.started_at=sqlc.arg(started_at) AND p.started_at_ns=sqlc.arg(started_at_ns) AND p.reaped_at IS NULL
AND EXISTS(SELECT 1 FROM runner_sessions s JOIN native_attempts a ON a.organization_id=s.organization_id AND a.id=s.attempt_id JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id WHERE s.organization_id=p.organization_id AND s.project_id=p.project_id AND s.id=p.session_id AND l.lease_id=p.lease_id AND l.fencing_token=p.fencing_token AND (s.completed_at IS NOT NULL OR a.status<>'running' OR l.released_at IS NOT NULL OR l.expires_at<=sqlc.arg(at))) RETURNING p.*;

-- name: ProjectWorkerProcessRegistration :execrows
UPDATE runner_sessions SET worker_pid=$4,worker_group_id=$5,worker_started_at=$6,worker_reaped_at=NULL
WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND (worker_pid=0 OR worker_reaped_at IS NOT NULL OR worker_pid=$4 AND worker_group_id=$5 AND worker_started_at=$6);

-- name: ProjectWorkerProcessReap :execrows
UPDATE runner_sessions SET worker_reaped_at=$7 WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND worker_pid=$4 AND worker_group_id=$5 AND worker_started_at=$6;
