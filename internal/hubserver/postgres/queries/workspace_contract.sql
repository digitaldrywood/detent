-- name: WorkspaceSession :one
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: WorkspaceSessions :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND id>$3 ORDER BY id LIMIT $4;

-- name: WorkspaceSubjectSessions :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND subject_work_item_id=$3 AND id>$4 ORDER BY id LIMIT $5;

-- name: CreateWorkspaceSession :one
INSERT INTO workspace_sessions(organization_id,project_id,id,subject_work_item_id,attempt_id,ref,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at,state)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,'requested') RETURNING *;

-- name: UpdateWorkspaceSession :one
UPDATE workspace_sessions SET state=sqlc.arg(state),reason=sqlc.arg(reason),runner_id=sqlc.arg(runner_id),machine_id=sqlc.arg(machine_id),lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),head_sha=sqlc.arg(head_sha),capabilities=sqlc.narg(capabilities)::text[],isolation=sqlc.arg(isolation),worktree=sqlc.arg(worktree),machine_hostname=sqlc.arg(machine_hostname),worktree_path=sqlc.arg(worktree_path),read_only=sqlc.arg(read_only),opened_at=sqlc.narg(opened_at),last_activity_at=sqlc.narg(last_activity_at),last_heartbeat_at=sqlc.narg(last_heartbeat_at),rebind_deadline=sqlc.narg(rebind_deadline),revision=revision+1,updated_at=sqlc.arg(updated_at)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id) AND revision=sqlc.arg(expected_revision) AND runner_id=sqlc.arg(expected_runner_id) AND lease_id=sqlc.arg(expected_lease_id) AND fencing_token=sqlc.arg(expected_fencing_token)
RETURNING *;

-- name: WorkspaceOwnerLease :one
SELECT EXISTS(SELECT 1 FROM leases l JOIN runner_identities r ON r.organization_id=l.organization_id AND r.machine_id=l.machine_id
 JOIN workspace_items w ON w.organization_id=l.organization_id AND w.project_id=sqlc.arg(project_id) AND w.workspace_id=sqlc.arg(workspace_id)
 JOIN issues i ON i.organization_id=w.organization_id AND i.project_id=w.project_id AND i.native_id=w.work_item_id AND i.id=l.issue_id
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id) AND l.fencing_token=sqlc.arg(fencing_token) AND l.machine_id=sqlc.arg(machine_id) AND r.runner_id=sqlc.arg(runner_id) AND l.released_at IS NULL AND l.expires_at>clock_timestamp());

-- name: ExpiringWorkspaces :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND state NOT IN ('closed','failed') AND expires_at<=$2 ORDER BY expires_at,id LIMIT $3;

-- name: CreateWorkspaceOccupancy :execrows
INSERT INTO workspace_occupancy(organization_id,project_id,workspace_id,runner_id,started_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING;

-- name: EndWorkspaceOccupancy :execrows
UPDATE workspace_occupancy SET ended_at=$5 WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 AND runner_id=$4 AND ended_at IS NULL;

-- name: WorkspaceOccupancy :many
SELECT * FROM workspace_occupancy WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 ORDER BY started_at,runner_id LIMIT $4;
