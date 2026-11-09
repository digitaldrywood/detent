-- name: CreateRelayTicket :one
INSERT INTO workspace_relay_tickets(organization_id,project_id,digest,workspace_id,session_id,principal_id,subject,support_reason,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *;

-- name: RedeemRelayTicket :one
UPDATE workspace_relay_tickets SET redeemed_at=sqlc.arg(now)::timestamptz
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND digest=sqlc.arg(digest) AND session_id=sqlc.arg(session_id) AND principal_id=sqlc.arg(principal_id) AND subject=sqlc.arg(subject) AND redeemed_at IS NULL AND expires_at>sqlc.arg(now) AND created_at<=sqlc.arg(now) RETURNING *;

-- name: CreateRelaySession :one
INSERT INTO workspace_relay_sessions(organization_id,project_id,workspace_id,id,principal_id,subject,connection_id,channels,opened_at,support_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *;

-- name: CloseRelaySession :execrows
UPDATE workspace_relay_sessions SET closed_at=sqlc.arg(closed_at),close_reason=sqlc.arg(close_reason),bytes_in=sqlc.arg(bytes_in),bytes_out=sqlc.arg(bytes_out),recording_artifact=COALESCE(NULLIF(sqlc.arg(recording_artifact)::text,''),recording_artifact)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id) AND connection_id=sqlc.arg(connection_id) AND closed_at IS NULL;

-- name: RelaySessions :many
SELECT * FROM workspace_relay_sessions WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 AND id>$4 ORDER BY id LIMIT $5;

-- name: PutTerminalRecording :one
INSERT INTO workspace_terminal_recordings(organization_id,project_id,workspace_id,id,relay_session_id,stream_id,principal_id,subject,isolation,support_reason,started_at,finished_at,terminal_cols,terminal_rows,cast_object_key,cast_sha256,cast_bytes,cast_content_bytes,truncated,updated_at)
VALUES(sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(workspace_id),sqlc.arg(id),sqlc.arg(relay_session_id),sqlc.arg(stream_id),sqlc.arg(principal_id),sqlc.arg(subject),sqlc.arg(isolation),sqlc.arg(support_reason),sqlc.arg(started_at),sqlc.narg(finished_at),sqlc.arg(terminal_cols),sqlc.arg(terminal_rows),sqlc.arg(cast_object_key),sqlc.arg(cast_sha256),sqlc.arg(cast_bytes),sqlc.arg(cast_content_bytes),sqlc.arg(truncated),sqlc.arg(updated_at))
ON CONFLICT (organization_id,project_id,id) DO UPDATE SET finished_at=EXCLUDED.finished_at,cast_object_key=EXCLUDED.cast_object_key,cast_sha256=EXCLUDED.cast_sha256,cast_bytes=EXCLUDED.cast_bytes,cast_content_bytes=EXCLUDED.cast_content_bytes,truncated=EXCLUDED.truncated,revision=workspace_terminal_recordings.revision+1,updated_at=EXCLUDED.updated_at
WHERE workspace_terminal_recordings.revision=sqlc.arg(expected_revision) AND workspace_terminal_recordings.workspace_id=EXCLUDED.workspace_id AND workspace_terminal_recordings.relay_session_id=EXCLUDED.relay_session_id AND workspace_terminal_recordings.stream_id=EXCLUDED.stream_id AND workspace_terminal_recordings.finished_at IS NULL RETURNING *;

-- name: TerminalRecordings :many
SELECT * FROM workspace_terminal_recordings WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 AND id>$4 ORDER BY id LIMIT $5;

-- name: ActionRun :one
SELECT * FROM project_action_runs WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: ActionRuns :many
SELECT * FROM project_action_runs WHERE organization_id=$1 AND project_id=$2 AND action_id=$3 AND id>$4 ORDER BY id LIMIT $5;

-- name: CreateActionRun :one
INSERT INTO project_action_runs(organization_id,project_id,id,action_id,workspace_id,command_object_key,command_sha256,command_bytes,command_content_bytes,created_by,created_at,updated_at)
SELECT a.organization_id,a.project_id,sqlc.arg(id)::text,a.id,w.id,a.command_object_key,a.command_sha256,a.command_bytes,a.command_content_bytes,sqlc.arg(created_by)::text,sqlc.arg(created_at)::timestamptz,sqlc.arg(created_at)::timestamptz
FROM project_actions a JOIN workspace_sessions w ON w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=sqlc.arg(workspace_id) AND w.state IN ('starting','ready','idle')
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(action_id) AND a.deleted_at IS NULL ON CONFLICT DO NOTHING RETURNING *;

-- name: ClaimActionRun :one
UPDATE project_action_runs a SET status='running',claimed_stream_id=sqlc.arg(claimed_stream_id),claimed_by=sqlc.arg(runner_id),lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),started_at=sqlc.arg(started_at),updated_at=sqlc.arg(started_at),revision=a.revision+1
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND EXISTS (SELECT 1 FROM project_actions d WHERE d.organization_id=a.organization_id AND d.project_id=a.project_id AND d.id=a.action_id AND d.deleted_at IS NULL) AND a.status='queued' AND a.claimed_by='' AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=sqlc.arg(runner_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token) AND w.state IN ('starting','ready','idle') RETURNING a.*;

