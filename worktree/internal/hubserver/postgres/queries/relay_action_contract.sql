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
ON CONFLICT (organization_id,project_id,id) DO UPDATE SET finished_at=EXCLUDED.finished_at,cast_object_key=EXCLUDED.cast_object_key,cast_sha256=EXCLUDED.cast_sha256,cast_bytes=EXCLUDED.cast_bytes,cast_content_bytes=EXCLUDED.cast_content_bytes,truncated=workspace_terminal_recordings.truncated OR EXCLUDED.truncated,revision=workspace_terminal_recordings.revision+1,updated_at=EXCLUDED.updated_at
WHERE workspace_terminal_recordings.revision=sqlc.arg(expected_revision) AND workspace_terminal_recordings.workspace_id=EXCLUDED.workspace_id AND workspace_terminal_recordings.relay_session_id=EXCLUDED.relay_session_id AND workspace_terminal_recordings.stream_id=EXCLUDED.stream_id
 AND workspace_terminal_recordings.principal_id=EXCLUDED.principal_id AND workspace_terminal_recordings.subject=EXCLUDED.subject AND workspace_terminal_recordings.isolation=EXCLUDED.isolation AND workspace_terminal_recordings.support_reason=EXCLUDED.support_reason AND workspace_terminal_recordings.started_at=EXCLUDED.started_at
 AND EXCLUDED.cast_content_bytes>=workspace_terminal_recordings.cast_content_bytes
 AND (workspace_terminal_recordings.finished_at IS NULL OR EXCLUDED.finished_at>=workspace_terminal_recordings.finished_at) RETURNING *;

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
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(action_id) ON CONFLICT DO NOTHING RETURNING sqlc.embed(project_action_runs),COALESCE((SELECT subject.subject_work_item_id FROM workspace_sessions subject WHERE subject.organization_id=project_action_runs.organization_id AND subject.project_id=project_action_runs.project_id AND subject.id=project_action_runs.workspace_id),'')::text AS subject_work_item_id;

-- name: ClaimActionRun :one
UPDATE project_action_runs a SET status='running',claimed_by=COALESCE(NULLIF(sqlc.arg(claimant)::text,''),sqlc.arg(runner_id)::text),lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),started_at=sqlc.arg(started_at),updated_at=sqlc.arg(started_at),revision=a.revision+1
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp())
JOIN workspace_items wi ON wi.organization_id=w.organization_id AND wi.project_id=w.project_id AND wi.workspace_id=w.id
JOIN issues i ON i.organization_id=wi.organization_id AND i.project_id=wi.project_id AND i.native_id=wi.work_item_id AND i.id=l.issue_id
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND a.status='queued' AND a.claimed_by='' AND (sqlc.arg(claimant)::text='' OR (l.project_id=a.project_id AND l.runner_id=sqlc.arg(runner_id))) AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=sqlc.arg(runner_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token) AND NOT w.read_only AND w.state IN ('starting','ready','idle') RETURNING sqlc.embed(a),w.subject_work_item_id;

-- name: ReportActionRun :one
UPDATE project_action_runs a SET status=sqlc.arg(status),exit_code=sqlc.narg(exit_code),reason=sqlc.arg(reason),finished_at=sqlc.narg(finished_at),output_object_key=sqlc.narg(output_object_key),output_sha256=sqlc.arg(output_sha256),output_bytes=sqlc.arg(output_bytes),output_content_bytes=sqlc.arg(output_content_bytes),output_sequence=sqlc.arg(output_sequence),truncated=sqlc.arg(truncated),revision=a.revision+1,updated_at=sqlc.arg(updated_at)
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.released_at IS NULL AND l.expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp())
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND a.status='running' AND a.claimed_by=sqlc.arg(runner_id) AND a.lease_id=sqlc.arg(lease_id) AND a.fencing_token=sqlc.arg(fencing_token) AND a.output_sequence<sqlc.arg(output_sequence)
AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=a.claimed_by AND w.lease_id=a.lease_id AND w.fencing_token=a.fencing_token RETURNING sqlc.embed(a),w.subject_work_item_id;

