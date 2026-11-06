-- name: MonthlyCostRevisions :many
SELECT s.project_id,s.source_key,s.payload_hash,r.* FROM cost_sources s
JOIN cost_observation_revisions r USING(organization_id,source_key,revision)
WHERE s.organization_id=sqlc.arg(organization_id) AND NOT s.voided
AND s.period_start < sqlc.arg(period_end) AND s.period_end > sqlc.arg(period_start)
AND (sqlc.arg(all_projects)::boolean OR s.project_id=ANY(sqlc.arg(projects)::text[]))
ORDER BY s.project_id,s.source_key LIMIT 5001;

-- name: MonthlyConversationUsage :many
SELECT * FROM conversation_usage WHERE organization_id=sqlc.arg(organization_id)
AND occurred_at>=sqlc.arg(period_start) AND occurred_at<sqlc.arg(period_end)
AND (sqlc.arg(all_projects)::boolean OR project_id=ANY(sqlc.arg(projects)::text[]))
ORDER BY project_id,occurred_at,turn_id LIMIT 5001;

-- name: MonthlySpriteResources :many
SELECT DISTINCT project_id,provider_account,name FROM (
 SELECT token_project_id AS project_id,provider_organization AS provider_account,name FROM runner_sprite_memberships membership WHERE membership.organization_id=sqlc.arg(organization_id) AND state<>'deleted'
 UNION
 SELECT g.project_id,ps.organization_slug,m.hostname FROM machines m JOIN runner_identities r ON r.organization_id=m.organization_id AND r.machine_id=m.id AND r.removed_at IS NULL
 JOIN token_grants g ON g.token_id=r.token_id AND g.organization_id=r.organization_id
 JOIN project_secrets ps ON ps.organization_id=g.organization_id AND ps.project_id=g.project_id AND ps.kind='fly_sprites_token'
 WHERE m.organization_id=sqlc.arg(organization_id) AND m.sprite_name=m.hostname
 UNION
 SELECT ''::text,os.organization_slug,m.hostname FROM machines m JOIN runner_identities r ON r.organization_id=m.organization_id AND r.machine_id=m.id AND r.removed_at IS NULL AND r.scope='organization'
 JOIN organization_secrets os ON os.organization_id=r.organization_id AND os.kind='fly_sprites_token'
 WHERE m.organization_id=sqlc.arg(organization_id) AND m.sprite_name=m.hostname
) resources WHERE (sqlc.arg(all_projects)::boolean OR project_id=ANY(sqlc.arg(projects)::text[]))
ORDER BY project_id,provider_account,name LIMIT 5001;

-- name: MonthlyAttemptCoverage :one
SELECT count(*) FILTER (WHERE status='running')::bigint AS active,
count(*) FILTER (WHERE NOT EXISTS(SELECT 1 FROM cost_sources c WHERE c.organization_id=a.organization_id AND c.resource_id=a.id AND c.bucket='runner_api'))::bigint AS unreported
FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=sqlc.arg(organization_id)
AND a.started_at<sqlc.arg(period_end) AND (a.status='running' OR a.updated_at>sqlc.arg(period_start))
AND (sqlc.arg(all_projects)::boolean OR i.project_id=ANY(sqlc.arg(projects)::text[]));
