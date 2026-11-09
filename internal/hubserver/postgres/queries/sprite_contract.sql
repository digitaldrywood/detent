-- name: SpritePool :one
SELECT * FROM organization_sprite_pools WHERE organization_id=$1;

-- name: SetSpriteProvisionCursor :execrows
UPDATE organization_sprite_pools
SET provision_after_requested_at=sqlc.arg(after_requested_at)::timestamptz,provision_after_project_id=sqlc.arg(after_project_id)::text
WHERE organization_id=sqlc.arg(organization_id);

-- name: PutSpritePool :one
INSERT INTO organization_sprite_pools(organization_id,min_runners,max_runners,idle_seconds,bootstrap_object_key,configured_by,revision,isolation_tier,share_idle_capacity,placement_mode,todo_threshold,overflow_slots,max_sprite_slots)
SELECT sqlc.arg(organization_id),sqlc.arg(min_runners),sqlc.arg(max_runners),sqlc.arg(idle_seconds),sqlc.narg(bootstrap_object_key),sqlc.arg(configured_by),1,sqlc.arg(isolation_tier),sqlc.arg(share_idle_capacity),sqlc.arg(placement_mode),sqlc.arg(todo_threshold),sqlc.arg(overflow_slots),sqlc.arg(max_sprite_slots)
WHERE sqlc.arg(expected_revision)::bigint=0
ON CONFLICT (organization_id) DO UPDATE SET min_runners=EXCLUDED.min_runners,max_runners=EXCLUDED.max_runners,idle_seconds=EXCLUDED.idle_seconds,bootstrap_object_key=EXCLUDED.bootstrap_object_key,configured_by=EXCLUDED.configured_by,revision=organization_sprite_pools.revision+1,isolation_tier=EXCLUDED.isolation_tier,share_idle_capacity=EXCLUDED.share_idle_capacity,placement_mode=EXCLUDED.placement_mode,todo_threshold=EXCLUDED.todo_threshold,overflow_slots=EXCLUDED.overflow_slots,max_sprite_slots=EXCLUDED.max_sprite_slots
WHERE organization_sprite_pools.revision=sqlc.arg(expected_revision)
RETURNING *;

-- name: UpdateSpritePool :one
UPDATE organization_sprite_pools SET min_runners=sqlc.arg(min_runners),max_runners=sqlc.arg(max_runners),idle_seconds=sqlc.arg(idle_seconds),bootstrap_object_key=sqlc.narg(bootstrap_object_key),configured_by=sqlc.arg(configured_by),revision=revision+1,isolation_tier=sqlc.arg(isolation_tier),share_idle_capacity=sqlc.arg(share_idle_capacity),placement_mode=sqlc.arg(placement_mode),todo_threshold=sqlc.arg(todo_threshold),overflow_slots=sqlc.arg(overflow_slots),max_sprite_slots=sqlc.arg(max_sprite_slots)
WHERE organization_id=sqlc.arg(organization_id) AND revision=sqlc.arg(expected_revision) RETURNING *;

-- name: SpriteAllocation :one
SELECT * FROM project_sprite_allocations WHERE organization_id=$1 AND project_id=$2;

-- name: CreateSpriteAllocation :one
INSERT INTO project_sprite_allocations(organization_id,project_id,desired_count,revision,provision_failures,requested_at,configured_by)
VALUES($1,$2,$3,1,0,$4,$5) ON CONFLICT DO NOTHING RETURNING *;

-- name: UpdateSpriteAllocation :one
UPDATE project_sprite_allocations SET desired_count=sqlc.arg(desired_count),revision=revision+1,provision_failures=0,requested_at=CASE WHEN (SELECT count(*) FROM runner_sprite_memberships m WHERE m.organization_id=project_sprite_allocations.organization_id AND m.allocation_project_id=project_sprite_allocations.project_id AND m.state<>'deleted')>=desired_count THEN sqlc.arg(requested_at)::timestamptz ELSE requested_at END,configured_by=sqlc.arg(configured_by)
WHERE project_sprite_allocations.organization_id=sqlc.arg(organization_id) AND project_sprite_allocations.project_id=sqlc.arg(project_id) AND project_sprite_allocations.revision=sqlc.arg(expected_revision) RETURNING *;