-- name: TerminalRecording :one
SELECT * FROM workspace_terminal_recordings WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: WorkspaceRelayOwner :one
SELECT w.* FROM workspace_sessions w
JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.machine_id=w.machine_id
JOIN runner_identities r ON r.organization_id=l.organization_id AND r.machine_id=l.machine_id AND r.runner_id=w.runner_id
JOIN workspace_items wi ON wi.organization_id=w.organization_id AND wi.project_id=w.project_id AND wi.workspace_id=w.id
JOIN issues i ON i.organization_id=wi.organization_id AND i.project_id=wi.project_id AND i.native_id=wi.work_item_id AND i.id=l.issue_id
WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id) AND w.id=sqlc.arg(id)
 AND w.runner_id=sqlc.arg(runner_id) AND w.machine_id=sqlc.arg(machine_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token)
 AND l.project_id=w.project_id AND l.runner_id=w.runner_id AND r.removed_at IS NULL AND r.state<>'disabled' AND r.expires_at>sqlc.arg(now)::timestamptz
 AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz;

-- name: FinishRelayActionRun :one
UPDATE project_action_runs a SET status=sqlc.arg(status),exit_code=sqlc.narg(exit_code)::integer,reason=sqlc.arg(reason),finished_at=sqlc.arg(finished_at)::timestamptz,
 output_object_key=sqlc.narg(output_object_key)::text,output_sha256=sqlc.arg(output_sha256),output_bytes=sqlc.arg(output_bytes),output_content_bytes=sqlc.arg(output_content_bytes),
 output_sequence=a.output_sequence+1,truncated=sqlc.arg(truncated),revision=a.revision+1,updated_at=sqlc.arg(finished_at)::timestamptz
FROM leases l JOIN workspace_sessions w ON w.organization_id=l.organization_id AND w.project_id=sqlc.arg(project_id) AND w.id=sqlc.arg(workspace_id)
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND a.workspace_id=sqlc.arg(workspace_id)
 AND a.status='running' AND a.claimed_by=sqlc.arg(claimant) AND a.lease_id=sqlc.arg(lease_id) AND a.fencing_token=sqlc.arg(fencing_token)
 AND a.revision=sqlc.arg(expected_revision) AND a.output_sequence=sqlc.arg(expected_output_sequence)
 AND l.organization_id=a.organization_id AND l.lease_id=a.lease_id AND l.fencing_token=a.fencing_token
 AND l.project_id=a.project_id AND l.runner_id=sqlc.arg(runner_id)
 AND ((sqlc.arg(reason)::text='' AND l.released_at IS NULL AND l.expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) AND l.expires_at>sqlc.arg(finished_at)::timestamptz
  AND EXISTS(SELECT 1 FROM runner_identities r WHERE r.organization_id=a.organization_id AND r.runner_id=sqlc.arg(runner_id)
   AND r.machine_id=l.machine_id AND r.removed_at IS NULL AND r.state<>'disabled' AND r.expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) AND r.expires_at>sqlc.arg(finished_at)::timestamptz))
 OR (sqlc.arg(status)::text='failed' AND sqlc.narg(exit_code)::integer IS NULL AND
  (sqlc.arg(reason)::text='stream_closed'
   OR (sqlc.arg(reason)::text='workspace_closed' AND w.state IN ('closed','failed') AND w.updated_at>=a.started_at)
   OR (sqlc.arg(reason)::text='lease_lost' AND (l.released_at IS NOT NULL OR l.expires_at<=COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) OR l.expires_at<=sqlc.arg(finished_at)::timestamptz OR (w.state='failed' AND w.reason='lease_lost' AND w.updated_at>=a.started_at))))))
RETURNING sqlc.embed(a),w.subject_work_item_id;

-- name: RecentActionRuns :many
SELECT created_at,id FROM project_action_runs
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND action_id=sqlc.arg(action_id)
 AND (created_at,id)<(COALESCE(sqlc.narg(before_created_at)::timestamptz,'infinity'::timestamptz),sqlc.arg(before_id)::text)
ORDER BY created_at DESC,id DESC LIMIT sqlc.arg(page_limit);

-- name: ActionRunRecordsByIDs :many
SELECT * FROM project_action_runs
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND action_id=sqlc.arg(action_id) AND id=ANY(sqlc.arg(run_ids)::text[]);

-- name: ScanActiveActionRuns :many
WITH page AS MATERIALIZED (
 SELECT candidate.organization_id,candidate.project_id,candidate.id FROM project_action_runs candidate
 WHERE candidate.organization_id=sqlc.arg(organization_id) AND candidate.status IN ('queued','running')
  AND (candidate.project_id,candidate.id)>(sqlc.arg(after_project_id)::text,sqlc.arg(after_id)::text)
 ORDER BY candidate.project_id,candidate.id LIMIT sqlc.arg(page_limit)
)
SELECT sqlc.embed(a),w.state AS workspace_state,w.reason AS workspace_reason FROM page
JOIN project_action_runs a ON a.organization_id=page.organization_id AND a.project_id=page.project_id AND a.id=page.id
JOIN workspace_sessions w ON w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id
ORDER BY a.project_id,a.id;

