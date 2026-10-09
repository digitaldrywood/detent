-- name: CreateWorkerGeneration :one
INSERT INTO worker_generations (
  pid,
  process_start,
  version,
  state,
  started_at,
  updated_at
) VALUES (?, ?, ?, ?, ?, ?)
RETURNING gen;

-- name: ListUnexitedWorkerGenerations :many
SELECT gen, pid, process_start
FROM worker_generations
WHERE state != 'exited'
  AND exited_at IS NULL
ORDER BY gen;

-- name: ExitWorkerGenerations :execrows
UPDATE worker_generations
SET state = sqlc.arg(state),
    exited_at = sqlc.arg(exited_at),
    updated_at = sqlc.arg(exited_at)
WHERE exited_at IS NULL
  AND gen IN (SELECT value FROM json_each(sqlc.arg(generations)));
