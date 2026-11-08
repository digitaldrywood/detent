-- name: CreateRelayTicket :one
INSERT INTO workspace_relay_tickets(organization_id,project_id,digest,workspace_id,session_id,principal_id,subject,support_reason,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *;

-- name: RedeemRelayTicket :one
UPDATE workspace_relay_tickets SET redeemed_at=sqlc.arg(now)::timestamptz
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND digest=sqlc.arg(digest) AND session_id=sqlc.arg(session_id) AND principal_id=sqlc.arg(principal_id) AND subject=sqlc.arg(subject) AND redeemed_at IS NULL AND expires_at>sqlc.arg(now) AND created_at<=sqlc.arg(now) RETURNING *;

-- name: CreateRelaySession :one
INSERT INTO workspace_relay_sessions(organization_id,project_id,workspace_id,id,principal_id,subject,connection_id,channels,opened_at,support_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *;

-- name: CloseRelaySession :execrows
UPDATE workspace_relay_sessions SET closed_at=sqlc.arg(closed_at),close_reason=sqlc.arg(close_reason),bytes_in=sqlc.arg(bytes_in),bytes_out=sqlc.arg(bytes_out),recording_artifact=sqlc.arg(recording_artifact)
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
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(action_id) ON CONFLICT DO NOTHING RETURNING *;

-- name: ClaimActionRun :one
UPDATE project_action_runs a SET status='running',claimed_by=sqlc.arg(runner_id),lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),started_at=sqlc.arg(started_at),updated_at=sqlc.arg(started_at),revision=a.revision+1
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>clock_timestamp()
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND a.status='queued' AND a.claimed_by='' AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=sqlc.arg(runner_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token) AND w.state IN ('starting','ready','idle') RETURNING a.*;

-- name: ReportActionRun :one
UPDATE project_action_runs a SET status=sqlc.arg(status),exit_code=sqlc.narg(exit_code),reason=sqlc.arg(reason),finished_at=sqlc.narg(finished_at),output_object_key=sqlc.narg(output_object_key),output_sha256=sqlc.arg(output_sha256),output_bytes=sqlc.arg(output_bytes),output_content_bytes=sqlc.arg(output_content_bytes),output_sequence=sqlc.arg(output_sequence),truncated=sqlc.arg(truncated),revision=a.revision+1,updated_at=sqlc.arg(updated_at)
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>clock_timestamp()
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND a.status='running' AND a.claimed_by=sqlc.arg(runner_id) AND a.lease_id=sqlc.arg(lease_id) AND a.fencing_token=sqlc.arg(fencing_token) AND a.output_sequence<sqlc.arg(output_sequence)
AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=a.claimed_by AND w.lease_id=a.lease_id AND w.fencing_token=a.fencing_token RETURNING a.*;

-- name: TerminalRecording :one
SELECT * FROM workspace_terminal_recordings WHERE organization_id=$1 AND project_id=$2 AND id=$3;
