-- name: RegisterLeaseMachine :exec
INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,sprite_name,last_heartbeat_at,registered_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,clock_timestamp(),clock_timestamp(),clock_timestamp())
ON CONFLICT (organization_id,id) DO UPDATE SET hostname=EXCLUDED.hostname,display_name=EXCLUDED.display_name,capacity=EXCLUDED.capacity,version=EXCLUDED.version,sprite_name=EXCLUDED.sprite_name,updated_at=clock_timestamp();

-- name: LockClaimIssue :one
SELECT i.revision,i.archived,i.project_id FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE i.organization_id=$1 AND i.id=$2 AND p.deleted_at IS NULL FOR SHARE OF p FOR NO KEY UPDATE OF i;

-- name: LockActiveLease :one
SELECT * FROM leases WHERE organization_id=$1 AND issue_id=$2 AND released_at IS NULL FOR UPDATE;

-- name: LockAttemptIssue :one
SELECT id FROM issues WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE;

-- name: LockAttemptProject :one
SELECT p.id FROM projects p JOIN issues i ON i.organization_id=p.organization_id AND i.project_id=p.id WHERE i.organization_id=$1 AND i.id=$2 FOR SHARE OF p;

-- name: LockLease :one
SELECT * FROM leases WHERE organization_id=$1 AND lease_id=$2 FOR UPDATE;

-- name: LeaseTime :one
SELECT clock_timestamp()::timestamptz AS now;

-- name: InsertLease :one
WITH lease_created AS (
 INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,landing_capacity,acquired_at,renewed_at,expires_at,project_id,runner_id,isolation_tier)
 VALUES ($1,$2,$3,$4,$5,$6,sqlc.arg(landing_capacity),sqlc.arg(now),sqlc.arg(now),sqlc.arg(expires_at),sqlc.arg(project_id),NULLIF(sqlc.arg(runner_id)::text,''),sqlc.narg(isolation_tier)::text) RETURNING *
), assigned AS (
 INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at)
 SELECT organization_id,lease_id,runner_id,acquired_at FROM lease_created WHERE runner_id IS NOT NULL
)
SELECT lease_created.organization_id, lease_created.lease_id, lease_created.fencing_token, lease_created.issue_id, lease_created.machine_id, lease_created.session_id, lease_created.work_item_revision, lease_created.landing_capacity, lease_created.acquired_at, lease_created.renewed_at, lease_created.expires_at, lease_created.released_at, lease_created.release_reason, lease_created.project_id AS lease_project_id, lease_created.runner_id, lease_created.isolation_tier FROM lease_created;

-- name: UpdateLeaseLandingCapacity :one
UPDATE leases SET landing_capacity=CASE WHEN landing_capacity=0 THEN sqlc.arg(landing_capacity)::bigint WHEN sqlc.arg(landing_capacity)::bigint>0 THEN LEAST(landing_capacity,sqlc.arg(landing_capacity)::bigint) ELSE landing_capacity END
WHERE organization_id=sqlc.arg(organization_id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND released_at IS NULL
RETURNING *;

-- name: LockLeaseExpiryScope :one
WITH locked_project AS MATERIALIZED (
 SELECT p.organization_id,p.id FROM leases l
 JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id) AND l.fencing_token=sqlc.arg(fencing_token)
 FOR SHARE OF p
)
SELECT l.* FROM leases l
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
JOIN locked_project p ON p.organization_id=i.organization_id AND p.id=i.project_id
WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id) AND l.fencing_token=sqlc.arg(fencing_token)
FOR UPDATE OF i;

-- name: ExpireLease :one
UPDATE leases AS l SET released_at=sqlc.arg(now)::timestamptz,release_reason='expired'
WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id)
AND l.fencing_token=sqlc.arg(fencing_token)::bigint AND l.released_at IS NULL AND l.expires_at<=sqlc.arg(now)
AND NOT EXISTS (
 SELECT 1 FROM native_attempts a WHERE a.organization_id=l.organization_id AND a.lease_id=l.lease_id
 AND a.fencing_token=l.fencing_token AND a.status='running'
)
RETURNING l.*;

-- name: HeartbeatLeaseMachine :exec
UPDATE machines SET last_heartbeat_at=greatest(last_heartbeat_at,$3),updated_at=greatest(updated_at,$3) WHERE organization_id=$1 AND id=$2;

