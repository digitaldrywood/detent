-- name: ActiveBudgetOverride :one
SELECT * FROM project_budget_overrides WHERE organization_id=$1 AND project_id=$2 AND expires_at>$3;

-- name: CurrentBudgetOverride :one
SELECT * FROM project_budget_overrides WHERE organization_id=$1 AND project_id=$2;

-- name: SetBudgetOverride :one
INSERT INTO project_budget_overrides(organization_id,project_id,per_day_max_usd,per_issue_max_usd,expires_at,created_at,reason) VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT(organization_id,project_id) DO UPDATE SET per_day_max_usd=EXCLUDED.per_day_max_usd,per_issue_max_usd=EXCLUDED.per_issue_max_usd,expires_at=EXCLUDED.expires_at,created_at=EXCLUDED.created_at,reason=EXCLUDED.reason,revision=project_budget_overrides.revision+1 RETURNING *;

-- name: ListActiveBudgetOverrides :many
SELECT * FROM project_budget_overrides WHERE organization_id=$1 AND expires_at>$2 AND project_id>$3 ORDER BY project_id LIMIT $4;

-- name: ClearBudgetOverride :execrows
DELETE FROM project_budget_overrides WHERE organization_id=$1 AND project_id=$2;
