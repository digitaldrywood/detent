-- name: CreateDetentRun :one
INSERT INTO detent_runs (
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

-- name: GetDetentRun :one
SELECT *
FROM detent_runs
WHERE id = ?;

-- name: UpdateDetentRun :execrows
UPDATE detent_runs
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
  requested_model,
  agent_backend_id,
  agent_backend_kind,
  agent_role,
  completed_at,
  turns,
  input_tokens,
  cached_input_tokens,
  output_tokens,
  reasoning_output_tokens,
  total_tokens,
  model_context_window,
  runtime_seconds,
  final_state,
  model,
  provider_thread_id,
  provider_session_id,
  resumed_from_session_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetCodexSession :one
SELECT *
FROM codex_sessions
WHERE id = ?;

-- name: FinishCodexSession :execrows
UPDATE codex_sessions
SET completed_at = sqlc.arg(completed_at),
    turns = sqlc.arg(turns),
    input_tokens = sqlc.arg(input_tokens),
    cached_input_tokens = sqlc.narg(cached_input_tokens),
    output_tokens = sqlc.arg(output_tokens),
    reasoning_output_tokens = sqlc.narg(reasoning_output_tokens),
    total_tokens = sqlc.arg(total_tokens),
    model_context_window = COALESCE(sqlc.narg(model_context_window), model_context_window),
    runtime_seconds = sqlc.arg(runtime_seconds),
    final_state = sqlc.narg(final_state),
    model = COALESCE(sqlc.narg(model), model),
    provider_thread_id = COALESCE(sqlc.narg(provider_thread_id), provider_thread_id),
    provider_session_id = COALESCE(sqlc.narg(provider_session_id), provider_session_id),
    resumed_from_session_id = COALESCE(sqlc.narg(resumed_from_session_id), resumed_from_session_id)
WHERE id = sqlc.arg(id);

-- name: GetLatestCompletedAgentResumeState :one
SELECT
  id,
  CAST(COALESCE(provider_thread_id, '') AS TEXT) AS provider_thread_id,
  CAST(COALESCE(provider_session_id, '') AS TEXT) AS provider_session_id,
  CAST(COALESCE(requested_model, '') AS TEXT) AS requested_model,
  CAST(COALESCE(model, '') AS TEXT) AS model,
  CAST(COALESCE(agent_backend_id, '') AS TEXT) AS agent_backend_id,
  CAST(COALESCE(agent_backend_kind, '') AS TEXT) AS agent_backend_kind,
  CAST(COALESCE(agent_role, '') AS TEXT) AS agent_role,
  CAST(completed_at AS TEXT) AS completed_at
FROM codex_sessions
WHERE completed_at IS NOT NULL
  AND lower(trim(COALESCE(final_state, ''))) = 'completed'
  AND (COALESCE(provider_thread_id, '') != '' OR COALESCE(provider_session_id, '') != '')
  AND COALESCE(agent_backend_id, '') = sqlc.arg(agent_backend_id)
  AND COALESCE(agent_backend_kind, '') = sqlc.arg(agent_backend_kind)
  AND COALESCE(agent_role, '') = sqlc.arg(agent_role)
  AND COALESCE(NULLIF(requested_model, ''), COALESCE(model, '')) = sqlc.arg(requested_model)
  AND (
    (sqlc.arg(issue_id) != '' AND COALESCE(issue_id, '') = sqlc.arg(issue_id))
    OR (sqlc.arg(identifier) != '' AND COALESCE(identifier, '') = sqlc.arg(identifier))
    OR (sqlc.arg(issue_url) != '' AND COALESCE(issue_url, '') = sqlc.arg(issue_url))
  )
ORDER BY completed_at DESC, id DESC
LIMIT 1;

-- name: ListRecentCodexSessions :many
SELECT *
FROM codex_sessions
ORDER BY completed_at DESC, id DESC
LIMIT ?;

-- name: CompletedIssueCycleRows :many
WITH issue_sessions AS (
  SELECT
    COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned') AS issue_key,
    started_at,
    completed_at,
    lower(trim(COALESCE(final_state, ''))) NOT IN ('failed', 'failure', 'cancelled', 'canceled') AS successful
  FROM codex_sessions
  WHERE started_at IS NOT NULL
    AND completed_at IS NOT NULL
),
successful_issues AS (
  SELECT
    issue_key,
    MAX(completed_at) AS completed_at
  FROM issue_sessions
  WHERE successful
  GROUP BY issue_key
)
SELECT
  CAST(issue_sessions.issue_key AS TEXT) AS issue_key,
  CAST(MIN(issue_sessions.started_at) AS TEXT) AS started_at,
  CAST(successful_issues.completed_at AS TEXT) AS completed_at,
  CAST(COUNT(*) AS INTEGER) AS sessions
FROM issue_sessions
JOIN successful_issues ON successful_issues.issue_key = issue_sessions.issue_key
WHERE issue_sessions.started_at <= successful_issues.completed_at
GROUP BY issue_sessions.issue_key, successful_issues.completed_at
ORDER BY completed_at DESC, issue_key;

-- name: LifetimeTotals :one
SELECT
  CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
  CAST(COALESCE(SUM(cached_input_tokens), 0) AS INTEGER) AS cached_input_tokens,
  CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(reasoning_output_tokens), 0) AS INTEGER) AS reasoning_output_tokens,
  CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
  CAST(COALESCE(SUM(runtime_seconds), 0) AS INTEGER) AS runtime_seconds,
  CAST(COUNT(*) AS INTEGER) AS sessions,
  CAST((SELECT COUNT(*) FROM detent_runs) AS INTEGER) AS runs