-- name: GetLease :one
SELECT * FROM leases WHERE organization_id=$1 AND lease_id=$2;

-- name: GetActiveIssueLease :one
SELECT * FROM leases
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id) AND released_at IS NULL AND expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp());

-- name: LeaseUsage :one
SELECT count(*) FROM leases WHERE organization_id=$1 AND machine_id=$2 AND released_at IS NULL AND expires_at>sqlc.arg(now)::timestamptz;

-- name: GetLeaseAttempt :one
SELECT * FROM native_attempts WHERE organization_id=$1 AND id=$2;

-- name: GetLatestIssueAttempt :one
SELECT * FROM native_attempts WHERE organization_id=$1 AND issue_id=$2 ORDER BY fencing_token DESC LIMIT 1;

-- name: GetLeaseLossCandidate :one
WITH unreleased AS MATERIALIZED (
 SELECT fencing_token FROM leases
 WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id) AND released_at IS NULL
)
SELECT a.* FROM native_attempts a
JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id AND l.fencing_token=a.fencing_token
WHERE a.organization_id=sqlc.arg(organization_id) AND a.issue_id=sqlc.arg(issue_id)
AND a.fencing_token=COALESCE(
 (SELECT fencing_token FROM unreleased),
 CASE WHEN sqlc.arg(include_released)::boolean THEN (
  SELECT latest.fencing_token FROM native_attempts latest
  WHERE latest.organization_id=sqlc.arg(organization_id) AND latest.issue_id=sqlc.arg(issue_id)
  ORDER BY latest.fencing_token DESC LIMIT 1
 ) END
)
AND a.status='running' AND (l.released_at IS NOT NULL OR l.expires_at<=COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()));

-- name: GetLatestIssueAttemptsByIssues :many
SELECT latest.*
FROM unnest(sqlc.arg(issue_ids)::bigint[]) AS requested(issue_id)
CROSS JOIN LATERAL (
  SELECT * FROM native_attempts
  WHERE organization_id=sqlc.arg(organization_id) AND issue_id=requested.issue_id
  ORDER BY fencing_token DESC
  LIMIT 1
) latest;

-- name: PageIssueAttempts :many
SELECT * FROM native_attempts
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id)
AND (sqlc.arg(before_fence)::bigint=0 OR fencing_token<sqlc.arg(before_fence)::bigint)
ORDER BY fencing_token DESC LIMIT sqlc.arg(page_limit);

-- name: PageIssueAttemptFailures :many
SELECT id,status,sequence,failure_sequence,failure_example,failure_class,failure_landing_observed_at,started_at,started_at_submicro_ns,updated_at
FROM native_attempts
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id)
ORDER BY fencing_token DESC LIMIT sqlc.arg(page_limit);

-- name: SetAttemptFailure :execrows
UPDATE native_attempts SET failure_sequence=sequence,failure_example=sqlc.narg(failure_example)::text,failure_class=sqlc.narg(failure_class)::text,failure_landing_observed_at=sqlc.narg(failure_landing_observed_at)::timestamptz
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND issue_id=sqlc.arg(issue_id) AND sequence=sqlc.arg(sequence)
AND failure_sequence IS DISTINCT FROM sequence;

-- name: PageIssueCheckpointAttempts :many
SELECT * FROM native_attempts
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id)
AND checkpoint_object_key IS NOT NULL
AND (sqlc.arg(before_fence)::bigint=0 OR fencing_token<sqlc.arg(before_fence)::bigint)
ORDER BY fencing_token DESC LIMIT sqlc.arg(page_limit);

-- name: PageIssueAttemptsOldest :many
SELECT * FROM native_attempts
WHERE organization_id=sqlc.arg(organization_id) AND issue_id=sqlc.arg(issue_id)
AND (sqlc.arg(after_fence)::bigint=0 OR fencing_token>sqlc.arg(after_fence)::bigint)
AND (sqlc.arg(through_fence)::bigint=0 OR fencing_token<=sqlc.arg(through_fence)::bigint)
ORDER BY fencing_token LIMIT sqlc.arg(page_limit);

-- name: GetAttemptReceipt :one
SELECT request_hash FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence=$3;