-- name: SpriteAllocations :many
SELECT * FROM project_sprite_allocations WHERE organization_id=$1 AND project_id>$2 ORDER BY project_id LIMIT $3;

-- name: SpriteMembers :many
SELECT * FROM runner_sprite_memberships WHERE organization_id=$1 AND name>$2 ORDER BY name LIMIT $3;

-- name: SpriteOperation :one
SELECT * FROM sprite_lifecycle_operations WHERE organization_id=$1 AND idempotency_key=$2;

-- name: CreateSpriteOperation :one
INSERT INTO sprite_lifecycle_operations(organization_id,idempotency_key,name,operation,outcome,attempts,next_attempt_at,created_at,updated_at,request_object_key,target_enrollment_id,project_id,provider_resource_id,source_revision)
VALUES($1,$2,$3,$4,'pending',sqlc.arg(initial_attempts)::bigint,$5,$6,$6,sqlc.narg(request_object_key),sqlc.arg(target_enrollment_id),sqlc.arg(project_id),sqlc.arg(provider_resource_id),sqlc.arg(source_revision)) ON CONFLICT DO NOTHING RETURNING *;

-- name: UpdateSpriteOperation :execrows
UPDATE sprite_lifecycle_operations SET outcome=sqlc.arg(outcome),receipt_object_key=sqlc.narg(receipt_object_key),provider_resource_id=CASE WHEN sqlc.arg(provider_resource_id)::text='' THEN provider_resource_id ELSE sqlc.arg(provider_resource_id)::text END,attempts=attempts+1,next_attempt_at=sqlc.arg(next_attempt_at),updated_at=sqlc.arg(updated_at)
WHERE organization_id=sqlc.arg(organization_id) AND idempotency_key=sqlc.arg(idempotency_key) AND attempts=sqlc.arg(expected_attempts) AND outcome IN ('pending','uncertain');

-- name: PendingSpriteOperations :many
SELECT * FROM sprite_lifecycle_operations WHERE organization_id=$1 AND outcome IN ('pending','uncertain') AND next_attempt_at<=$2 ORDER BY next_attempt_at,idempotency_key LIMIT $3;

-- name: RescheduleBusySpriteOperation :execrows
UPDATE sprite_lifecycle_operations SET next_attempt_at=sqlc.arg(checked_at)::timestamptz,updated_at=sqlc.arg(checked_at)::timestamptz
WHERE organization_id=sqlc.arg(organization_id) AND idempotency_key=sqlc.arg(idempotency_key)
AND operation='delete' AND outcome IN ('pending','uncertain') AND attempts=sqlc.arg(expected_attempts)::bigint
AND next_attempt_at<=sqlc.arg(checked_at)::timestamptz AND updated_at<=sqlc.arg(checked_at)::timestamptz;

-- name: SpriteJobSlotCount :one
SELECT count(*) FROM (
 SELECT CASE WHEN l.landing_capacity>0 THEN '' ELSE l.lease_id END AS lease_key,
 CASE WHEN l.landing_capacity>0 THEN l.machine_id ELSE '' END AS machine_key,
 CASE WHEN l.landing_capacity>0 THEN i.project_id ELSE '' END AS project_key,
 CASE WHEN l.landing_capacity>0 THEN l.session_id ELSE '' END AS session_key
 FROM leases l JOIN machines m ON m.organization_id=l.organization_id AND m.id=l.machine_id
 JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.released_at IS NULL AND l.expires_at>sqlc.arg(at)::timestamptz
 AND m.sprite_name<>'' AND m.sprite_name=m.hostname
 AND NOT(l.landing_capacity>0 AND l.machine_id=sqlc.arg(joining_machine)::text
  AND i.project_id=sqlc.arg(joining_project)::text AND l.session_id=sqlc.arg(joining_session)::text AND sqlc.arg(joining_session)::text<>'')
 GROUP BY lease_key,machine_key,project_key,session_key
 LIMIT sqlc.arg(slot_limit)::bigint
) active;