-- name: ReportActionRun :one
UPDATE project_action_runs a SET claimed_by=sqlc.arg(runner_id),lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),started_at=coalesce(a.started_at,sqlc.arg(updated_at)),status=sqlc.arg(status),exit_code=sqlc.narg(exit_code),reason=sqlc.arg(reason),finished_at=sqlc.narg(finished_at),output_object_key=sqlc.narg(output_object_key),output_sha256=sqlc.arg(output_sha256),output_bytes=sqlc.arg(output_bytes),output_content_bytes=sqlc.arg(output_content_bytes),output_sequence=sqlc.arg(output_sequence),truncated=sqlc.arg(truncated),revision=a.revision+1,updated_at=sqlc.arg(updated_at)
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND (a.status='queued' AND a.claimed_by='' OR a.status='running' AND a.claimed_by=sqlc.arg(runner_id) AND a.lease_id=sqlc.arg(lease_id) AND a.fencing_token=sqlc.arg(fencing_token)) AND a.output_sequence<sqlc.arg(output_sequence)
AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=sqlc.arg(runner_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token) RETURNING a.*;

-- name: TerminalRecording :one
SELECT * FROM workspace_terminal_recordings WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: ActionRunByID :one
SELECT * FROM project_action_runs WHERE organization_id=$1 AND id=$2;

-- name: PendingActionRuns :many
SELECT a.* FROM workspace_sessions w JOIN LATERAL (SELECT a.* FROM project_action_runs a WHERE a.organization_id=w.organization_id AND a.project_id=w.project_id AND a.workspace_id=w.id AND a.status='queued' AND a.created_at<=$2 AND EXISTS (SELECT 1 FROM project_actions d WHERE d.organization_id=a.organization_id AND d.project_id=a.project_id AND d.id=a.action_id AND d.deleted_at IS NULL) ORDER BY a.created_at,a.id LIMIT $3) a ON true WHERE w.organization_id=$1 AND w.state IN ('ready','idle') ORDER BY a.created_at,a.id LIMIT $3;

-- name: ActiveActionRunsAfter :many
SELECT * FROM project_action_runs WHERE organization_id=$1 AND id>$2 AND status IN ('queued','running') ORDER BY id LIMIT $3;

-- name: FinishActionRun :one
UPDATE project_action_runs SET status=sqlc.arg(status),exit_code=sqlc.narg(exit_code),reason=sqlc.arg(reason),finished_at=sqlc.arg(finished_at),output_object_key=sqlc.narg(output_object_key),output_sha256=sqlc.arg(output_sha256),output_bytes=sqlc.arg(output_bytes),output_content_bytes=sqlc.arg(output_content_bytes),output_sequence=output_sequence+1,truncated=sqlc.arg(truncated),revision=revision+1,updated_at=sqlc.arg(finished_at)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id) AND revision=sqlc.arg(expected_revision) AND status IN ('queued','running') RETURNING *;

