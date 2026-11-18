-- name: RuntimePublished :one
SELECT (sqlc.arg(revision_id)::text='') OR EXISTS(SELECT 1 FROM public.hosted_runtime_revisions r WHERE r.organization_id=$1 AND r.id=sqlc.arg(revision_id) AND (r.local_published OR EXISTS(SELECT 1 FROM public.hosted_runtime_publications p WHERE p.id=r.id)));

-- name: CurrentRuntime :one
SELECT r.* FROM public.hosted_runtime_heads h
JOIN public.hosted_runtime_revisions staged ON staged.organization_id=h.organization_id AND staged.id=h.prepared_id
JOIN public.hosted_runtime_revisions r ON r.organization_id=h.organization_id AND r.id=CASE WHEN staged.local_published OR EXISTS(SELECT 1 FROM public.hosted_runtime_publications p WHERE p.id=h.prepared_id) THEN h.prepared_id ELSE h.active_id END
WHERE h.organization_id=$1;

-- name: LockRuntimeHead :one
SELECT * FROM public.hosted_runtime_heads WHERE organization_id=$1 FOR UPDATE;

-- name: InsertRuntimeRevision :exec
INSERT INTO public.hosted_runtime_revisions(organization_id,id,local_published,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13);

-- name: SetRuntimeHead :exec
INSERT INTO public.hosted_runtime_heads(organization_id,active_id,prepared_id) VALUES($1,$2,$3)
ON CONFLICT(organization_id) DO UPDATE SET active_id=EXCLUDED.active_id,prepared_id=EXCLUDED.prepared_id;

-- name: LockRuntimePublicationTargets :many
SELECT organization_id FROM public.hosted_runtime_heads WHERE organization_id=ANY(sqlc.arg(organizations)::text[]) AND prepared_id=sqlc.arg(revision)::text ORDER BY organization_id FOR SHARE;

-- name: PublishRuntime :exec
INSERT INTO public.hosted_runtime_publications(id) VALUES($1);

-- name: SetEntryRuntime :exec
INSERT INTO platform.runtime_heads(issuer,revision_id,object_key) VALUES($1,$2,$3)
ON CONFLICT(issuer) DO UPDATE SET revision_id=EXCLUDED.revision_id,object_key=EXCLUDED.object_key;

-- name: EntryRuntime :one
SELECT revision_id,object_key FROM platform.runtime_heads WHERE issuer=$1;

-- name: RuntimeTenantPage :many
SELECT id,generation FROM platform.organizations WHERE id>sqlc.arg(after_id)::text ORDER BY id LIMIT sqlc.arg(page_limit)::integer;

-- name: RuntimeAdoptionTarget :one
SELECT o.generation,o.managed,o.state,r.revision_id,r.object_key,COALESCE(h.prepared_id,'')::text AS prepared_revision
FROM platform.organizations o
JOIN platform.runtime_heads r ON r.issuer=sqlc.arg(issuer)::text
LEFT JOIN public.hosted_runtime_heads h ON h.organization_id=o.id
WHERE o.id=sqlc.arg(organization_id)::text;

-- name: RuntimePlanCatalog :many
SELECT p.id,p.version,p.name,p.monthly_usd_cents,p.runtime_id,COALESCE(c.metric,'')::text AS metric,COALESCE(c.amount,0)::bigint AS amount,COALESCE(c.feature,'')::text AS feature
FROM ROWS FROM (unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[])) q(id,version)
JOIN hosted_plans p ON p.organization_id=sqlc.arg(organization_id) AND p.id=q.id AND p.version=q.version
LEFT JOIN LATERAL (
 SELECT a.metric,a.amount,NULL::text AS feature FROM hosted_plan_allowances a WHERE a.organization_id=p.organization_id AND a.plan_id=p.id AND a.plan_version=p.version
 UNION ALL
 SELECT NULL::text,NULL::bigint,f.feature FROM hosted_plan_features f WHERE f.organization_id=p.organization_id AND f.plan_id=p.id AND f.plan_version=p.version
) c ON true
ORDER BY p.id,p.version;