-- name: InsertLeaseAttempt :one
INSERT INTO native_attempts (organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,artifacts_object_key,started_at,started_at_submicro_ns,updated_at,runtime_heartbeat_at,runtime_summary_sequence,worker_cleanup_required,local_attempt_id,local_attempt_id_recorded,checkpoint_preserves_source,project_id,failure_sequence,failure_example,failure_class,failure_landing_observed_at,
identity_sequence,identity_execution_role,identity_execution_backend,identity_execution_model,identity_execution_reasoning_effort,identity_selection_effort_clamped_from,identity_selection_policy,identity_selection_policy_source,identity_selection_reason,identity_selection_level,identity_selection_requested_model,identity_selection_model_source,identity_selection_effort_source,identity_selection_fallback_reason,identity_selection_backend_source,identity_selection_route_source,identity_backend_id,identity_backend_kind,identity_route,identity_role,identity_provider,identity_provider_provenance,identity_requested_model,identity_requested_model_provenance,identity_resolved_model,identity_resolved_model_provenance,identity_reasoning_effort,identity_reasoning_effort_provenance,identity_service_tier,identity_service_tier_provenance,identity_observed_at,worker_pid,worker_pgid,worker_started_at,worker_start_key,worker_runner_id)
VALUES ($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12,sqlc.arg(started_at),sqlc.arg(started_at_submicro_ns),sqlc.arg(now),sqlc.narg(runtime_heartbeat_at),1,sqlc.narg(worker_cleanup_required)::boolean,NULLIF(sqlc.arg(local_attempt_id)::bigint,0),true,sqlc.narg(checkpoint_preserves_source)::boolean,(SELECT i.project_id FROM work_item_identities i WHERE i.organization_id=$1 AND i.id=$3),
CASE WHEN sqlc.arg(failure_recorded)::boolean THEN 1 END,sqlc.narg(failure_example)::text,sqlc.narg(failure_class)::text,sqlc.narg(failure_landing_observed_at)::timestamptz,
CASE WHEN sqlc.narg(identity_execution_role)::text IS NOT NULL THEN 1 END,sqlc.narg(identity_execution_role)::text,sqlc.narg(identity_execution_backend)::text,sqlc.narg(identity_execution_model)::text,sqlc.narg(identity_execution_reasoning_effort)::text,sqlc.narg(identity_selection_effort_clamped_from)::text,sqlc.narg(identity_selection_policy)::text,sqlc.narg(identity_selection_policy_source)::text,sqlc.narg(identity_selection_reason)::text,sqlc.narg(identity_selection_level)::text,sqlc.narg(identity_selection_requested_model)::text,sqlc.narg(identity_selection_model_source)::text,sqlc.narg(identity_selection_effort_source)::text,sqlc.narg(identity_selection_fallback_reason)::text,sqlc.narg(identity_selection_backend_source)::text,sqlc.narg(identity_selection_route_source)::text,sqlc.narg(identity_backend_id)::text,sqlc.narg(identity_backend_kind)::text,sqlc.narg(identity_route)::text,sqlc.narg(identity_role)::text,sqlc.narg(identity_provider)::text,sqlc.narg(identity_provider_provenance)::text,sqlc.narg(identity_requested_model)::text,sqlc.narg(identity_requested_model_provenance)::text,sqlc.narg(identity_resolved_model)::text,sqlc.narg(identity_resolved_model_provenance)::text,sqlc.narg(identity_reasoning_effort)::text,sqlc.narg(identity_reasoning_effort_provenance)::text,sqlc.narg(identity_service_tier)::text,sqlc.narg(identity_service_tier_provenance)::text,sqlc.narg(identity_observed_at)::timestamptz,sqlc.narg(worker_pid)::integer,sqlc.narg(worker_pgid)::integer,sqlc.narg(worker_started_at)::timestamptz,sqlc.narg(worker_start_key)::text,
CASE WHEN sqlc.narg(worker_pid)::integer IS NOT NULL THEN (SELECT a.runner_id FROM runner_lease_assignments a WHERE a.organization_id=$1 AND a.lease_id=$4) END) RETURNING *;

