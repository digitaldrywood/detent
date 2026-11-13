-- name: DueGitHubReconciliations :many
WITH ready AS (
 SELECT r.project_id, r.repository AS stored_repository,
        lower(COALESCE(NULLIF(ps.checkout_repository,''),ps.repository))::text AS repository,
        ps.repository_enabled,
        r.node_id, r.after_work_item, r.revision, r.attempts, r.next_attempt_at,
        r.last_successful_sweep_at, r.last_failure_at
 FROM github_reconciliation r
 JOIN projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
 JOIN project_settings ps ON ps.organization_id=r.organization_id AND ps.project_id=r.project_id
 WHERE r.organization_id=sqlc.arg(organization_id) AND r.next_attempt_at<=sqlc.arg(due_at)::timestamptz
   AND p.deleted_at IS NULL AND p.profile='native'
   AND (ps.checkout_repository<>'' OR ps.repository<>'')
   AND (ps.repository_enabled OR sqlc.arg(linked_intake)::boolean)
 ORDER BY r.next_attempt_at,r.project_id LIMIT sqlc.arg(row_limit)
), initializing AS (
 SELECT ps.project_id, lower(COALESCE(NULLIF(ps.checkout_repository,''),ps.repository))::text AS repository, ps.repository_enabled
 FROM project_settings ps
 JOIN projects p ON p.organization_id=ps.organization_id AND p.id=ps.project_id
 WHERE ps.organization_id=sqlc.arg(organization_id) AND NOT ps.github_reconciliation_initialized
   AND p.deleted_at IS NULL AND p.profile='native'
   AND (ps.checkout_repository<>'' OR ps.repository<>'')
   AND (ps.repository_enabled OR sqlc.arg(linked_intake)::boolean)
 ORDER BY ps.project_id LIMIT sqlc.arg(row_limit)
)
SELECT project_id,stored_repository,repository,repository_enabled,node_id,after_work_item,revision,attempts,next_attempt_at,last_successful_sweep_at,last_failure_at
FROM (
 SELECT * FROM ready
 UNION ALL
 SELECT project_id,repository,repository,repository_enabled,''::text,''::text,0::bigint,0::bigint,sqlc.arg(due_at)::timestamptz,NULL::timestamptz,NULL::timestamptz
 FROM initializing
) candidates ORDER BY next_attempt_at,project_id LIMIT sqlc.arg(row_limit);

-- name: ReadGitHubReconciliation :one
SELECT r.*
FROM github_reconciliation r
JOIN projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
JOIN project_settings ps ON ps.organization_id=r.organization_id AND ps.project_id=r.project_id
WHERE r.organization_id=$1 AND r.project_id=$2 AND p.deleted_at IS NULL AND p.profile='native'
  AND lower(r.repository)=lower(COALESCE(NULLIF(ps.checkout_repository,''),ps.repository));

-- name: InitializeGitHubReconciliation :execrows
INSERT INTO github_reconciliation
 (organization_id,project_id,repository,node_id,after_work_item,revision,attempts,next_attempt_at,last_successful_sweep_at,last_failure_at)
SELECT ps.organization_id,ps.project_id,sqlc.arg(repository),sqlc.arg(node_id),sqlc.arg(after_work_item),1,
       sqlc.arg(attempts),sqlc.arg(next_attempt_at),sqlc.narg(last_successful_sweep_at),sqlc.narg(last_failure_at)
FROM project_settings ps
JOIN projects p ON p.organization_id=ps.organization_id AND p.id=ps.project_id
WHERE ps.organization_id=sqlc.arg(organization_id) AND ps.project_id=sqlc.arg(project_id)
  AND p.deleted_at IS NULL AND p.profile='native'
  AND sqlc.arg(repository)<>'' AND sqlc.arg(repository)=lower(COALESCE(NULLIF(ps.checkout_repository,''),ps.repository))
ON CONFLICT (organization_id,project_id) DO NOTHING;

-- name: MarkGitHubReconciliationInitialized :exec
UPDATE project_settings SET github_reconciliation_initialized=true WHERE organization_id=$1 AND project_id=$2;

-- name: AdvanceGitHubReconciliation :execrows
UPDATE github_reconciliation r SET
 repository=sqlc.arg(repository),
 node_id=sqlc.arg(node_id),
 after_work_item=sqlc.arg(after_work_item),
 revision=r.revision+1,
 attempts=sqlc.arg(attempts),
 next_attempt_at=sqlc.arg(next_attempt_at),
 last_successful_sweep_at=CASE WHEN lower(r.repository)=sqlc.arg(repository)
  THEN GREATEST(r.last_successful_sweep_at,sqlc.narg(last_successful_sweep_at)::timestamptz)
  ELSE sqlc.narg(last_successful_sweep_at)::timestamptz END,
 last_failure_at=CASE WHEN lower(r.repository)=sqlc.arg(repository)
  THEN GREATEST(r.last_failure_at,sqlc.narg(last_failure_at)::timestamptz)
  ELSE sqlc.narg(last_failure_at)::timestamptz END
FROM project_settings ps
JOIN projects p ON p.organization_id=ps.organization_id AND p.id=ps.project_id
WHERE r.organization_id=sqlc.arg(organization_id) AND r.project_id=sqlc.arg(project_id)
  AND ps.organization_id=r.organization_id AND ps.project_id=r.project_id
  AND p.deleted_at IS NULL AND p.profile='native' AND r.revision=sqlc.arg(expected_revision)
  AND sqlc.arg(repository)<>'' AND sqlc.arg(repository)=lower(COALESCE(NULLIF(ps.checkout_repository,''),ps.repository))
  AND (lower(r.repository)<>sqlc.arg(repository) OR r.node_id=sqlc.arg(node_id) OR r.node_id='');