FROM codex_sessions
WHERE completed_at IS NOT NULL;

-- name: DailyTokenSpend :many
SELECT
  CAST(COALESCE(model, '') AS TEXT) AS model,
  CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
  CAST(COALESCE(SUM(cached_input_tokens), 0) AS INTEGER) AS cached_input_tokens,
  CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(reasoning_output_tokens), 0) AS INTEGER) AS reasoning_output_tokens,
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
  CAST(COALESCE(SUM(cached_input_tokens), 0) AS INTEGER) AS cached_input_tokens,
  CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(reasoning_output_tokens), 0) AS INTEGER) AS reasoning_output_tokens,
  CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
  CAST(COUNT(*) AS INTEGER) AS sessions
FROM codex_sessions
WHERE issue_id = sqlc.arg(issue_id)
   OR identifier = sqlc.arg(identifier)
   OR issue_url = sqlc.arg(issue_url)
GROUP BY COALESCE(model, '')
ORDER BY COALESCE(model, '');

-- name: RecentModelTokenQuantiles :one
WITH recent AS (
  SELECT
    input_tokens,
    COALESCE(cached_input_tokens, 0) AS cached_input_tokens,
    output_tokens,
    total_tokens
  FROM codex_sessions
  WHERE completed_at IS NOT NULL
    AND lower(trim(COALESCE(model, ''))) = lower(trim(sqlc.arg(model)))
  ORDER BY completed_at DESC, id DESC
  LIMIT sqlc.arg(limit)
),
counts AS (
  SELECT
    CAST(COUNT(*) AS INTEGER) AS sessions
  FROM recent
),
targets AS (
  SELECT
    sessions,
    CAST((sessions + 1) / 2 AS INTEGER) AS p50_rank,
    CAST(((sessions * 9) + 9) / 10 AS INTEGER) AS p90_rank
  FROM counts
),
ranked AS (
  SELECT
    input_tokens,
    cached_input_tokens,
    output_tokens,
    total_tokens,
    ROW_NUMBER() OVER (ORDER BY input_tokens) AS input_rank,
    ROW_NUMBER() OVER (ORDER BY cached_input_tokens) AS cached_input_rank,
    ROW_NUMBER() OVER (ORDER BY output_tokens) AS output_rank,
    ROW_NUMBER() OVER (ORDER BY total_tokens) AS total_rank
  FROM recent
)
SELECT
  CAST(targets.sessions AS INTEGER) AS sessions,
  CAST(COALESCE((SELECT input_tokens FROM ranked WHERE input_rank = targets.p50_rank), 0) AS INTEGER) AS p50_input_tokens,
  CAST(COALESCE((SELECT input_tokens FROM ranked WHERE input_rank = targets.p90_rank), 0) AS INTEGER) AS p90_input_tokens,
  CAST(COALESCE((SELECT cached_input_tokens FROM ranked WHERE cached_input_rank = targets.p50_rank), 0) AS INTEGER) AS p50_cached_input_tokens,
  CAST(COALESCE((SELECT cached_input_tokens FROM ranked WHERE cached_input_rank = targets.p90_rank), 0) AS INTEGER) AS p90_cached_input_tokens,
  CAST(COALESCE((SELECT output_tokens FROM ranked WHERE output_rank = targets.p50_rank), 0) AS INTEGER) AS p50_output_tokens,
  CAST(COALESCE((SELECT output_tokens FROM ranked WHERE output_rank = targets.p90_rank), 0) AS INTEGER) AS p90_output_tokens,
  CAST(COALESCE((SELECT total_tokens FROM ranked WHERE total_rank = targets.p50_rank), 0) AS INTEGER) AS p50_total_tokens,
  CAST(COALESCE((SELECT total_tokens FROM ranked WHERE total_rank = targets.p90_rank), 0) AS INTEGER) AS p90_total_tokens