-- name: RecordSpriteProvisionFailure :execrows
UPDATE project_sprite_allocations SET provision_failures=provision_failures+1 WHERE organization_id=$1 AND project_id=$2 AND revision=$3;

-- name: LockSpritePool :one
SELECT * FROM organization_sprite_pools WHERE organization_id=$1 FOR UPDATE;

-- name: SpriteMember :one
SELECT * FROM runner_sprite_memberships WHERE organization_id=$1 AND name=$2;

-- name: SpriteActiveMemberCount :one
SELECT count(*) FROM runner_sprite_memberships WHERE organization_id=$1 AND allocation_project_id=$2 AND state<>'deleted';

-- name: SpritePoolMemberCount :one
SELECT count(*) FROM runner_sprite_memberships WHERE organization_id=$1 AND state<>'deleted';

-- name: SpriteAllocationQueue :many
SELECT a.* FROM project_sprite_allocations a
JOIN projects p ON p.organization_id=a.organization_id AND p.id=a.project_id AND p.deleted_at IS NULL
WHERE a.organization_id=sqlc.arg(organization_id) AND (a.requested_at,a.project_id)>(sqlc.arg(after_requested_at)::timestamptz,sqlc.arg(after_project_id)::text)
ORDER BY a.requested_at,a.project_id LIMIT sqlc.arg(page_limit);

-- name: SpriteProvisionQueue :many
SELECT a.* FROM project_sprite_allocations a
JOIN projects p ON p.organization_id=a.organization_id AND p.id=a.project_id AND p.deleted_at IS NULL
WHERE a.organization_id=sqlc.arg(organization_id) AND a.desired_count>0
 AND a.desired_count>(SELECT count(*) FROM runner_sprite_memberships m WHERE m.organization_id=a.organization_id AND m.allocation_project_id=a.project_id AND m.state<>'deleted')
 AND (a.requested_at,a.project_id)>(sqlc.arg(after_requested_at)::timestamptz,sqlc.arg(after_project_id)::text)
ORDER BY a.requested_at,a.project_id LIMIT sqlc.arg(page_limit);

-- name: SpriteRetirementProjects :many
SELECT a.project_id FROM runner_sprite_memberships m
JOIN project_sprite_allocations a ON a.organization_id=m.organization_id AND a.project_id=m.allocation_project_id
WHERE m.organization_id=sqlc.arg(organization_id) AND m.name=ANY(sqlc.arg(member_names)::text[]) AND m.state<>'deleted'
GROUP BY a.project_id,a.desired_count,a.requested_at
ORDER BY (a.desired_count=0) DESC,a.requested_at DESC,a.project_id DESC;

-- name: SpritePoolMemberPage :many
WITH recent_deleted AS (
 SELECT d.organization_id,d.name,d.created_at FROM runner_sprite_memberships d
 WHERE d.organization_id=sqlc.arg(organization_id) AND d.state='deleted'
 ORDER BY d.created_at DESC,d.name DESC LIMIT 20
), selected AS (
 (SELECT a.organization_id,a.name,a.created_at FROM runner_sprite_memberships a
  WHERE a.organization_id=sqlc.arg(organization_id) AND a.state<>'deleted'
   AND (a.created_at,a.name)>(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_name)::text)
  ORDER BY a.created_at,a.name LIMIT sqlc.arg(page_limit))
 UNION ALL
 SELECT d.organization_id,d.name,d.created_at FROM recent_deleted d
 WHERE (d.created_at,d.name)>(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_name)::text)
)
SELECT sqlc.embed(m),COALESCE(r.runner_id,'')::text AS runner_id
FROM selected s JOIN runner_sprite_memberships m ON m.organization_id=s.organization_id AND m.name=s.name
LEFT JOIN runner_identities r ON r.organization_id=m.organization_id AND r.enrollment_id=m.enrollment_id
ORDER BY m.created_at,m.name LIMIT sqlc.arg(page_limit);