-- name: AdvanceLeaseAttempt :one
UPDATE native_attempts SET sequence=sqlc.arg(next_sequence),
local_attempt_id=CASE WHEN local_attempt_id_recorded THEN COALESCE(local_attempt_id,NULLIF(sqlc.arg(local_attempt_id)::bigint,0)) ELSE local_attempt_id END,
runtime_heartbeat_at=sqlc.narg(runtime_heartbeat_at),runtime_summary_sequence=sqlc.arg(next_sequence),worker_cleanup_required=sqlc.narg(worker_cleanup_required)::boolean,
worker_pid=COALESCE(sqlc.narg(worker_pid)::integer,worker_pid),
worker_pgid=COALESCE(sqlc.narg(worker_pgid)::integer,worker_pgid),
worker_started_at=COALESCE(sqlc.narg(worker_started_at)::timestamptz,worker_started_at),
worker_start_key=COALESCE(sqlc.narg(worker_start_key)::text,worker_start_key),
worker_runner_id=CASE WHEN sqlc.narg(worker_pid)::integer IS NOT NULL AND worker_runner_id IS NULL
 THEN (SELECT a.runner_id FROM runner_lease_assignments a WHERE a.organization_id=native_attempts.organization_id AND a.lease_id=native_attempts.lease_id)
 ELSE worker_runner_id END,
cleanup_acknowledged_at=CASE
 WHEN sqlc.narg(worker_pid)::integer IS NOT NULL AND ROW(worker_pid,worker_pgid,worker_started_at,worker_start_key) IS DISTINCT FROM
      ROW(sqlc.narg(worker_pid)::integer,sqlc.narg(worker_pgid)::integer,sqlc.narg(worker_started_at)::timestamptz,sqlc.narg(worker_start_key)::text) THEN NULL
 WHEN sqlc.arg(termination)::boolean AND worker_pid IS NULL THEN sqlc.arg(now)
 ELSE cleanup_acknowledged_at END,
