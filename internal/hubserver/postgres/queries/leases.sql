-- name: RegisterLeaseMachine :exec
INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,sprite_name,last_heartbeat_at,registered_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,clock_timestamp(),clock_timestamp(),clock_timestamp())
ON CONFLICT (organization_id,id) DO UPDATE SET hostname=EXCLUDED.hostname,display_name=EXCLUDED.display_name,capacity=EXCLUDED.capacity,version=EXCLUDED.version,sprite_name=EXCLUDED.sprite_name,updated_at=clock_timestamp();

-- name: LockClaimIssue :one
SELECT i.revision,i.archived FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE i.organization_id=$1 AND i.id=$2 AND p.deleted_at IS NULL FOR SHARE OF p FOR NO KEY UPDATE OF i;

-- name: LockActiveLease :one
SELECT * FROM leases WHERE organization_id=$1 AND issue_id=$2 AND released_at IS NULL FOR UPDATE;

-- name: LockAttemptIssue :one
SELECT id FROM issues WHERE organization_id=$1 AND id=$2 FOR UPDATE;

-- name: LockAttemptProject :one
SELECT p.id FROM projects p JOIN issues i ON i.organization_id=p.organization_id AND i.project_id=p.id WHERE i.organization_id=$1 AND i.id=$2 FOR SHARE OF p;

-- name: LockLease :one
SELECT * FROM leases WHERE organization_id=$1 AND lease_id=$2 FOR UPDATE;

-- name: LeaseTime :one
SELECT clock_timestamp()::timestamptz AS now;

-- name: InsertLease :one
INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,landing_capacity,acquired_at,renewed_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,sqlc.arg(landing_capacity),sqlc.arg(now),sqlc.arg(now),sqlc.arg(expires_at)) RETURNING *;