-- name: SpriteAllocationAuthority :one
SELECT EXISTS(
 SELECT 1 FROM project_sprite_allocations a
 JOIN projects p ON p.organization_id=a.organization_id AND p.id=a.project_id AND p.deleted_at IS NULL
 JOIN api_tokens t ON t.organization_id=a.organization_id AND t.id=a.configured_by
 WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id)
 AND t.revoked_at IS NULL AND t.created_at<=sqlc.arg(at)::timestamptz AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(at)::timestamptz)
 AND ((t.scope='admin' AND NOT t.native_only AND t.key_kind='service') OR EXISTS(
  SELECT 1 FROM hosted_members m JOIN hosted_project_grants g ON g.organization_id=m.organization_id AND g.user_id=m.user_id
  WHERE m.organization_id=a.organization_id AND m.principal_id=t.id AND m.active AND m.role IN ('owner','admin') AND g.project_id=a.project_id AND g.can_write
 ))
)::boolean;

-- name: SetSpriteExecSession :execrows
UPDATE sprite_lifecycle_operations SET exec_session_id=sqlc.arg(exec_session_id)::text
WHERE organization_id=sqlc.arg(organization_id) AND idempotency_key=sqlc.arg(idempotency_key)
AND attempts=sqlc.arg(expected_attempts) AND operation='bootstrap' AND outcome='uncertain'
AND (exec_session_id IS NULL OR exec_session_id=sqlc.arg(exec_session_id)::text);

-- name: SpriteMemberBusy :one
SELECT EXISTS(SELECT 1 FROM runner_sprite_memberships m JOIN runner_identities r ON r.organization_id=m.organization_id AND r.enrollment_id=m.enrollment_id
 JOIN leases l ON l.organization_id=r.organization_id AND l.machine_id=r.machine_id
 WHERE m.organization_id=$1 AND m.name=$2 AND l.released_at IS NULL
 AND (r.removed_at IS NULL OR EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=l.organization_id AND a.lease_id=l.lease_id AND a.cleanup_acknowledged_at IS NULL)))::boolean;

-- name: SpriteMemberWanted :one
SELECT (m.state IN ('bootstrapping','enrolled') AND CASE WHEN m.allocation_project_id='' THEN p.max_runners>0 ELSE
 EXISTS(SELECT 1 FROM project_sprite_allocations a JOIN projects pr ON pr.organization_id=a.organization_id AND pr.id=a.project_id AND pr.deleted_at IS NULL
 WHERE a.organization_id=m.organization_id AND a.project_id=m.allocation_project_id AND
 (SELECT count(*) FROM runner_sprite_memberships ahead WHERE ahead.organization_id=m.organization_id AND ahead.allocation_project_id=m.allocation_project_id AND ahead.name<>m.name
 AND (ahead.state='enrolled' OR ahead.state='bootstrapping' AND (ahead.created_at,ahead.name)<(m.created_at,m.name)))
 < LEAST(a.desired_count,GREATEST(p.max_runners-(SELECT count(*) FROM runner_sprite_memberships other WHERE other.organization_id=m.organization_id AND other.allocation_project_id<>m.allocation_project_id AND other.state NOT IN ('deleted','deleting')),0))) END)::boolean
FROM runner_sprite_memberships m JOIN organization_sprite_pools p ON p.organization_id=m.organization_id WHERE m.organization_id=$1 AND m.name=$2;

-- name: SpriteMemberRunner :one
SELECT r.runner_id FROM runner_sprite_memberships m JOIN runner_identities r ON r.organization_id=m.organization_id AND r.enrollment_id=m.enrollment_id
WHERE m.organization_id=$1 AND m.name=$2;