status=CASE WHEN sqlc.arg(usage_correction)::boolean THEN status ELSE sqlc.arg(status) END,
payload_object_key=sqlc.arg(payload_object_key),
checkpoint_object_key=COALESCE(sqlc.narg(checkpoint_object_key),checkpoint_object_key),
checkpoint_preserves_source=CASE WHEN sqlc.narg(checkpoint_object_key)::text IS NOT NULL THEN sqlc.narg(checkpoint_preserves_source)::boolean ELSE checkpoint_preserves_source END,
artifacts_object_key=COALESCE(sqlc.narg(artifacts_object_key),artifacts_object_key),
failure_sequence=CASE WHEN sqlc.arg(usage_correction)::boolean THEN CASE WHEN failure_sequence=sequence THEN sqlc.arg(next_sequence) END WHEN sqlc.arg(failure_recorded)::boolean THEN sqlc.arg(next_sequence) END,
failure_example=CASE WHEN sqlc.arg(usage_correction)::boolean THEN CASE WHEN failure_sequence=sequence THEN failure_example END ELSE sqlc.narg(failure_example)::text END,
failure_class=CASE WHEN sqlc.arg(usage_correction)::boolean THEN CASE WHEN failure_sequence=sequence THEN failure_class END ELSE sqlc.narg(failure_class)::text END,
failure_landing_observed_at=CASE WHEN sqlc.arg(usage_correction)::boolean THEN CASE WHEN failure_sequence=sequence THEN failure_landing_observed_at END ELSE sqlc.narg(failure_landing_observed_at)::timestamptz END,
identity_sequence=CASE WHEN identity_sequence<sqlc.arg(next_sequence) THEN sqlc.arg(next_sequence) ELSE identity_sequence END,
identity_execution_role=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_execution_role)::text IS NOT NULL THEN sqlc.narg(identity_execution_role)::text ELSE identity_execution_role END,
identity_execution_backend=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_execution_role)::text IS NOT NULL THEN sqlc.narg(identity_execution_backend)::text ELSE identity_execution_backend END,
identity_execution_model=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_execution_role)::text IS NOT NULL THEN sqlc.narg(identity_execution_model)::text ELSE identity_execution_model END,
identity_execution_reasoning_effort=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_execution_role)::text IS NOT NULL THEN sqlc.narg(identity_execution_reasoning_effort)::text ELSE identity_execution_reasoning_effort END,
identity_selection_effort_clamped_from=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_effort_clamped_from)::text ELSE identity_selection_effort_clamped_from END,
identity_selection_policy=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_policy)::text ELSE identity_selection_policy END,
identity_selection_policy_source=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_policy_source)::text ELSE identity_selection_policy_source END,
identity_selection_reason=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_reason)::text ELSE identity_selection_reason END,
identity_selection_level=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_level)::text ELSE identity_selection_level END,
identity_selection_requested_model=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_requested_model)::text ELSE identity_selection_requested_model END,
identity_selection_model_source=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_model_source)::text ELSE identity_selection_model_source END,
identity_selection_effort_source=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_effort_source)::text ELSE identity_selection_effort_source END,
identity_selection_fallback_reason=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_fallback_reason)::text ELSE identity_selection_fallback_reason END,
identity_selection_backend_source=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_backend_source)::text ELSE identity_selection_backend_source END,
identity_selection_route_source=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.arg(identity_selection_present)::boolean THEN sqlc.narg(identity_selection_route_source)::text ELSE identity_selection_route_source END,
identity_backend_id=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_backend_id)::text IS NOT NULL THEN sqlc.narg(identity_backend_id)::text ELSE identity_backend_id END,
identity_backend_kind=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_backend_kind)::text IS NOT NULL THEN sqlc.narg(identity_backend_kind)::text ELSE identity_backend_kind END,
identity_route=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_route)::text IS NOT NULL THEN sqlc.narg(identity_route)::text ELSE identity_route END,
identity_role=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_role)::text IS NOT NULL THEN sqlc.narg(identity_role)::text ELSE identity_role END,
identity_provider=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_provider_provenance)::text IS NOT NULL THEN sqlc.narg(identity_provider)::text ELSE identity_provider END,
identity_provider_provenance=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_provider_provenance)::text IS NOT NULL THEN sqlc.narg(identity_provider_provenance)::text ELSE identity_provider_provenance END,
identity_requested_model=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_requested_model_provenance)::text IS NOT NULL THEN sqlc.narg(identity_requested_model)::text ELSE identity_requested_model END,
identity_requested_model_provenance=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_requested_model_provenance)::text IS NOT NULL THEN sqlc.narg(identity_requested_model_provenance)::text ELSE identity_requested_model_provenance END,
identity_resolved_model=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_resolved_model_provenance)::text IS NOT NULL THEN sqlc.narg(identity_resolved_model)::text ELSE identity_resolved_model END,
identity_resolved_model_provenance=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_resolved_model_provenance)::text IS NOT NULL THEN sqlc.narg(identity_resolved_model_provenance)::text ELSE identity_resolved_model_provenance END,
identity_reasoning_effort=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_reasoning_effort_provenance)::text IS NOT NULL THEN sqlc.narg(identity_reasoning_effort)::text ELSE identity_reasoning_effort END,
identity_reasoning_effort_provenance=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_reasoning_effort_provenance)::text IS NOT NULL THEN sqlc.narg(identity_reasoning_effort_provenance)::text ELSE identity_reasoning_effort_provenance END,
identity_service_tier=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_service_tier_provenance)::text IS NOT NULL THEN sqlc.narg(identity_service_tier)::text ELSE identity_service_tier END,
identity_service_tier_provenance=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_service_tier_provenance)::text IS NOT NULL THEN sqlc.narg(identity_service_tier_provenance)::text ELSE identity_service_tier_provenance END,
identity_observed_at=CASE WHEN identity_sequence<sqlc.arg(next_sequence) AND sqlc.narg(identity_observed_at)::timestamptz IS NOT NULL THEN sqlc.narg(identity_observed_at)::timestamptz ELSE identity_observed_at END,
updated_at=CASE WHEN sqlc.arg(usage_correction)::boolean THEN updated_at ELSE sqlc.arg(now) END
WHERE native_attempts.organization_id=sqlc.arg(organization_id) AND native_attempts.id=sqlc.arg(id) AND native_attempts.lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND run_id=sqlc.arg(run_id)
AND (sqlc.arg(local_attempt_id)::bigint=0 OR local_attempt_id IS NULL OR local_attempt_id=sqlc.arg(local_attempt_id))
AND dispatch_generation=sqlc.arg(dispatch_generation) AND sequence=sqlc.arg(next_sequence)-1
AND (sqlc.narg(worker_pid)::integer IS NULL OR worker_pid IS NULL OR cleanup_acknowledged_at IS NOT NULL
 OR ROW(worker_pid,worker_pgid,worker_started_at,worker_start_key) IS NOT DISTINCT FROM
    ROW(sqlc.narg(worker_pid)::integer,sqlc.narg(worker_pgid)::integer,sqlc.narg(worker_started_at)::timestamptz,sqlc.narg(worker_start_key)::text))
