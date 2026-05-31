-- name: CreateSymphonyRun :one
INSERT INTO symphony_runs (
  started_at,
  stopped_at,
  restart_reason,
  peak_concurrent_agents,
  sessions_launched,
  input_tokens,
  output_tokens,
  total_tokens,
  runtime_seconds
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetSymphonyRun :one
SELECT *
FROM symphony_runs
WHERE id = ?;

-- name: UpdateSymphonyRun :execrows
UPDATE symphony_runs
SET stopped_at = COALESCE(?, stopped_at),
    restart_reason = COALESCE(?, restart_reason),
    peak_concurrent_agents = ?,
    sessions_launched = ?,
    input_tokens = ?,
    output_tokens = ?,
    total_tokens = ?,
    runtime_seconds = ?
WHERE id = ?;

-- name: CreateCodexSession :one
INSERT INTO codex_sessions (
  run_id,
  issue_id,
  identifier,
  issue_url,
  started_at,
  completed_at,
  turns,
  input_tokens,
  output_tokens,
  total_tokens,
  runtime_seconds,
  final_state,
  model
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetCodexSession :one
SELECT *
FROM codex_sessions
WHERE id = ?;

-- name: FinishCodexSession :execrows
UPDATE codex_sessions
SET completed_at = ?,
    turns = ?,
    input_tokens = ?,
    output_tokens = ?,
    total_tokens = ?,
    runtime_seconds = ?,
    final_state = ?,
    model = COALESCE(?, model)
WHERE id = ?;

-- name: ListRecentCodexSessions :many
SELECT *
FROM codex_sessions
ORDER BY completed_at DESC, id DESC
LIMIT ?;

-- name: DailyTokenSpend :many
SELECT
  CAST(COALESCE(model, '') AS TEXT) AS model,
  CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
  CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
  CAST(COUNT(*) AS INTEGER) AS sessions
FROM codex_sessions
WHERE substr(completed_at, 1, 10) = ?
GROUP BY COALESCE(model, '')
ORDER BY COALESCE(model, '');

-- name: IssueTokenSpend :many
SELECT
  CAST(COALESCE(model, '') AS TEXT) AS model,
  CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
  CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
  CAST(COUNT(*) AS INTEGER) AS sessions
FROM codex_sessions
WHERE issue_id = sqlc.arg(issue_id)
   OR identifier = sqlc.arg(identifier)
   OR issue_url = sqlc.arg(issue_url)
GROUP BY COALESCE(model, '')
ORDER BY COALESCE(model, '');

-- name: CreateUsageEvent :one
INSERT INTO usage_events (
  project_id,
  run_id,
  session_id,
  issue_id,
  identifier,
  pr_number,
  model,
  input_tokens,
  output_tokens,
  total_tokens,
  runtime_seconds,
  started_at,
  finished_at,
  event_day,
  outcome
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetUsageEvent :one
SELECT *
FROM usage_events
WHERE id = ?;

-- name: ListFairShareUsage :many
SELECT
  project_id,
  weight,
  dispatches,
  runtime_seconds,
  updated_at
FROM fair_share_usage
ORDER BY project_id;

-- name: UpsertFairShareUsage :one
INSERT INTO fair_share_usage (
  project_id,
  weight,
  dispatches,
  runtime_seconds,
  updated_at
) VALUES (?, ?, 1, ?, ?)
ON CONFLICT(project_id) DO UPDATE SET
  weight = excluded.weight,
  dispatches = fair_share_usage.dispatches + excluded.dispatches,
  runtime_seconds = fair_share_usage.runtime_seconds + excluded.runtime_seconds,
  updated_at = excluded.updated_at
RETURNING *;
