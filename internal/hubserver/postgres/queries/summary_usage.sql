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

-- name: ReadCompletionImportAttempts :many
WITH requested AS MATERIALIZED (
 SELECT p.project_id,(sqlc.arg(work_item_ids)::text[])[p.ordinal] AS work_item_id
 FROM unnest(sqlc.arg(project_ids)::text[]) WITH ORDINALITY AS p(project_id,ordinal) LIMIT sqlc.arg(item_count)
), items AS MATERIALIZED (
 SELECT i.project_id,i.native_id AS work_item_id,i.id
 FROM requested r CROSS JOIN LATERAL (
  SELECT i.project_id,i.native_id,i.id,i.actor_principal_id FROM issues i
  WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=r.project_id AND i.native_id=r.work_item_id LIMIT 1
 ) i
 WHERE sqlc.arg(principal)::text='' OR i.actor_principal_id=sqlc.arg(principal)
), attempt_ids AS MATERIALIZED (
 SELECT a.issue_id,a.id FROM native_attempts a
 WHERE a.organization_id=sqlc.arg(organization_id) AND a.issue_id=ANY(ARRAY(SELECT id FROM items))
 ORDER BY a.issue_id,a.started_at,a.started_at_submicro_ns,a.id LIMIT 50001
)
SELECT a.id, (NOT sqlc.arg(has_sources)::boolean OR coalesce((SELECT false FROM cost_sources c WHERE c.organization_id=sqlc.arg(organization_id) AND c.bucket='runner_api' AND c.resource_id=a.id LIMIT 1),true))::boolean AS imported
FROM attempt_ids a;