-- name: PruneRelayTickets :execrows
DELETE FROM workspace_relay_tickets t WHERE t.organization_id=$1 AND t.digest IN (SELECT expired.digest FROM workspace_relay_tickets expired WHERE expired.organization_id=$1 AND expired.expires_at<=$2 ORDER BY expired.expires_at,expired.digest LIMIT $3);

-- name: TerminalRecordingByID :one
SELECT * FROM workspace_terminal_recordings WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 AND id=$4;

-- name: ReferenceRelayRecording :execrows
UPDATE workspace_relay_sessions SET recording_artifact=$4 WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND recording_artifact='';

-- name: CreateReportedActionRun :one
INSERT INTO project_action_runs(organization_id,project_id,id,action_id,workspace_id,command_object_key,command_sha256,command_bytes,command_content_bytes,status,exit_code,reason,started_at,finished_at,output_object_key,output_sha256,output_bytes,output_content_bytes,output_sequence,truncated,claimed_by,lease_id,fencing_token,created_by,created_at,updated_at)
SELECT a.organization_id,a.project_id,sqlc.arg(id)::text,a.id,w.id,a.command_object_key,a.command_sha256,a.command_bytes,a.command_content_bytes,sqlc.arg(status)::text,sqlc.narg(exit_code)::integer,sqlc.arg(reason)::text,sqlc.arg(started_at)::timestamptz,sqlc.narg(finished_at)::timestamptz,NULLIF(sqlc.arg(output_object_key)::text,''),sqlc.arg(output_sha256)::text,sqlc.arg(output_bytes)::bigint,sqlc.arg(output_content_bytes)::bigint,sqlc.arg(output_sequence)::bigint,sqlc.arg(truncated)::boolean,w.runner_id,w.lease_id,w.fencing_token,'',sqlc.arg(created_at)::timestamptz,sqlc.arg(created_at)::timestamptz
FROM project_actions a JOIN workspace_sessions w ON w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=sqlc.arg(workspace_id) AND w.state IN ('starting','ready','idle') AND w.read_only=false AND 'exec'=ANY(w.capabilities)
JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(action_id) AND a.run_on_worktree_creation=true AND a.deleted_at IS NULL AND w.runner_id=sqlc.arg(runner_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token)
ON CONFLICT DO NOTHING RETURNING *;

-- name: RecentActionRuns :many
SELECT * FROM project_action_runs WHERE organization_id=$1 AND project_id=$2 AND action_id=$3 ORDER BY created_at DESC,id DESC LIMIT $4;

-- name: RecentTerminalRecordings :many
SELECT * FROM workspace_terminal_recordings WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 ORDER BY started_at DESC,id DESC LIMIT $4;

-- name: RecentRelaySessions :many
SELECT * FROM workspace_relay_sessions WHERE organization_id=$1 AND project_id=$2 AND workspace_id=$3 ORDER BY opened_at DESC,id DESC LIMIT $4;

-- name: DeletedProjectActions :many
SELECT project_id,id FROM project_actions WHERE organization_id=$1 AND deleted_at IS NOT NULL ORDER BY project_id,id LIMIT $2;

-- name: PurgeDeletedActionRun :one
DELETE FROM project_action_runs r WHERE r.organization_id=sqlc.arg(organization_id) AND r.project_id=sqlc.arg(project_id) AND r.action_id=sqlc.arg(action_id) AND r.id=(SELECT selected.id FROM project_action_runs selected WHERE selected.organization_id=sqlc.arg(organization_id) AND selected.project_id=sqlc.arg(project_id) AND selected.action_id=sqlc.arg(action_id) ORDER BY selected.id LIMIT 1) AND EXISTS (SELECT 1 FROM project_actions a WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(action_id) AND a.deleted_at IS NOT NULL) RETURNING r.*;

-- name: PurgeEmptyDeletedAction :execrows
DELETE FROM project_actions a WHERE a.organization_id=$1 AND a.project_id=$2 AND a.id=$3 AND a.deleted_at IS NOT NULL AND NOT EXISTS (SELECT 1 FROM project_action_runs r WHERE r.organization_id=a.organization_id AND r.project_id=a.project_id AND r.action_id=a.id);
