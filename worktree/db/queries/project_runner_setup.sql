-- name: GetProjectRunnerSetupHash :one
SELECT content_hash FROM project_runner_setup WHERE runner_id = ? AND project_id = ?;

-- name: SetProjectRunnerSetupHash :exec
INSERT INTO project_runner_setup (runner_id, project_id, content_hash) VALUES (?, ?, ?)
ON CONFLICT (runner_id, project_id) DO UPDATE SET content_hash = excluded.content_hash;
