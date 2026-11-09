-- name: WorkspaceSession :one
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: WorkspaceSubjectSessions :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND subject_work_item_id=$3 AND id>$4 ORDER BY id LIMIT $5;

-- name: CreateWorkspaceSession :one
INSERT INTO workspace_sessions(organization_id,project_id,id,subject_work_item_id,attempt_id,ref,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at,state,read_only)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,'requested',$13) RETURNING *;

-- name: UpdateWorkspaceSession :one
UPDATE workspace_sessions SET state=sqlc.arg(state),reason=sqlc.arg(reason),runner_id=sqlc.arg(runner_id),machine_id=sqlc.arg(machine_id),lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),head_sha=sqlc.arg(head_sha),capabilities=sqlc.narg(capabilities)::text[],isolation=sqlc.arg(isolation),worktree=sqlc.arg(worktree),machine_hostname=sqlc.arg(machine_hostname),worktree_path=sqlc.arg(worktree_path),read_only=sqlc.arg(read_only),opened_at=sqlc.narg(opened_at),last_activity_at=sqlc.narg(last_activity_at),last_heartbeat_at=sqlc.narg(last_heartbeat_at),rebind_deadline=sqlc.narg(rebind_deadline),revision=revision+1,updated_at=sqlc.arg(updated_at)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id) AND revision=sqlc.arg(expected_revision) AND runner_id=sqlc.arg(expected_runner_id) AND lease_id=sqlc.arg(expected_lease_id) AND fencing_token=sqlc.arg(expected_fencing_token)
RETURNING *;

-- name: WorkspaceOwnerLease :one
SELECT EXISTS(SELECT 1 FROM leases l JOIN runner_identities r ON r.organization_id=l.organization_id AND r.machine_id=l.machine_id
 JOIN workspace_items w ON w.organization_id=l.organization_id AND w.project_id=sqlc.arg(project_id) AND w.workspace_id=sqlc.arg(workspace_id)
 JOIN issues i ON i.organization_id=w.organization_id AND i.project_id=w.project_id AND i.native_id=w.work_item_id AND i.id=l.issue_id
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.lease_id=sqlc.arg(lease_id) AND l.fencing_token=sqlc.arg(fencing_token) AND l.machine_id=sqlc.arg(machine_id) AND r.runner_id=sqlc.arg(runner_id) AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz);

-- name: ExpiringWorkspaces :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND state NOT IN ('closed','failed') AND expires_at<=$2 ORDER BY expires_at,id LIMIT $3;

-- name: CreateWorkspaceOccupancy :execrows
INSERT INTO workspace_occupancy(organization_id,project_id,workspace_id,runner_id,started_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING;

-- name: EndWorkspaceOccupancy :execrows
UPDATE workspace_occupancy SET ended_at=$5 WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 AND runner_id=$4 AND ended_at IS NULL;

-- name: WorkspaceOccupancy :many
SELECT * FROM workspace_occupancy WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 ORDER BY started_at,runner_id LIMIT $4;

-- name: WorkspaceSessionByItem :one
SELECT w.* FROM workspace_sessions w JOIN workspace_items i ON i.organization_id=w.organization_id AND i.project_id=w.project_id AND i.workspace_id=w.id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.work_item_id=$3;

-- name: WorkspaceDispatchItem :one
SELECT work_item_id FROM workspace_items WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3;

-- name: OpenWorkspaceCounts :one
SELECT count(*)::bigint AS total, count(*) FILTER (WHERE created_by=sqlc.arg(actor))::bigint AS mine FROM workspace_sessions WHERE organization_id=sqlc.arg(organization_id) AND state NOT IN ('closed','failed');

-- name: OpenWorkspaceAttempt :one
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 AND state NOT IN ('closed','failed');

-- name: ActiveWorkspaceSessions :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND state NOT IN ('closed','failed') AND id>$2 ORDER BY id LIMIT $3;

-- name: NextWorkspaceMaintenanceOrganization :one
SELECT COALESCE(public.next_workspace_maintenance_organization(sqlc.arg(after_organization)::text),'')::text AS organization_id;

-- name: WorkspaceSessionByID :one
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND id=$2;

-- name: WorkspaceProjectRecent :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at DESC,id DESC LIMIT $3;

-- name: WorkspaceStateRecent :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND state=$3 ORDER BY created_at DESC,id DESC LIMIT $4;

-- name: WorkspaceSubjectRecent :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND subject_work_item_id=$3 ORDER BY created_at DESC,id DESC LIMIT $4;

-- name: WorkspaceSubjectStateRecent :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND subject_work_item_id=$3 AND state=$4 ORDER BY created_at DESC,id DESC LIMIT $5;

-- name: ReporterWorkspaceProjectRecent :many
SELECT w.* FROM issues i
JOIN workspace_sessions w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.subject_work_item_id=i.native_id
WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND i.actor_principal_id=sqlc.arg(principal_id)::text AND sqlc.arg(principal_id)::text<>''
ORDER BY w.created_at DESC,w.id DESC LIMIT sqlc.arg(page_limit)::integer;

-- name: ReporterWorkspaceStateRecent :many
SELECT w.* FROM issues i
JOIN workspace_sessions w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.subject_work_item_id=i.native_id
WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND i.actor_principal_id=sqlc.arg(principal_id)::text AND sqlc.arg(principal_id)::text<>'' AND w.state=sqlc.arg(state)
ORDER BY w.created_at DESC,w.id DESC LIMIT sqlc.arg(page_limit)::integer;

-- name: ReporterWorkspaceSubjectRecent :many
SELECT w.* FROM issues i
JOIN workspace_sessions w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.subject_work_item_id=i.native_id
WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND i.actor_principal_id=sqlc.arg(principal_id)::text AND sqlc.arg(principal_id)::text<>'' AND w.subject_work_item_id=sqlc.arg(subject_work_item_id)
ORDER BY w.created_at DESC,w.id DESC LIMIT sqlc.arg(page_limit)::integer;

-- name: ReporterWorkspaceSubjectStateRecent :many
SELECT w.* FROM issues i
JOIN workspace_sessions w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.subject_work_item_id=i.native_id
WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND i.actor_principal_id=sqlc.arg(principal_id)::text AND sqlc.arg(principal_id)::text<>'' AND w.subject_work_item_id=sqlc.arg(subject_work_item_id) AND w.state=sqlc.arg(state)
ORDER BY w.created_at DESC,w.id DESC LIMIT sqlc.arg(page_limit)::integer;

-- name: WorkspaceSessionsByIDs :many
SELECT * FROM workspace_sessions WHERE organization_id=$1 AND id=ANY($2::text[]) ORDER BY id;

-- name: WorkspaceClaimSession :one
SELECT sqlc.embed(s),i.closed_at
FROM workspace_items i JOIN workspace_sessions s ON s.organization_id=i.organization_id AND s.project_id=i.project_id AND s.id=i.workspace_id
WHERE i.organization_id=$1 AND i.project_id=$2 AND i.work_item_id=$3;

-- name: LockWorkspaceClaimSession :one
SELECT sqlc.embed(s),i.closed_at
FROM workspace_items i JOIN workspace_sessions s ON s.organization_id=i.organization_id AND s.project_id=i.project_id AND s.id=i.workspace_id
WHERE i.organization_id=$1 AND i.project_id=$2 AND i.work_item_id=$3
FOR UPDATE OF i,s;