-- name: UpdateLeaseLandingCapacity :one
UPDATE leases SET landing_capacity=GREATEST(landing_capacity,sqlc.arg(landing_capacity)::bigint)
WHERE organization_id=sqlc.arg(organization_id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND released_at IS NULL
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
ORDER BY fencing_token LIMIT sqlc.arg(page_limit);

-- name: GetAttemptReceipt :one
SELECT request_hash FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence=$3;

-- name: InsertLeaseAttempt :one
INSERT INTO native_attempts (organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,artifacts_object_key,started_at,started_at_submicro_ns,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12,sqlc.arg(started_at),sqlc.arg(started_at_submicro_ns),sqlc.arg(now)) RETURNING *;

-- name: AdvanceLeaseAttempt :one
UPDATE native_attempts SET sequence=sqlc.arg(next_sequence),
cleanup_acknowledged_at=CASE WHEN sqlc.arg(termination)::boolean THEN sqlc.arg(now) ELSE cleanup_acknowledged_at END,
status=CASE WHEN sqlc.arg(usage_correction)::boolean THEN status ELSE sqlc.arg(status) END,
payload_object_key=sqlc.arg(payload_object_key),
checkpoint_object_key=COALESCE(sqlc.narg(checkpoint_object_key),checkpoint_object_key),
artifacts_object_key=COALESCE(sqlc.narg(artifacts_object_key),artifacts_object_key),
updated_at=CASE WHEN sqlc.arg(usage_correction)::boolean THEN updated_at ELSE sqlc.arg(now) END
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND lease_id=sqlc.arg(lease_id)
AND fencing_token=sqlc.arg(fencing_token)::bigint AND run_id=sqlc.arg(run_id)
AND dispatch_generation=sqlc.arg(dispatch_generation) AND sequence=sqlc.arg(next_sequence)-1
AND (status='running' OR status='interrupted' AND sqlc.arg(status)='running' OR sqlc.arg(usage_correction)::boolean AND status=sqlc.arg(status) OR sqlc.arg(termination)::boolean AND status IN ('interrupted','cancelled') AND sqlc.arg(status)='cancelled')
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
WITH locked AS MATERIALIZED (
 SELECT l.* FROM leases l
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id)
 FOR UPDATE OF l
), at AS MATERIALIZED (
 SELECT COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) AS now FROM locked
), changed AS (
 UPDATE leases l SET
 renewed_at=CASE WHEN sqlc.arg(operation)::text='renew' THEN at.now ELSE l.renewed_at END,
 expires_at=CASE WHEN sqlc.arg(operation)::text='renew' THEN at.now+sqlc.arg(ttl_us)::bigint*interval '1 microsecond' ELSE l.expires_at END,
 landing_capacity=CASE WHEN sqlc.arg(operation)::text='renew' AND sqlc.arg(landing_capacity)::bigint>0 THEN sqlc.arg(landing_capacity)::bigint ELSE l.landing_capacity END,
 released_at=CASE WHEN sqlc.arg(operation)::text='renew' THEN l.released_at ELSE at.now END,
 release_reason=CASE WHEN sqlc.arg(operation)::text='renew' THEN l.release_reason WHEN sqlc.arg(operation)::text='expire' THEN 'expired' ELSE sqlc.arg(reason)::text END
 FROM locked,at
 WHERE l.organization_id=locked.organization_id AND l.lease_id=locked.lease_id
 AND locked.fencing_token=sqlc.arg(fencing_token)::bigint AND locked.released_at IS NULL
 AND CASE WHEN sqlc.arg(operation)::text='expire' THEN locked.expires_at<=at.now ELSE locked.expires_at>at.now END
 AND (sqlc.arg(operation)::text<>'renew' OR NOT EXISTS (
  SELECT 1 FROM issues i
  JOIN project_workflow_states s ON s.organization_id=i.organization_id AND s.project_id=i.project_id AND s.name=i.workflow_state_name
  WHERE i.organization_id=locked.organization_id AND i.id=locked.issue_id AND s.terminal
 ) OR EXISTS (
  SELECT 1 FROM native_attempts a
  JOIN change_landing_receipts r ON r.organization_id=a.organization_id AND r.attempt_id=a.id
  WHERE a.organization_id=locked.organization_id AND a.issue_id=locked.issue_id
  AND a.lease_id=locked.lease_id AND a.fencing_token=locked.fencing_token AND a.status='running'
 ))
 RETURNING l.*
), cancelled AS (
 UPDATE native_attempts a SET status='cancelled',sequence=sequence+1,updated_at=c.released_at
 FROM changed c WHERE a.organization_id=c.organization_id AND a.lease_id=c.lease_id AND a.fencing_token=c.fencing_token
 AND a.status='running' AND sqlc.arg(operation)::text<>'renew'
), heartbeat AS (
 UPDATE machines m SET last_heartbeat_at=greatest(m.last_heartbeat_at,at.now),updated_at=greatest(m.updated_at,at.now)
 FROM changed c,at WHERE m.organization_id=c.organization_id AND m.id=c.machine_id AND sqlc.arg(operation)::text<>'expire'
)
SELECT changed.*,true AS applied FROM changed
UNION ALL SELECT locked.*,false AS applied FROM locked WHERE NOT EXISTS (SELECT 1 FROM changed);

-- name: LeaseMutationFacts :one
SELECT sqlc.embed(l),sqlc.embed(i)
FROM leases l
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id AND p.deleted_at IS NULL
WHERE l.organization_id=$1 AND l.lease_id=$2 AND i.project_id=$3;

-- name: LeaseMutationRunner :one
SELECT r.organization_id,r.runner_id,r.machine_id,r.token_id,r.state,r.expires_at,r.removed_at,r.isolation_tier,r.host_services
FROM runner_identities r JOIN runner_lease_assignments a ON a.organization_id=r.organization_id AND a.runner_id=r.runner_id
WHERE a.organization_id=$1 AND a.lease_id=$2;

-- name: LockLeaseMutationRunner :one
SELECT r.organization_id,r.runner_id,r.machine_id,r.token_id,r.state,r.expires_at,r.removed_at,r.isolation_tier,r.host_services
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