AND (status='running' OR status='interrupted' AND sqlc.arg(status)='running' OR sqlc.arg(usage_correction)::boolean AND status=sqlc.arg(status) OR sqlc.arg(termination)::boolean AND status IN ('interrupted','cancelled') AND sqlc.arg(status)='cancelled')
AND (NOT sqlc.arg(runtime_ordered)::boolean OR CASE WHEN sqlc.arg(usage_correction)::boolean THEN status ELSE sqlc.arg(status) END NOT IN ('running','interrupted') OR runtime_heartbeat_at IS NULL OR sqlc.narg(runtime_heartbeat_at)>=runtime_heartbeat_at)
RETURNING *;

-- name: InsertAttemptReceipt :exec
INSERT INTO native_attempt_events (organization_id,attempt_id,sequence,request_hash) VALUES ($1,$2,$3,$4);

-- name: BindAttemptRun :exec
INSERT INTO native_attempt_runs (organization_id,run_id,issue_id) VALUES ($1,$2,$3) ON CONFLICT (organization_id,run_id) DO NOTHING;

-- name: RegisterOwnedMachine :execrows
INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at,registration_token_id)
VALUES ($1,$2,$3,$4,$5,$6,clock_timestamp(),clock_timestamp(),clock_timestamp(),$7)
ON CONFLICT (organization_id,id) DO UPDATE SET hostname=EXCLUDED.hostname,display_name=EXCLUDED.display_name,capacity=EXCLUDED.capacity,version=EXCLUDED.version,last_heartbeat_at=greatest(machines.last_heartbeat_at,EXCLUDED.last_heartbeat_at),updated_at=greatest(machines.updated_at,EXCLUDED.updated_at)
WHERE machines.registration_token_id=EXCLUDED.registration_token_id;

-- name: LockRunnerHostRevision :one
SELECT routing_revision FROM machines WHERE organization_id=$1 AND id=$2 FOR UPDATE;

-- name: UpdateRunnerHostRouting :one
UPDATE machines SET display_name=sqlc.arg(display_name),capacity=sqlc.arg(capacity)::bigint,routing_revision=routing_revision+1,updated_at=clock_timestamp()
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND routing_revision=sqlc.arg(expected_revision)::bigint
RETURNING routing_revision;

