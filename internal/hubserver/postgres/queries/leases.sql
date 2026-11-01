-- name: RegisterLeaseMachine :exec
INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,clock_timestamp(),clock_timestamp(),clock_timestamp())
ON CONFLICT (organization_id,id) DO UPDATE SET hostname=EXCLUDED.hostname,display_name=EXCLUDED.display_name,capacity=EXCLUDED.capacity,version=EXCLUDED.version,updated_at=clock_timestamp();

-- name: LockClaimIssue :one
SELECT i.revision,i.archived FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE i.organization_id=$1 AND i.id=$2 AND p.deleted_at IS NULL FOR SHARE OF p FOR NO KEY UPDATE OF i;

-- name: LockActiveLease :one
SELECT * FROM leases WHERE organization_id=$1 AND issue_id=$2 AND released_at IS NULL FOR UPDATE;

-- name: LockLease :one
SELECT * FROM leases WHERE organization_id=$1 AND lease_id=$2 FOR UPDATE;

-- name: LeaseTime :one
SELECT clock_timestamp()::timestamptz AS now;

-- name: InsertLease :one
INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,sqlc.arg(now),sqlc.arg(now),sqlc.arg(expires_at)) RETURNING *;

-- name: RenewLease :one
UPDATE leases SET renewed_at=sqlc.arg(now),expires_at=sqlc.arg(expires_at),
landing_capacity=CASE WHEN sqlc.arg(landing_capacity)::bigint>0 THEN sqlc.arg(landing_capacity)::bigint ELSE landing_capacity END
WHERE organization_id=sqlc.arg(organization_id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND released_at IS NULL AND expires_at>sqlc.arg(now)
RETURNING *;

-- name: ReleaseLease :one
UPDATE leases SET released_at=sqlc.arg(now)::timestamptz,release_reason=sqlc.arg(reason)
WHERE organization_id=sqlc.arg(organization_id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND released_at IS NULL AND expires_at>sqlc.arg(now)
RETURNING *;

-- name: ExpireLease :one
UPDATE leases SET released_at=sqlc.arg(now)::timestamptz,release_reason='expired'
WHERE organization_id=sqlc.arg(organization_id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND released_at IS NULL AND expires_at<=sqlc.arg(now)
RETURNING *;

-- name: CancelLeaseAttempt :exec
UPDATE native_attempts SET status='cancelled',sequence=sequence+1,updated_at=$4
WHERE organization_id=$1 AND lease_id=$2 AND fencing_token=$3 AND status='running';

-- name: HeartbeatLeaseMachine :exec
UPDATE machines SET last_heartbeat_at=greatest(last_heartbeat_at,$3),updated_at=greatest(updated_at,$3) WHERE organization_id=$1 AND id=$2;

-- name: GetLease :one
SELECT * FROM leases WHERE organization_id=$1 AND lease_id=$2;

-- name: GetActiveIssueLease :one
SELECT * FROM leases
WHERE organization_id=$1 AND issue_id=$2 AND released_at IS NULL AND expires_at>clock_timestamp();

-- name: LeaseUsage :one
SELECT count(*) FROM leases WHERE organization_id=$1 AND machine_id=$2 AND released_at IS NULL AND expires_at>sqlc.arg(now)::timestamptz;

-- name: GetLeaseAttempt :one
SELECT * FROM native_attempts WHERE organization_id=$1 AND id=$2;

-- name: GetLatestIssueAttempt :one
SELECT * FROM native_attempts WHERE organization_id=$1 AND issue_id=$2 ORDER BY fencing_token DESC LIMIT 1;

-- name: PageIssueAttempts :many
SELECT * FROM native_attempts
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id)
AND (sqlc.arg(before_fence)::bigint=0 OR fencing_token<sqlc.arg(before_fence)::bigint)
ORDER BY fencing_token DESC LIMIT sqlc.arg(page_limit);

-- name: PageIssueAttemptsOldest :many
SELECT * FROM native_attempts
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id)
AND (sqlc.arg(after_fence)::bigint=0 OR fencing_token>sqlc.arg(after_fence)::bigint)
ORDER BY fencing_token LIMIT sqlc.arg(page_limit);

-- name: GetAttemptReceipt :one
SELECT request_hash FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence=$3;

-- name: InsertLeaseAttempt :one
INSERT INTO native_attempts (organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,artifacts_object_key,started_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12,sqlc.arg(now),sqlc.arg(now)) RETURNING *;

-- name: AdvanceLeaseAttempt :one
UPDATE native_attempts SET sequence=sqlc.arg(next_sequence),status=sqlc.arg(status),payload_object_key=sqlc.arg(payload_object_key),
checkpoint_object_key=COALESCE(sqlc.narg(checkpoint_object_key),checkpoint_object_key),
artifacts_object_key=COALESCE(sqlc.narg(artifacts_object_key),artifacts_object_key),updated_at=sqlc.arg(now)
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND run_id=sqlc.arg(run_id)
AND dispatch_generation=sqlc.arg(dispatch_generation) AND sequence=sqlc.arg(next_sequence)-1 AND status='running'
RETURNING *;

-- name: InsertAttemptReceipt :exec
INSERT INTO native_attempt_events (organization_id,attempt_id,sequence,request_hash) VALUES ($1,$2,$3,$4);

-- name: BindAttemptRun :exec
INSERT INTO native_attempt_runs (organization_id,run_id,issue_id) VALUES ($1,$2,$3) ON CONFLICT (organization_id,run_id) DO NOTHING;
