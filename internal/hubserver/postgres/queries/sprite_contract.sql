-- name: SpritePool :one
SELECT * FROM organization_sprite_pools WHERE organization_id=$1;

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
INSERT INTO sprite_lifecycle_operations(organization_id,idempotency_key,name,operation,outcome,attempts,next_attempt_at,created_at,updated_at)
VALUES($1,$2,$3,$4,'pending',0,$5,$6,$6) ON CONFLICT DO NOTHING RETURNING *;

-- name: UpdateSpriteOperation :execrows
UPDATE sprite_lifecycle_operations SET outcome=sqlc.arg(outcome),receipt_object_key=sqlc.narg(receipt_object_key),attempts=attempts+1,next_attempt_at=sqlc.arg(next_attempt_at),updated_at=sqlc.arg(updated_at)
WHERE organization_id=sqlc.arg(organization_id) AND idempotency_key=sqlc.arg(idempotency_key) AND attempts=sqlc.arg(expected_attempts) AND outcome IN ('pending','uncertain');

-- name: PendingSpriteOperations :many
SELECT * FROM sprite_lifecycle_operations WHERE organization_id=$1 AND outcome IN ('pending','uncertain') AND next_attempt_at<=$2 ORDER BY next_attempt_at,idempotency_key LIMIT $3;

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