-- name: MutateLeaseCommand :one
WITH lease_locked AS MATERIALIZED (
 SELECT l.* FROM leases l
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id)
 FOR UPDATE OF l
), at AS MATERIALIZED (
 SELECT COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) AS now FROM lease_locked
), lease_changed AS (
 UPDATE leases l SET
 renewed_at=CASE WHEN sqlc.arg(operation)::text='renew' THEN at.now ELSE l.renewed_at END,
 expires_at=CASE WHEN sqlc.arg(operation)::text='renew' THEN at.now+sqlc.arg(ttl_us)::bigint*interval '1 microsecond' ELSE l.expires_at END,
 landing_capacity=CASE WHEN sqlc.arg(operation)::text='renew' AND sqlc.arg(landing_capacity)::bigint>0 THEN CASE WHEN l.landing_capacity=0 THEN sqlc.arg(landing_capacity)::bigint ELSE LEAST(l.landing_capacity,sqlc.arg(landing_capacity)::bigint) END ELSE l.landing_capacity END,
 released_at=CASE WHEN sqlc.arg(operation)::text='renew' THEN l.released_at ELSE at.now END,
 release_reason=CASE WHEN sqlc.arg(operation)::text='renew' THEN l.release_reason WHEN sqlc.arg(operation)::text='expire' THEN 'expired' ELSE sqlc.arg(reason)::text END
 FROM lease_locked,at
 WHERE l.organization_id=lease_locked.organization_id AND l.lease_id=lease_locked.lease_id
 AND lease_locked.fencing_token=sqlc.arg(fencing_token)::bigint AND lease_locked.released_at IS NULL
 AND CASE WHEN sqlc.arg(operation)::text='expire' THEN lease_locked.expires_at<=at.now ELSE lease_locked.expires_at>at.now OR sqlc.arg(operation)::text='renew' AND sqlc.arg(landing_capacity)::bigint>0 END
 AND (sqlc.arg(operation)::text<>'expire' OR NOT EXISTS (
  SELECT 1 FROM native_attempts a WHERE a.organization_id=lease_locked.organization_id AND a.lease_id=lease_locked.lease_id
  AND a.fencing_token=lease_locked.fencing_token AND a.status='running'
 ))
 AND (sqlc.arg(operation)::text<>'renew' OR NOT EXISTS (
  SELECT 1 FROM issues i
  JOIN project_workflow_states s ON s.organization_id=i.organization_id AND s.project_id=i.project_id AND s.name=i.workflow_state_name
  WHERE i.organization_id=lease_locked.organization_id AND i.id=lease_locked.issue_id AND s.terminal
 ) OR EXISTS (
  SELECT 1 FROM native_attempts a
  JOIN change_landing_receipts r ON r.organization_id=a.organization_id AND r.attempt_id=a.id
  WHERE a.organization_id=lease_locked.organization_id AND a.issue_id=lease_locked.issue_id
  AND a.lease_id=lease_locked.lease_id AND a.fencing_token=lease_locked.fencing_token AND a.status='running'
 ))
 RETURNING l.*
), heartbeat AS (
 UPDATE machines m SET last_heartbeat_at=greatest(m.last_heartbeat_at,at.now),updated_at=greatest(m.updated_at,at.now)
 FROM lease_changed c,at WHERE m.organization_id=c.organization_id AND m.id=c.machine_id AND sqlc.arg(operation)::text<>'expire'
)
SELECT lease_changed.organization_id,lease_changed.lease_id,lease_changed.fencing_token,lease_changed.issue_id,lease_changed.machine_id,lease_changed.session_id,lease_changed.work_item_revision,lease_changed.landing_capacity,lease_changed.acquired_at,lease_changed.renewed_at,lease_changed.expires_at,lease_changed.released_at,lease_changed.release_reason,lease_changed.project_id AS lease_project_id,lease_changed.runner_id,lease_changed.isolation_tier,true AS applied FROM lease_changed
UNION ALL SELECT lease_locked.organization_id,lease_locked.lease_id,lease_locked.fencing_token,lease_locked.issue_id,lease_locked.machine_id,lease_locked.session_id,lease_locked.work_item_revision,lease_locked.landing_capacity,lease_locked.acquired_at,lease_locked.renewed_at,lease_locked.expires_at,lease_locked.released_at,lease_locked.release_reason,lease_locked.project_id AS lease_project_id,lease_locked.runner_id,lease_locked.isolation_tier,false AS applied FROM lease_locked WHERE NOT EXISTS (SELECT 1 FROM lease_changed);

-- name: LeaseMutationFacts :one
SELECT sqlc.embed(l),sqlc.embed(i)
FROM leases l
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id AND p.deleted_at IS NULL
WHERE l.organization_id=$1 AND l.lease_id=$2 AND i.project_id=$3;

-- name: LeaseMutationRunner :one
SELECT r.organization_id,r.runner_id,r.machine_id,r.token_id,r.state,r.expires_at,r.removed_at,r.isolation_tier,r.host_services,r.tags
FROM runner_identities r JOIN runner_lease_assignments a ON a.organization_id=r.organization_id AND a.runner_id=r.runner_id
WHERE a.organization_id=$1 AND a.lease_id=$2;

-- name: LockLeaseMutationRunner :one
SELECT r.organization_id,r.runner_id,r.machine_id,r.token_id,r.state,r.expires_at,r.removed_at,r.isolation_tier,r.host_services,r.tags
FROM runner_identities r JOIN runner_lease_assignments a ON a.organization_id=r.organization_id AND a.runner_id=r.runner_id
WHERE a.organization_id=$1 AND a.lease_id=$2 FOR UPDATE OF r;