-- name: SpriteBudgetRetainsMember :one
SELECT EXISTS(SELECT 1 FROM runner_sprite_memberships m JOIN runner_identities r ON r.organization_id=m.organization_id AND r.enrollment_id=m.enrollment_id
 JOIN leases l ON l.organization_id=r.organization_id AND l.machine_id=r.machine_id
 JOIN monthly_budget_admissions a ON a.organization_id=l.organization_id AND a.issue_id=l.issue_id
 JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
 JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name
 WHERE NOT i.archived AND NOT ws.terminal AND m.organization_id=$1 AND m.name=$2 AND l.fencing_token=(SELECT max(latest.fencing_token) FROM leases latest WHERE latest.organization_id=l.organization_id AND latest.issue_id=l.issue_id))::boolean;

-- name: SpriteWakeTarget :one
SELECT EXISTS(SELECT 1 FROM runner_identities r JOIN machines m ON m.organization_id=r.organization_id AND m.id=r.machine_id
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
WHERE r.organization_id=sqlc.arg(organization_id) AND r.enrollment_id=sqlc.arg(enrollment_id) AND m.sprite_name=sqlc.arg(sprite_name) AND m.hostname=sqlc.arg(sprite_name)
AND r.removed_at IS NULL AND r.state='active' AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(at)::timestamptz)
AND (r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=sqlc.arg(project_id))))::boolean;

-- name: SpriteMemberOperations :many
SELECT * FROM sprite_lifecycle_operations WHERE organization_id=$1 AND name=$2 AND outcome IN ('committed','rejected') AND receipt_object_key IS NOT NULL ORDER BY created_at,idempotency_key LIMIT 6;

-- name: SpritePromotionSource :one
SELECT s.* FROM project_secrets s JOIN projects p ON p.organization_id=s.organization_id AND p.id=s.project_id
WHERE s.organization_id=$1 AND s.kind='fly_sprites_token' AND p.deleted_at IS NULL ORDER BY p.scheduling_rank,p.id LIMIT 1;

-- name: PromoteSpriteSecret :execrows
INSERT INTO organization_secrets(organization_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at)
SELECT sqlc.arg(organization_id),'fly_sprites_token',s.organization_slug,sqlc.arg(ciphertext)::bytea,sqlc.arg(nonce)::bytea,sqlc.arg(wrapped_data_key)::bytea,sqlc.arg(master_key_version)::integer,sqlc.arg(updated_at)::timestamptz
FROM project_secrets s
WHERE s.organization_id=sqlc.arg(organization_id) AND s.kind='fly_sprites_token'
AND s.project_id=sqlc.arg(source_project_id) AND s.ciphertext=sqlc.arg(source_ciphertext)::bytea AND s.nonce=sqlc.arg(source_nonce)::bytea
AND s.project_id=(SELECT first.project_id FROM project_secrets first JOIN projects p ON p.organization_id=first.organization_id AND p.id=first.project_id WHERE first.organization_id=sqlc.arg(organization_id) AND first.kind='fly_sprites_token' AND p.deleted_at IS NULL ORDER BY p.scheduling_rank,p.id LIMIT 1)
ON CONFLICT (organization_id,kind) DO NOTHING;

-- name: SpriteEnrollmentWake :one
SELECT * FROM sprite_lifecycle_operations WHERE organization_id=$1 AND target_enrollment_id=$2 AND operation='wake' ORDER BY created_at DESC,idempotency_key DESC LIMIT 1;

-- name: SpriteRetainedMemberCount :one
SELECT count(*) FROM runner_sprite_memberships WHERE organization_id=$1 AND allocation_project_id=$2 AND state<>'deleted' AND state<>'deleting';

-- name: SpriteRetainedPoolCount :one
SELECT count(*) FROM runner_sprite_memberships WHERE organization_id=$1 AND state<>'deleted' AND state<>'deleting';

-- name: SpriteProvisionSourceFailure :one
SELECT * FROM sprite_lifecycle_operations WHERE organization_id=$1 AND project_id=$2 AND source_revision=$3 AND outcome='rejected' AND next_attempt_at>updated_at AND operation IN ('provision','bootstrap','checkpoint') ORDER BY updated_at DESC,idempotency_key DESC LIMIT 1;