-- name: FailOrphanedActionRun :one
UPDATE project_action_runs a SET status='failed',reason=CASE WHEN w.reason='lease_lost' THEN 'lease_lost' ELSE 'workspace_closed' END,
 started_at=COALESCE(a.started_at,sqlc.arg(now)::timestamptz),finished_at=sqlc.arg(now)::timestamptz,revision=a.revision+1,updated_at=sqlc.arg(now)::timestamptz
FROM workspace_sessions w
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id)
 AND a.revision=sqlc.arg(expected_revision) AND a.status IN ('queued','running')
 AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.state IN ('closed','failed')
RETURNING sqlc.embed(a),w.subject_work_item_id;

-- name: DispatchableActionRuns :many
WITH page AS MATERIALIZED (
 SELECT candidate.organization_id,candidate.project_id,candidate.id FROM project_action_runs candidate
 WHERE candidate.organization_id=sqlc.arg(organization_id) AND candidate.status='queued' AND candidate.claimed_by='' AND candidate.created_at<=sqlc.arg(before_created_at)::timestamptz
  AND (candidate.created_at,candidate.id)>(COALESCE(sqlc.narg(after_created_at)::timestamptz,'-infinity'::timestamptz),sqlc.arg(after_id)::text)
 ORDER BY candidate.created_at,candidate.id LIMIT sqlc.arg(page_limit)
)
SELECT sqlc.embed(a),w.state AS workspace_state,w.read_only AS workspace_read_only FROM page
JOIN project_action_runs a ON a.organization_id=page.organization_id AND a.project_id=page.project_id AND a.id=page.id
JOIN workspace_sessions w ON w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id
ORDER BY a.created_at,a.id;

-- name: RelayTicketPage :many
SELECT * FROM workspace_relay_tickets WHERE organization_id=sqlc.arg(organization_id) AND digest>sqlc.arg(after_digest) ORDER BY digest LIMIT sqlc.arg(page_limit);

-- name: DeleteExpiredRelayTicket :exec
DELETE FROM workspace_relay_tickets WHERE organization_id=sqlc.arg(organization_id) AND digest=sqlc.arg(digest)
 AND (expires_at<=sqlc.arg(now)::timestamptz OR redeemed_at<=sqlc.arg(redeemed_before)::timestamptz);

-- name: CreateReportedActionRun :one
INSERT INTO project_action_runs(organization_id,project_id,id,action_id,workspace_id,command_object_key,command_sha256,command_bytes,command_content_bytes,status,exit_code,reason,started_at,finished_at,output_object_key,output_sha256,output_bytes,output_content_bytes,output_sequence,truncated,claimed_by,lease_id,fencing_token,created_by,created_at,updated_at)
SELECT a.organization_id,a.project_id,sqlc.arg(id)::text,a.id,w.id,a.command_object_key,a.command_sha256,a.command_bytes,a.command_content_bytes,
 sqlc.arg(status)::text,sqlc.narg(exit_code)::integer,sqlc.arg(reason)::text,sqlc.narg(started_at)::timestamptz,sqlc.narg(finished_at)::timestamptz,
 sqlc.narg(output_object_key)::text,sqlc.arg(output_sha256)::text,sqlc.arg(output_bytes)::bigint,sqlc.arg(output_content_bytes)::bigint,1,sqlc.arg(truncated)::boolean,
 'runner:'||sqlc.arg(runner_id)::text,l.lease_id,l.fencing_token,'',sqlc.arg(created_at)::timestamptz,sqlc.arg(created_at)::timestamptz
FROM project_actions a JOIN workspace_sessions w ON w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=sqlc.arg(workspace_id)
JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.runner_id=w.runner_id AND l.project_id=w.project_id
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(action_id) AND a.run_on_worktree_creation
 AND w.runner_id=sqlc.arg(runner_id) AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token)
 AND l.released_at IS NULL AND l.expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) AND l.expires_at>sqlc.arg(created_at)::timestamptz