-- name: LeaseMutationReservation :many
SELECT sqlc.embed(r),c.*
FROM provider_reservations r
CROSS JOIN LATERAL (
 SELECT 0::integer AS kind,0::integer AS position,''::text AS model_id,false AS has_detail,
 ''::text AS label,''::text AS model_provider,false AS is_default,''::text AS default_reasoning_effort,false AS legacy,
 ''::text AS effort,''::text AS window_kind,NULL::double precision AS percent_used,NULL::timestamptz AS window_reset_at,NULL::timestamptz AS window_observed_at
 UNION ALL
 SELECT 1,m.position,m.model_id,m.has_detail,m.label,m.model_provider,m.is_default,m.default_reasoning_effort,m.legacy,'','',NULL,NULL,NULL
 FROM provider_reservation_models m WHERE m.organization_id=r.organization_id AND m.lease_id=r.lease_id
 UNION ALL
 SELECT 2,e.position,e.model_id,false,'','',false,'',false,e.effort,'',NULL,NULL,NULL
 FROM provider_reservation_model_efforts e WHERE e.organization_id=r.organization_id AND e.lease_id=r.lease_id
 UNION ALL
 SELECT 3,w.position,'',false,'','',false,'',false,'',w.kind,w.percent_used,w.reset_at,w.observed_at
 FROM provider_reservation_usage_windows w WHERE w.organization_id=r.organization_id AND w.lease_id=r.lease_id
) c
WHERE r.organization_id=$1 AND r.lease_id=$2
ORDER BY c.kind,CASE WHEN c.kind=2 THEN c.model_id ELSE '' END,c.position;

-- name: SetAttemptRuntimeHeartbeat :exec
UPDATE native_attempts SET runtime_heartbeat_at=sqlc.narg(runtime_heartbeat_at),runtime_summary_sequence=sqlc.arg(sequence),
worker_cleanup_required=CASE WHEN sqlc.narg(worker_cleanup_required)::boolean IS NOT NULL THEN sqlc.narg(worker_cleanup_required)::boolean WHEN runtime_summary_sequence=sequence THEN worker_cleanup_required ELSE NULL END
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND sequence=sqlc.arg(sequence)
AND (runtime_summary_sequence IS DISTINCT FROM sequence OR sqlc.narg(worker_cleanup_required)::boolean IS NOT NULL AND worker_cleanup_required IS DISTINCT FROM sqlc.narg(worker_cleanup_required)::boolean);

-- name: LocalAttemptMatches :many
WITH owned_item AS MATERIALIZED (
    SELECT item.id FROM issues item
    WHERE item.organization_id=sqlc.arg(organization_id) AND item.project_id=sqlc.arg(project_id)
    AND item.native_id=sqlc.arg(work_item_id)
), matches AS (
    SELECT a.id FROM native_attempts a JOIN owned_item i ON i.id=a.issue_id
    WHERE a.organization_id=sqlc.arg(organization_id) AND a.local_attempt_id=sqlc.arg(local_attempt_id)
    LIMIT 2
), unprojected AS (
    SELECT true AS coverage_unknown FROM native_attempts a JOIN owned_item i ON i.id=a.issue_id
    WHERE a.organization_id=sqlc.arg(organization_id) AND NOT a.local_attempt_id_recorded
    LIMIT 1
)
SELECT id,false AS coverage_unknown FROM matches
UNION ALL SELECT '' AS id,coverage_unknown FROM unprojected;

-- name: AcknowledgeAttemptWorker :one
UPDATE native_attempts n SET cleanup_acknowledged_at=coalesce(n.cleanup_acknowledged_at,sqlc.arg(acknowledged_at)::timestamptz)
FROM leases l, runner_lease_assignments a, work_item_identities i
WHERE n.organization_id=sqlc.arg(organization_id) AND n.project_id=sqlc.arg(project_id) AND n.id=sqlc.arg(attempt_id)
AND n.lease_id=sqlc.arg(lease_id) AND n.fencing_token=sqlc.arg(fencing_token) AND n.worker_runner_id=sqlc.arg(runner_id)
AND l.organization_id=n.organization_id AND l.lease_id=n.lease_id AND l.issue_id=n.issue_id AND l.fencing_token=n.fencing_token AND l.machine_id=sqlc.arg(machine_id)
AND a.organization_id=n.organization_id AND a.lease_id=n.lease_id AND a.runner_id=n.worker_runner_id
AND i.organization_id=n.organization_id AND i.id=n.issue_id AND i.project_id=n.project_id AND i.native_id=sqlc.arg(work_item_id)
AND (n.worker_pid,n.worker_pgid,n.worker_started_at,n.worker_start_key)=(sqlc.arg(pid)::integer,sqlc.arg(pgid)::integer,sqlc.arg(started_at)::timestamptz,sqlc.arg(start_key)::text)
RETURNING n.id;
