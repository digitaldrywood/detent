-- name: ReadSummaryUsage :many
WITH usage AS (
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,'USD'::text AS currency
 FROM report_usage_current u
 WHERE u.organization_id=sqlc.arg(organization_id) AND u.project_id=sqlc.arg(project_id) AND u.period<sqlc.arg(end_at)
 AND (u.period>=sqlc.arg(start_at) OR EXISTS(SELECT 1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=u.organization_id AND a.id=u.attempt_id AND i.project_id=u.project_id AND i.native_id=ANY(sqlc.arg(lifetime_ids)::text[])))
 UNION ALL
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency
 FROM attempt_usage u
 WHERE u.organization_id=sqlc.arg(organization_id) AND u.project_id=sqlc.arg(project_id) AND u.period<sqlc.arg(end_at)
 AND (u.period>=sqlc.arg(start_at) OR EXISTS(SELECT 1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=u.organization_id AND a.id=u.attempt_id AND i.project_id=u.project_id AND i.native_id=ANY(sqlc.arg(lifetime_ids)::text[])))
 AND NOT EXISTS(SELECT 1 FROM report_usage_current c WHERE c.organization_id=u.organization_id AND c.attempt_id=u.attempt_id)
)
SELECT i.native_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency
FROM usage u JOIN native_attempts a ON a.organization_id=u.organization_id AND a.id=u.attempt_id
JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=u.project_id
WHERE (sqlc.arg(principal)::text='' OR i.actor_principal_id=sqlc.arg(principal)) AND upper(u.currency)='USD'
ORDER BY u.period,u.attempt_id,u.provider,u.model LIMIT 100001;