-- name: ReadCompletionCosts :many
WITH requested AS MATERIALIZED (
 SELECT p.project_id,(sqlc.arg(work_item_ids)::text[])[p.ordinal] AS work_item_id
 FROM unnest(sqlc.arg(project_ids)::text[]) WITH ORDINALITY AS p(project_id,ordinal) LIMIT sqlc.arg(item_count)
), items AS MATERIALIZED (
 SELECT i.project_id,i.native_id AS work_item_id,i.id
 FROM requested r CROSS JOIN LATERAL (
  SELECT i.project_id,i.native_id,i.id,i.actor_principal_id FROM issues i
  WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=r.project_id AND i.native_id=r.work_item_id LIMIT 1
 ) i
 WHERE sqlc.arg(principal)::text='' OR i.actor_principal_id=sqlc.arg(principal)
), attempt_ids AS MATERIALIZED (
 SELECT a.issue_id,a.id FROM native_attempts a
 WHERE a.organization_id=sqlc.arg(organization_id) AND a.issue_id=ANY(ARRAY(SELECT id FROM items))
 ORDER BY a.issue_id,a.started_at,a.started_at_submicro_ns,a.id LIMIT 50001
), attempts AS MATERIALIZED (
 SELECT i.project_id,i.work_item_id,a.id AS attempt_id FROM attempt_ids a JOIN items i ON i.id=a.issue_id
), first_sources AS MATERIALIZED (
 SELECT c.resource_id,c.source_key,c.project_id,c.revision FROM cost_sources c
 WHERE c.organization_id=sqlc.arg(organization_id) AND c.bucket='runner_api' AND c.resource_id=ANY(ARRAY(SELECT attempt_id FROM attempts))
 ORDER BY c.resource_id,c.source_key LIMIT 50000
), next_sources AS MATERIALIZED (
 SELECT c.resource_id,c.source_key,c.project_id,c.revision FROM cost_sources c
 WHERE c.organization_id=sqlc.arg(organization_id) AND c.bucket='runner_api' AND c.resource_id=ANY(ARRAY(SELECT attempt_id FROM attempts)) AND (SELECT count(*) FROM first_sources)=50000 AND (c.resource_id,c.source_key)>(
  SELECT resource_id,source_key FROM first_sources ORDER BY resource_id DESC,source_key DESC LIMIT 1
 ) ORDER BY c.resource_id,c.source_key LIMIT 50001
), projected_sources AS MATERIALIZED (
 SELECT c.*,u.project_id AS usage_project,u.attempt_id AS usage_attempt,u.revision AS projected_revision,u.complete,u.period,u.cost_estimate
 FROM (SELECT * FROM first_sources UNION ALL SELECT * FROM next_sources) c
 LEFT JOIN LATERAL (SELECT u.project_id,u.attempt_id,u.revision,u.complete,u.period,u.cost_estimate FROM report_usage_current u WHERE u.organization_id=sqlc.arg(organization_id) AND u.source_key=c.source_key LIMIT 1) u ON true
), source_rows AS MATERIALIZED (
 SELECT a.attempt_id,c.revision,c.projected_revision,c.complete,c.period,c.cost_estimate,
 c.project_id=a.project_id AND c.usage_project=a.project_id AND c.usage_attempt=a.attempt_id AS scoped
 FROM projected_sources c JOIN attempts a ON a.attempt_id=c.resource_id
), source_costs AS MATERIALIZED (
 SELECT attempt_id,count(*) AS source_count,
 bool_and(coalesce(complete AND projected_revision=revision AND scoped,false)) AS complete,
 coalesce(sum(cost_estimate) FILTER(WHERE scoped AND period<sqlc.arg(current_to)::timestamptz),0)::double precision AS cost,
 coalesce(sum(cost_estimate) FILTER(WHERE scoped AND period<sqlc.arg(prior_to)::timestamptz),0)::double precision AS prior_cost
 FROM source_rows GROUP BY attempt_id
), import_keys AS MATERIALIZED (
 SELECT k.* FROM ROWS FROM(unnest(sqlc.arg(usage_attempts)::text[]),unnest(sqlc.arg(usage_periods)::timestamptz[]),unnest(sqlc.arg(usage_providers)::text[]),unnest(sqlc.arg(usage_models)::text[]),unnest(sqlc.arg(usage_currencies)::text[]),unnest(sqlc.arg(usage_origins)::text[])) AS k(attempt_id,period,provider,model,currency,usage_origin)
), usage_rows AS MATERIALIZED (
 SELECT k.attempt_id,u.project_id,k.period,u.cost_estimate,k.currency FROM import_keys k
 CROSS JOIN LATERAL (SELECT u.project_id,u.cost_estimate FROM attempt_usage u WHERE u.organization_id=sqlc.arg(organization_id) AND u.attempt_id=k.attempt_id AND u.period=k.period AND u.provider=k.provider AND u.model=k.model AND u.currency=k.currency AND u.usage_origin=k.usage_origin LIMIT 1) u
), import_rows AS MATERIALIZED (
 SELECT u.attempt_id,a.project_id=u.project_id AS scoped,u.period,u.cost_estimate,u.currency FROM usage_rows u JOIN attempts a ON a.attempt_id=u.attempt_id
), import_costs AS MATERIALIZED (
 SELECT attempt_id,count(*) AS actual,
 bool_and(scoped AND upper(currency)='USD') AS scoped,
 coalesce(sum(cost_estimate) FILTER(WHERE scoped AND upper(currency)='USD' AND period<sqlc.arg(current_to)::timestamptz),0)::double precision AS cost,
 coalesce(sum(cost_estimate) FILTER(WHERE scoped AND upper(currency)='USD' AND period<sqlc.arg(prior_to)::timestamptz),0)::double precision AS prior_cost
 FROM import_rows GROUP BY attempt_id
), attempt_costs AS MATERIALIZED (
 SELECT a.project_id,a.work_item_id,
 CASE WHEN c.attempt_id IS NOT NULL THEN c.cost ELSE coalesce(u.cost,0) END AS cost,
 CASE WHEN c.attempt_id IS NOT NULL THEN c.prior_cost ELSE coalesce(u.prior_cost,0) END AS prior_cost,
 CASE WHEN c.attempt_id IS NOT NULL THEN c.complete ELSE coalesce(u.scoped AND u.actual=v.row_count AND v.row_count>0,false) END AS complete
 FROM attempts a LEFT JOIN source_costs c ON c.attempt_id=a.attempt_id
 LEFT JOIN import_costs u ON u.attempt_id=a.attempt_id
 LEFT JOIN LATERAL (SELECT v.row_count FROM report_usage_import_coverage v WHERE v.organization_id=sqlc.arg(organization_id) AND v.attempt_id=a.attempt_id LIMIT 1) v ON true
), totals AS MATERIALIZED (
 SELECT project_id,work_item_id,sum(cost)::double precision AS cost,sum(prior_cost)::double precision AS prior_cost,bool_and(complete) AS complete
 FROM attempt_costs GROUP BY project_id,work_item_id
), limits AS MATERIALIZED (
 SELECT (SELECT count(*) FROM attempts)>50000 OR (SELECT count(*) FROM source_rows)>100000 AS exceeded
)
SELECT i.project_id,i.work_item_id,
 coalesce(t.cost,0)::double precision AS cost,coalesce(t.prior_cost,0)::double precision AS prior_cost,
 coalesce(t.complete,true)::boolean AS complete,(SELECT exceeded FROM limits)::boolean AS exceeded
FROM items i LEFT JOIN totals t ON t.project_id=i.project_id AND t.work_item_id=i.work_item_id
ORDER BY i.project_id,i.work_item_id;

