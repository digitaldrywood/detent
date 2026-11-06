-- name: AccessAccountAuditID :one
SELECT id FROM hosted_audit
WHERE organization_id=$1 AND actual_actor=$2 AND account_kind='account_administration' AND account_retry=$3
ORDER BY id DESC LIMIT 1;

-- name: BindAccessTenantProvider :execrows
UPDATE hosted_tenant SET provider_id=$2
WHERE organization_id=$1 AND (provider_id='' OR provider_id=$2);

-- name: AccessMissingUsageGrant :one
SELECT EXISTS (
 SELECT 1 FROM projects p WHERE p.organization_id=$1 AND p.deleted_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.user_id=$2)
)::boolean;