FROM targets;

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
  cached_input_tokens,
  output_tokens,
  reasoning_output_tokens,
  total_tokens,
  model_context_window,
  cost_usd,
  runtime_seconds,
  started_at,
  finished_at,
  event_day,
  outcome
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetUsageEvent :one
SELECT *
FROM usage_events
WHERE id = ?;

-- name: UsageReportRows :many
WITH usage_report_rows AS (
  SELECT
    CASE
      WHEN sqlc.arg(bucket_by) = 'day' THEN event_day
      WHEN sqlc.arg(bucket_by) = 'project' THEN project_id
      WHEN sqlc.arg(bucket_by) = 'issue' THEN COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), 'unassigned')
      WHEN sqlc.arg(bucket_by) = 'pr' THEN project_id || '#' || COALESCE(CAST(pr_number AS TEXT), 'unassigned')
      WHEN sqlc.arg(bucket_by) = 'model' THEN COALESCE(NULLIF(model, ''), 'unassigned')
      ELSE event_day
    END AS group_key,
    COALESCE(NULLIF(model, ''), 'unassigned') AS model,
    input_tokens,
    cached_input_tokens,
    output_tokens,
    reasoning_output_tokens,
    total_tokens,
    model_context_window,
    runtime_seconds
  FROM usage_events
  WHERE (sqlc.narg(from_day) IS NULL OR event_day >= sqlc.narg(from_day))
    AND (sqlc.narg(to_day) IS NULL OR event_day <= sqlc.narg(to_day))
)
SELECT
  CAST(usage_report_rows.group_key AS TEXT) AS group_key,
  CAST(usage_report_rows.model AS TEXT) AS model,
  CAST(COALESCE(SUM(usage_report_rows.input_tokens), 0) AS INTEGER) AS input_tokens,
  CAST(COALESCE(SUM(usage_report_rows.cached_input_tokens), 0) AS INTEGER) AS cached_input_tokens,
  CAST(COALESCE(SUM(usage_report_rows.output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(usage_report_rows.reasoning_output_tokens), 0) AS INTEGER) AS reasoning_output_tokens,
  CAST(COALESCE(SUM(usage_report_rows.total_tokens), 0) AS INTEGER) AS total_tokens,
  CAST(COALESCE(MAX(usage_report_rows.model_context_window), 0) AS INTEGER) AS model_context_window,
  CAST(COALESCE(SUM(usage_report_rows.runtime_seconds), 0) AS INTEGER) AS runtime_seconds,
  CAST(COUNT(*) AS INTEGER) AS events
FROM usage_report_rows
GROUP BY usage_report_rows.group_key, usage_report_rows.model
ORDER BY usage_report_rows.group_key, usage_report_rows.model;

-- name: BudgetCostEvents :many
SELECT
  project_id,
  finished_at,
  cost_usd
FROM usage_events
WHERE finished_at >= sqlc.arg(from_time)
  AND finished_at < sqlc.arg(to_time)
ORDER BY finished_at, id;

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

-- name: CreateWorkflowPhaseEvent :one
INSERT INTO workflow_phase_events (
  project_id,
  run_id,
  session_id,
  issue_id,
  identifier,
  issue_url,
  pr_number,
  phase_type,
  phase_name,
  previous_phase_name,
  reason,
  status,
  started_at,
  finished_at,
  duration_seconds,
  event_day,
  command_name,
  exit_code,
  turns,
  input_tokens,
  cached_input_tokens,
  output_tokens,
  reasoning_output_tokens,
  total_tokens,
  model_context_window,
  endpoint_family,
  metadata_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: WorkflowPhaseDurationRows :many
SELECT *
FROM workflow_phase_events
WHERE finished_at IS NOT NULL
  AND (sqlc.narg(project_id) IS NULL OR project_id = sqlc.narg(project_id))
  AND (sqlc.narg(from_time) IS NULL OR finished_at >= sqlc.narg(from_time))
  AND (sqlc.narg(to_time) IS NULL OR finished_at < sqlc.narg(to_time))
ORDER BY project_id, phase_type, phase_name, finished_at, id;

-- name: WorkflowPhaseFlowRows :many
SELECT event.*
FROM workflow_phase_events AS event
WHERE event.finished_at IS NOT NULL
  AND event.phase_type IN ('agent_session', 'local_check', 'ci')
  AND (sqlc.narg(project_id) IS NULL OR event.project_id = sqlc.narg(project_id))
  AND EXISTS (
    SELECT 1
    FROM workflow_phase_events AS lane
    WHERE lane.finished_at IS NOT NULL
      AND lane.phase_type = 'lane'
      AND (sqlc.narg(project_id) IS NULL OR lane.project_id = sqlc.narg(project_id))
      AND (sqlc.narg(from_time) IS NULL OR lane.finished_at >= sqlc.narg(from_time))
      AND (sqlc.narg(to_time) IS NULL OR lane.finished_at < sqlc.narg(to_time))
      AND event.project_id = lane.project_id
      AND event.started_at < lane.finished_at
      AND event.finished_at > lane.started_at
      AND (
        (event.issue_id IS NOT NULL AND event.issue_id <> '' AND event.issue_id = lane.issue_id)
        OR (event.identifier IS NOT NULL AND event.identifier <> '' AND event.identifier = lane.identifier)
        OR (event.issue_url IS NOT NULL AND event.issue_url <> '' AND event.issue_url = lane.issue_url)
        OR (event.pr_number IS NOT NULL AND event.pr_number = lane.pr_number)
      )
  )
ORDER BY event.project_id, event.phase_type, event.phase_name, event.finished_at, event.id;

-- name: IssueWorkflowTimelineRows :many
SELECT *
FROM workflow_phase_events
WHERE issue_id = sqlc.arg(issue_id)
   OR identifier = sqlc.arg(identifier)
   OR issue_url = sqlc.arg(issue_url)
ORDER BY started_at, id;

-- name: CreateWorkAttempt :one
INSERT INTO work_attempts (
  project_id,
  issue_id,
  identifier,
  issue_url,
  pr_number,
  repo,
  worker_type,
  worker_host,
  lane,
  attempt_number,
  status,
  started_at,
  lease_expires_at,
  heartbeat_at,
  phase,
  status_message,
  current_step,
  total_steps,
  progress_percent,
  current_command,
  wait_reason,
  github_rate_snapshot_json,
  ci_state,
  capacity_snapshot_json,
  worker_metadata_json,
  metrics_json,
  next_action
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetWorkAttempt :one
SELECT *
FROM work_attempts
WHERE id = ?;

-- name: UpdateWorkAttemptHeartbeat :execrows
UPDATE work_attempts
SET heartbeat_at = ?,
    lease_expires_at = ?,
    phase = ?,
    status_message = ?,
    current_step = ?,
    total_steps = ?,
    progress_percent = ?,
    current_command = ?,
    wait_reason = ?,
    github_rate_snapshot_json = ?,
    ci_state = ?,
    capacity_snapshot_json = ?,
    metrics_json = ?,
    next_action = ?,
    error_class = ?,
    error_message = ?
WHERE id = ?
  AND completed_at IS NULL;

-- name: CompleteWorkAttempt :execrows
UPDATE work_attempts
SET status = ?,
    terminal_state = ?,
    completed_at = ?,
    heartbeat_at = ?,
    lease_expires_at = ?,
    error_class = ?,
    error_message = ?,
    phase = ?,
    status_message = ?,
    wait_reason = ?,
    github_rate_snapshot_json = ?,
    ci_state = ?,
    capacity_snapshot_json = ?,
    metrics_json = ?,
    next_action = ?
WHERE id = ?
  AND completed_at IS NULL;

-- name: ListActiveWorkAttempts :many
SELECT *
FROM work_attempts
WHERE completed_at IS NULL
  AND (sqlc.arg(filter_project_id) = '' OR project_id = sqlc.arg(filter_project_id))
ORDER BY started_at, id;

-- name: TimeoutExpiredWorkAttempts :many
UPDATE work_attempts
SET status = ?,
    terminal_state = ?,
    completed_at = ?,
    heartbeat_at = ?,
    error_class = ?,
    error_message = ?,
    phase = ?,
    status_message = ?
WHERE completed_at IS NULL
  AND (sqlc.arg(filter_project_id) = '' OR project_id = sqlc.arg(filter_project_id))
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at <= sqlc.arg(lease_expires_at)
RETURNING *;

-- name: ReclaimActiveWorkAttempts :many
UPDATE work_attempts
SET status = ?,
    terminal_state = ?,
    completed_at = ?,
    heartbeat_at = ?,
    error_class = ?,
    error_message = ?,
    phase = ?,
    status_message = ?
WHERE completed_at IS NULL
  AND project_id = ?
RETURNING *;

-- name: CreateSchedulerDecision :one
INSERT INTO scheduler_decisions (
  project_id,
  issue_id,
  identifier,
  issue_url,
  pr_number,
  repo,
  lane,
  queue_position,
  result,
  reason,
  selected,
  retry,
  attempt_number,
  worker_host,
  decision_at,
  wait_reason,
  capacity_snapshot_json,
  github_rate_snapshot_json,
  metadata_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListRecentSchedulerDecisions :many
SELECT *
FROM scheduler_decisions
WHERE sqlc.arg(filter_project_id) = '' OR project_id = sqlc.arg(filter_project_id)
ORDER BY decision_at DESC, id DESC
LIMIT sqlc.arg(limit);

-- name: UpsertValidatorVerdict :one
INSERT INTO validator_verdicts (
  project_id,
  issue_id,
  head_sha,
  identifier,
  issue_url,
  pr_number,
  submitted,
  verdict,
  score,
  summary,
  findings_json,
  commented,
  recorded_at,
  updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id, issue_id, head_sha) DO UPDATE SET
  identifier = excluded.identifier,
  issue_url = excluded.issue_url,
  pr_number = excluded.pr_number,
  submitted = excluded.submitted,
  verdict = excluded.verdict,
  score = excluded.score,
  summary = excluded.summary,
  findings_json = excluded.findings_json,
  commented = excluded.commented,
  recorded_at = excluded.recorded_at,
  updated_at = excluded.updated_at
RETURNING *;

-- name: GetValidatorVerdict :one
SELECT *
FROM validator_verdicts
WHERE project_id = ?
  AND issue_id = ?
  AND head_sha = ?;

-- name: MarkValidatorVerdictCommented :execrows
UPDATE validator_verdicts
SET commented = 1,
    updated_at = ?
WHERE project_id = ?
  AND issue_id = ?
  AND head_sha = ?;