-- name: ReadCompletionImportCosts :many
WITH requested AS MATERIALIZED (
 SELECT p.project_id,(sqlc.arg(work_item_ids)::text[])[p.ordinal] AS work_item_id
 FROM unnest(sqlc.arg(project_ids)::text[]) WITH ORDINALITY AS p(project_id,ordinal) LIMIT sqlc.arg(item_count)
), items AS MATERIALIZED (
 SELECT i.project_id,i.native_id AS work_item_id,i.id
 FROM requested r CROSS JOIN LATERAL (
  SELECT i.project_id,i.native_id,i.id,i.actor_principal_id FROM issues i
  WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=r.project_id AND i.native_id=r.work_item_id LIMIT 1
 ) i
 WHERE sqlc.arg(principal)::text='' OR i.actor_principal_id=sqlc.arg(principal)
), attempt_ids AS MATERIALIZED (
 SELECT a.issue_id,a.id FROM native_attempts a
 WHERE a.organization_id=sqlc.arg(organization_id) AND a.issue_id=ANY(ARRAY(SELECT id FROM items))
 ORDER BY a.issue_id,a.started_at,a.started_at_submicro_ns,a.id LIMIT 50001
), attempts AS MATERIALIZED (
 SELECT i.project_id,i.work_item_id,a.id AS attempt_id FROM attempt_ids a JOIN items i ON i.id=a.issue_id
), import_keys AS MATERIALIZED (
 SELECT k.* FROM ROWS FROM(unnest(sqlc.arg(usage_attempts)::text[]),unnest(sqlc.arg(usage_periods)::timestamptz[]),unnest(sqlc.arg(usage_providers)::text[]),unnest(sqlc.arg(usage_models)::text[]),unnest(sqlc.arg(usage_currencies)::text[]),unnest(sqlc.arg(usage_origins)::text[])) AS k(attempt_id,period,provider,model,currency,usage_origin)
), usage_rows AS MATERIALIZED (
 SELECT k.attempt_id,u.project_id,k.period,u.cost_estimate,k.currency FROM import_keys k
 CROSS JOIN LATERAL (SELECT u.project_id,u.cost_estimate FROM attempt_usage u WHERE u.organization_id=sqlc.arg(organization_id) AND u.attempt_id=k.attempt_id AND u.period=k.period AND u.provider=k.provider AND u.model=k.model AND u.currency=k.currency AND u.usage_origin=k.usage_origin LIMIT 1) u
), import_rows AS MATERIALIZED (
 SELECT u.attempt_id,a.project_id=u.project_id AS scoped,u.period,u.cost_estimate,u.currency FROM usage_rows u JOIN attempts a ON a.attempt_id=u.attempt_id
), import_costs AS MATERIALIZED (
 SELECT attempt_id,count(*) AS actual,
 bool_and(scoped AND upper(currency)='USD') AS scoped,
 coalesce(sum(cost_estimate) FILTER(WHERE scoped AND upper(currency)='USD' AND period<sqlc.arg(current_to)::timestamptz),0)::double precision AS cost,
 coalesce(sum(cost_estimate) FILTER(WHERE scoped AND upper(currency)='USD' AND period<sqlc.arg(prior_to)::timestamptz),0)::double precision AS prior_cost
 FROM import_rows GROUP BY attempt_id
), attempt_costs AS MATERIALIZED (
 SELECT a.project_id,a.work_item_id,coalesce(u.cost,0) AS cost,coalesce(u.prior_cost,0) AS prior_cost,
 coalesce(u.scoped AND u.actual=v.row_count AND v.row_count>0,false) AS complete
 FROM attempts a LEFT JOIN import_costs u ON u.attempt_id=a.attempt_id
 LEFT JOIN LATERAL (SELECT v.row_count FROM report_usage_import_coverage v WHERE v.organization_id=sqlc.arg(organization_id) AND v.attempt_id=a.attempt_id LIMIT 1) v ON true
), totals AS MATERIALIZED (
 SELECT project_id,work_item_id,sum(cost)::double precision AS cost,sum(prior_cost)::double precision AS prior_cost,bool_and(complete) AS complete
 FROM attempt_costs GROUP BY project_id,work_item_id
), limits AS MATERIALIZED (
 SELECT (SELECT count(*) FROM attempts)>50000 AS exceeded
)
SELECT i.project_id,i.work_item_id,
 coalesce(t.cost,0)::double precision AS cost,coalesce(t.prior_cost,0)::double precision AS prior_cost,
 coalesce(t.complete,true)::boolean AS complete,(SELECT exceeded FROM limits)::boolean AS exceeded
FROM items i LEFT JOIN totals t ON t.project_id=i.project_id AND t.work_item_id=i.work_item_id
ORDER BY i.project_id,i.work_item_id;
