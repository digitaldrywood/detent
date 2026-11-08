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
UPDATE project_sprite_allocations SET desired_count=sqlc.arg(desired_count),revision=revision+1,provision_failures=0,requested_at=sqlc.arg(requested_at),configured_by=sqlc.arg(configured_by)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND revision=sqlc.arg(expected_revision) RETURNING *;

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