RETURNING sqlc.embed(project_action_runs),COALESCE((SELECT subject.subject_work_item_id FROM workspace_sessions subject WHERE subject.organization_id=project_action_runs.organization_id AND subject.project_id=project_action_runs.project_id AND subject.id=project_action_runs.workspace_id),'')::text AS subject_work_item_id;

-- name: ApplyReportedActionRun :one
UPDATE project_action_runs a SET status=sqlc.arg(status),exit_code=COALESCE(sqlc.narg(exit_code)::integer,a.exit_code),
 reason=CASE WHEN sqlc.arg(reason)::text<>'' THEN sqlc.arg(reason)::text ELSE a.reason END,
 started_at=COALESCE(sqlc.narg(started_at)::timestamptz,a.started_at),finished_at=COALESCE(sqlc.narg(finished_at)::timestamptz,a.finished_at),
 output_object_key=CASE WHEN sqlc.arg(output_present)::boolean THEN sqlc.narg(output_object_key)::text ELSE a.output_object_key END,
 output_sha256=CASE WHEN sqlc.arg(output_present)::boolean THEN sqlc.arg(output_sha256)::text ELSE a.output_sha256 END,
 output_bytes=CASE WHEN sqlc.arg(output_present)::boolean THEN sqlc.arg(output_bytes)::bigint ELSE a.output_bytes END,
 output_content_bytes=CASE WHEN sqlc.arg(output_present)::boolean THEN sqlc.arg(output_content_bytes)::bigint ELSE a.output_content_bytes END,
 truncated=CASE WHEN sqlc.arg(output_present)::boolean THEN sqlc.arg(truncated)::boolean ELSE a.truncated END,
 output_sequence=a.output_sequence+1,claimed_by='runner:'||sqlc.arg(runner_id)::text,lease_id=sqlc.arg(lease_id),fencing_token=sqlc.arg(fencing_token),revision=a.revision+1,updated_at=sqlc.arg(updated_at)::timestamptz
FROM workspace_sessions w JOIN leases l ON l.organization_id=w.organization_id AND l.lease_id=w.lease_id AND l.fencing_token=w.fencing_token AND l.runner_id=w.runner_id AND l.project_id=w.project_id
WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.id=sqlc.arg(id) AND a.action_id=sqlc.arg(action_id)
 AND a.workspace_id=sqlc.arg(workspace_id) AND a.revision=sqlc.arg(expected_revision) AND a.status IN ('queued','running')
 AND (a.status='queued' OR sqlc.arg(status)::text<>'queued') AND a.claimed_by IN ('','runner:'||sqlc.arg(runner_id)::text)
 AND (a.lease_id='' OR (a.lease_id=sqlc.arg(lease_id) AND a.fencing_token=sqlc.arg(fencing_token)))
 AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id AND w.runner_id=sqlc.arg(runner_id)
 AND w.lease_id=sqlc.arg(lease_id) AND w.fencing_token=sqlc.arg(fencing_token)
 AND l.released_at IS NULL AND l.expires_at>COALESCE(sqlc.narg(lease_clock)::timestamptz,clock_timestamp()) AND l.expires_at>sqlc.arg(updated_at)::timestamptz
RETURNING sqlc.embed(a),w.subject_work_item_id;

-- name: RecentTerminalRecordings :many
SELECT * FROM workspace_terminal_recordings WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND workspace_id=sqlc.arg(workspace_id)
ORDER BY started_at DESC,id DESC LIMIT sqlc.arg(page_limit);

-- name: RecentRelayTerminalRecordings :many
SELECT * FROM workspace_terminal_recordings WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND workspace_id=sqlc.arg(workspace_id) AND relay_session_id=sqlc.arg(relay_session_id)
ORDER BY started_at DESC,id DESC LIMIT sqlc.arg(page_limit);

-- name: AttachRelayRecording :exec
UPDATE workspace_relay_sessions s SET recording_artifact=sqlc.arg(recording_artifact)
WHERE s.organization_id=sqlc.arg(organization_id) AND s.project_id=sqlc.arg(project_id) AND s.workspace_id=sqlc.arg(workspace_id)
 AND s.id=sqlc.arg(relay_session_id) AND s.recording_artifact=''
 AND EXISTS(SELECT 1 FROM workspace_terminal_recordings r WHERE r.organization_id=s.organization_id AND r.project_id=s.project_id AND r.workspace_id=s.workspace_id AND r.relay_session_id=s.id AND r.id=sqlc.arg(recording_id));