-- name: UpsertRuntimePlans :exec
INSERT INTO hosted_plans(organization_id,id,version,name,monthly_usd_cents,runtime_id)
SELECT sqlc.arg(organization_id),id,version,name,CASE WHEN priced THEN price ELSE NULL END,sqlc.arg(revision_id)
FROM ROWS FROM (unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[]),unnest(sqlc.arg(names)::text[]),unnest(sqlc.arg(prices)::bigint[]),unnest(sqlc.arg(priced)::boolean[])) q(id,version,name,price,priced)
ON CONFLICT(organization_id,id,version) DO UPDATE SET name=EXCLUDED.name,monthly_usd_cents=EXCLUDED.monthly_usd_cents,runtime_id=EXCLUDED.runtime_id;

-- name: DeleteRuntimeAllowances :exec
DELETE FROM hosted_plan_allowances WHERE organization_id=sqlc.arg(organization_id) AND (plan_id,plan_version) IN (SELECT q.id,q.version FROM ROWS FROM (unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[])) q(id,version));

-- name: DeleteRuntimeFeatures :exec
DELETE FROM hosted_plan_features WHERE organization_id=sqlc.arg(organization_id) AND (plan_id,plan_version) IN (SELECT q.id,q.version FROM ROWS FROM (unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[])) q(id,version));

-- name: InsertRuntimeAllowances :exec
INSERT INTO hosted_plan_allowances(organization_id,plan_id,plan_version,metric,amount)
SELECT sqlc.arg(organization_id),id,version,metric,amount FROM ROWS FROM (unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[]),unnest(sqlc.arg(metrics)::text[]),unnest(sqlc.arg(amounts)::bigint[])) q(id,version,metric,amount);

-- name: InsertRuntimeFeatures :exec
INSERT INTO hosted_plan_features(organization_id,plan_id,plan_version,feature)
SELECT sqlc.arg(organization_id),id,version,feature FROM ROWS FROM (unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[]),unnest(sqlc.arg(features)::text[])) q(id,version,feature);

-- name: InsertRuntimePrices :many
INSERT INTO hosted_billing_prices(organization_id,price_id,plan_id,plan_version)
SELECT sqlc.arg(organization_id),price,id,version FROM ROWS FROM (unnest(sqlc.arg(prices)::text[]),unnest(sqlc.arg(ids)::text[]),unnest(sqlc.arg(versions)::bigint[])) q(price,id,version)
ON CONFLICT(organization_id,price_id) DO UPDATE SET plan_id=EXCLUDED.plan_id
WHERE hosted_billing_prices.plan_id=EXCLUDED.plan_id AND hosted_billing_prices.plan_version=EXCLUDED.plan_version
RETURNING price_id;

-- name: InsertRuntimeAudits :exec
INSERT INTO hosted_plan_audit(organization_id,command_id,request_hash,actor_id,action,recorded_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention)
SELECT sqlc.arg(organization_id),q.command,q.hash,q.actor,q.action,q.recorded,q.binding,q.customer_owned,q.object_key,q.digest,q.compressed,q.content,q.codec,q.format_version,q.kind,q.retention FROM ROWS FROM (unnest(sqlc.arg(commands)::text[]),unnest(sqlc.arg(hashes)::text[]),unnest(sqlc.arg(actors)::text[]),unnest(sqlc.arg(actions)::text[]),unnest(sqlc.arg(times)::timestamptz[]),unnest(sqlc.arg(bindings)::text[]),unnest(sqlc.arg(customer_owned)::boolean[]),unnest(sqlc.arg(keys)::text[]),unnest(sqlc.arg(digests)::text[]),unnest(sqlc.arg(compressed)::bigint[]),unnest(sqlc.arg(content)::bigint[]),unnest(sqlc.arg(codecs)::text[]),unnest(sqlc.arg(formats)::integer[]),unnest(sqlc.arg(kinds)::text[]),unnest(sqlc.arg(retentions)::text[])) q(command,hash,actor,action,recorded,binding,customer_owned,object_key,digest,compressed,content,codec,format_version,kind,retention);
