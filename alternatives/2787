-- name: BillingPlan :one
SELECT * FROM hosted_plans WHERE organization_id=$1 AND id=$2 AND version=$3;

-- name: BillingPlans :many
SELECT * FROM hosted_plans WHERE organization_id=$1 AND (id,version)>(sqlc.arg(after_id)::text,sqlc.arg(after_version)::bigint) ORDER BY id,version LIMIT sqlc.arg(page_size);

-- name: BillingPlanAllowances :many
SELECT metric,amount FROM hosted_plan_allowances WHERE organization_id=$1 AND plan_id=$2 AND plan_version=$3 ORDER BY metric;

-- name: BillingPlanFeatures :many
SELECT feature FROM hosted_plan_features WHERE organization_id=$1 AND plan_id=$2 AND plan_version=$3 ORDER BY feature;

-- name: CreateBillingPlan :exec
INSERT INTO hosted_plans(organization_id,id,version,name,monthly_usd_cents,runtime_id) VALUES($1,$2,$3,$4,$5,$6);

-- name: CreateBillingPlanAllowance :exec
INSERT INTO hosted_plan_allowances(organization_id,plan_id,plan_version,metric,amount) VALUES($1,$2,$3,$4,$5);

-- name: CreateBillingPlanFeature :exec
INSERT INTO hosted_plan_features(organization_id,plan_id,plan_version,feature) VALUES($1,$2,$3,$4);

-- name: BillingAssignment :one
SELECT * FROM hosted_plan_assignments WHERE organization_id=$1;

-- name: CreateBillingAssignment :exec
INSERT INTO hosted_plan_assignments(organization_id,base_id,base_version,subscription_id,subscription_version,subscription_expires_at,revision) VALUES($1,$2,$3,$4,$5,$6,1);

-- name: SetBillingAssignment :execrows
UPDATE hosted_plan_assignments SET base_id=$2,base_version=$3,subscription_id=$4,subscription_version=$5,subscription_expires_at=$6,revision=revision+1 WHERE organization_id=$1 AND revision=sqlc.arg(expected_revision);

-- name: BillingGrants :many
SELECT * FROM hosted_complimentary_grants WHERE organization_id=$1 AND id>sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(page_size);

-- name: BillingGrant :one
SELECT * FROM hosted_complimentary_grants WHERE organization_id=$1 AND id=$2;

-- name: BillingEffectiveGrants :many
SELECT g.organization_id,g.id,g.plan_id,g.plan_version,g.starts_at,g.expires_at,g.revoked_at,s.scope
FROM hosted_complimentary_grants g
LEFT JOIN hosted_complimentary_grant_scopes s ON s.organization_id=g.organization_id AND s.grant_id=g.id
WHERE g.organization_id=$1 AND g.revoked_at IS NULL AND g.starts_at<=$2 AND (g.expires_at IS NULL OR g.expires_at>$2)
ORDER BY g.id,s.scope;

-- name: BillingFeatureEnabled :one
SELECT EXISTS(
    SELECT 1
    FROM hosted_plan_assignments a
    JOIN hosted_plans p ON p.organization_id=a.organization_id AND p.id=CASE WHEN a.subscription_id IS NOT NULL AND a.subscription_expires_at>$3 THEN a.subscription_id ELSE a.base_id END AND p.version=CASE WHEN a.subscription_id IS NOT NULL AND a.subscription_expires_at>$3 THEN a.subscription_version ELSE a.base_version END
    JOIN hosted_plan_features f ON f.organization_id=p.organization_id AND f.plan_id=p.id AND f.plan_version=p.version
    WHERE a.organization_id=$1 AND f.feature=$2 AND (p.runtime_id='' OR EXISTS(SELECT 1 FROM hosted_runtime_revisions r WHERE r.organization_id=p.organization_id AND r.id=p.runtime_id AND (r.local_published OR EXISTS(SELECT 1 FROM hosted_runtime_publications pub WHERE pub.id=r.id))))
    UNION ALL
    SELECT 1
    FROM hosted_complimentary_grants g
    JOIN hosted_plans p ON p.organization_id=g.organization_id AND p.id=g.plan_id AND p.version=g.plan_version
    JOIN hosted_complimentary_grant_scopes s ON s.organization_id=g.organization_id AND s.grant_id=g.id
    LEFT JOIN hosted_plan_features f ON f.organization_id=p.organization_id AND f.plan_id=p.id AND f.plan_version=p.version AND f.feature=s.scope
    WHERE g.organization_id=$1 AND g.revoked_at IS NULL AND g.starts_at<=$3 AND (g.expires_at IS NULL OR g.expires_at>$3)
      AND (p.runtime_id='' OR EXISTS(SELECT 1 FROM hosted_runtime_revisions r WHERE r.organization_id=p.organization_id AND r.id=p.runtime_id AND (r.local_published OR EXISTS(SELECT 1 FROM hosted_runtime_publications pub WHERE pub.id=r.id))))
      AND ((s.scope=$2 AND (f.feature IS NOT NULL OR s.scope='model_choice')) OR ($2='native_execution' AND p.id||':'||p.version::text=ANY(sqlc.arg(capacity_plan_refs)::text[])))
);

-- name: BillingGrantScopes :many
SELECT scope FROM hosted_complimentary_grant_scopes WHERE organization_id=$1 AND grant_id=$2 ORDER BY scope;

-- name: CreateBillingGrant :exec
INSERT INTO hosted_complimentary_grants(organization_id,id,plan_id,plan_version,starts_at,expires_at,revoked_at) VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: CreateBillingGrantScope :exec
INSERT INTO hosted_complimentary_grant_scopes(organization_id,grant_id,scope) VALUES($1,$2,$3);

-- name: RevokeBillingGrant :execrows
UPDATE hosted_complimentary_grants SET revoked_at=COALESCE(revoked_at,$3) WHERE organization_id=$1 AND id=$2;
