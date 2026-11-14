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

-- name: SpriteMember :one
SELECT * FROM runner_sprite_memberships WHERE organization_id=$1 AND name=$2;

-- name: SpriteLeaseUsage :one
WITH active AS (
 SELECT l.lease_id,l.machine_id,l.session_id,COALESCE(assignment.runner_id,'') AS runner_id,
 CASE WHEN lower(trim(COALESCE(i.workflow_state_name,'')))='merging' AND landing_policy.gate_github_pull_request IS FALSE THEN i.project_id ELSE '' END AS capacity_project
 FROM leases l
 JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 JOIN machines machine ON machine.organization_id=l.organization_id AND machine.id=l.machine_id AND machine.sprite_name=machine.hostname AND machine.sprite_name<>''
 LEFT JOIN runner_lease_assignments assignment ON assignment.organization_id=l.organization_id AND assignment.lease_id=l.lease_id
 LEFT JOIN LATERAL (
  SELECT approved.gate_github_pull_request FROM change_issue_links link
  JOIN change_requests change ON change.organization_id=link.organization_id AND change.project_id=link.project_id AND change.id=link.change_id
  JOIN change_versions version ON version.organization_id=change.organization_id AND version.change_id=change.id AND version.id=change.current_version_id
  JOIN policy_revisions approved ON approved.organization_id=version.organization_id AND approved.project_id=i.project_id AND approved.policy_id=version.policy_revision_id
  JOIN projects project ON project.organization_id=i.organization_id AND project.id=i.project_id AND project.profile='native'
  WHERE link.organization_id=i.organization_id AND link.project_id=i.project_id AND link.work_item_id=i.native_id
  ORDER BY link.created_at DESC,link.change_id DESC LIMIT 1
 ) landing_policy ON true
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz
), slots AS (
 SELECT DISTINCT CASE WHEN capacity_project<>'' AND session_id ~ '^landing/[^/]+/[^/]+$'
 THEN machine_id||'/'||capacity_project||'/'||split_part(session_id,'/',2)||'/'||runner_id ELSE lease_id END AS slot
 FROM active
)
SELECT count(*) FROM slots WHERE sqlc.arg(joining)::text='' OR slot<>sqlc.arg(joining)::text;

-- name: SpriteAllocationSize :one
SELECT count(*) FROM runner_sprite_memberships WHERE organization_id=$1 AND allocation_project_id=$2 AND state<>'deleted';

-- name: SpriteMemberHoldsWork :one
SELECT EXISTS (
 SELECT 1 FROM runner_identities r JOIN leases l ON l.organization_id=r.organization_id AND l.machine_id=r.machine_id
 WHERE r.organization_id=sqlc.arg(organization_id) AND r.enrollment_id=sqlc.arg(enrollment_id) AND l.released_at IS NULL
 AND (r.removed_at IS NULL OR EXISTS (SELECT 1 FROM native_attempts n WHERE n.organization_id=l.organization_id AND n.lease_id=l.lease_id AND n.status='interrupted' AND n.checkpoint_object_key IS NOT NULL AND n.cleanup_acknowledged_at IS NULL))
) OR EXISTS (
 SELECT 1 FROM runner_identities r JOIN runner_lease_assignments assignment ON assignment.organization_id=r.organization_id AND assignment.runner_id=r.runner_id
 JOIN native_attempts n ON n.organization_id=assignment.organization_id AND n.lease_id=assignment.lease_id
 JOIN leases l ON l.organization_id=n.organization_id AND l.lease_id=n.lease_id
 WHERE r.organization_id=sqlc.arg(organization_id) AND r.enrollment_id=sqlc.arg(enrollment_id)
 AND (n.status='running' AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz
 OR n.status IN ('running','interrupted') AND n.checkpoint_object_key IS NOT NULL AND n.cleanup_acknowledged_at IS NULL)
);

-- name: SpriteAllocationAuthority :one
SELECT EXISTS (
 SELECT 1 FROM project_sprite_allocations allocation
 JOIN api_tokens token ON token.organization_id=allocation.organization_id AND token.id=allocation.configured_by
 JOIN projects project ON project.organization_id=allocation.organization_id AND project.id=allocation.project_id AND project.deleted_at IS NULL
 WHERE allocation.organization_id=sqlc.arg(organization_id) AND allocation.project_id=sqlc.arg(project_id)
 AND token.revoked_at IS NULL AND token.created_at<=sqlc.arg(now)::timestamptz AND (token.expires_at IS NULL OR token.expires_at>sqlc.arg(now)::timestamptz)
 AND (token.operator_key_scope='' OR token.operator_key_scope='admin')
 AND ((token.scope='admin' AND NOT token.native_only) OR EXISTS (
  SELECT 1 FROM hosted_members member
  WHERE member.organization_id=token.organization_id AND member.principal_id=token.id AND hosted_administrator_access(member.organization_id,member.user_id,member.principal_id)
 ))
);

-- name: SpriteMemberPlacementCounts :one
SELECT count(*) FILTER (WHERE other.allocation_project_id=member.allocation_project_id AND other.state NOT IN ('deleted','deleting'))::bigint AS active_count,
 count(*) FILTER (WHERE other.allocation_project_id<>member.allocation_project_id AND other.state NOT IN ('deleted','deleting'))::bigint AS other_count,
 count(*) FILTER (WHERE other.allocation_project_id=member.allocation_project_id AND other.name<>member.name AND (other.state='enrolled' OR other.state='bootstrapping' AND (other.created_at<member.created_at OR other.created_at=member.created_at AND other.name<member.name)))::bigint AS ahead_count
FROM runner_sprite_memberships member JOIN runner_sprite_memberships other ON other.organization_id=member.organization_id
WHERE member.organization_id=$1 AND member.name=$2;
