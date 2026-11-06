-- name: HealthFindings :many
SELECT f.* FROM health_findings f
WHERE f.organization_id=sqlc.arg(organization_id)
 AND EXISTS (SELECT 1 FROM health_finding_projects p WHERE p.organization_id=f.organization_id AND p.finding_id=f.id AND p.project_id=sqlc.arg(project_id))
 AND (sqlc.arg(state)::text='all' OR (f.resolved_at IS NOT NULL)=(sqlc.arg(state)::text='resolved'))
 AND ((sqlc.arg(state)::text='all' AND f.resolved_at IS NULL) OR sqlc.narg(since)::timestamptz IS NULL OR f.last_seen_at>=sqlc.narg(since) OR f.resolved_at>=sqlc.narg(since))
 AND f.sequence>sqlc.arg(after_sequence)
 AND (sqlc.arg(reporter)::text='' OR EXISTS (SELECT 1 FROM health_finding_issues l JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id WHERE l.organization_id=f.organization_id AND l.finding_id=f.id AND l.project_id=sqlc.arg(project_id) AND i.actor_principal_id=sqlc.arg(reporter)))
ORDER BY f.sequence LIMIT sqlc.arg(page_limit);

-- name: HealthSummaryFindings :many
SELECT * FROM health_findings WHERE organization_id=$1 AND resolved_at IS NULL AND severity='attention'
ORDER BY opened_at,id LIMIT $2;

-- name: HealthFindingProjects :many
SELECT finding_id,project_id FROM health_finding_projects
WHERE organization_id=$1 AND finding_id=ANY(sqlc.arg(finding_ids)::text[]) ORDER BY finding_id,project_id LIMIT 10001;

-- name: HealthFindingIssues :many
SELECT l.finding_id,l.project_id,l.work_item_id FROM health_finding_issues l
JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id
WHERE l.organization_id=sqlc.arg(organization_id) AND l.project_id=sqlc.arg(project_id) AND l.finding_id=ANY(sqlc.arg(finding_ids)::text[])
 AND (sqlc.arg(reporter)::text='' OR i.actor_principal_id=sqlc.arg(reporter))
ORDER BY l.finding_id,l.work_item_id LIMIT 10001;

-- name: HealthLastTick :one
SELECT last_tick_at FROM health_detector_ticks WHERE organization_id=$1;

-- name: HealthBaselines :many
SELECT * FROM health_baseline_unavailable WHERE organization_id=$1 AND project_id=$2 ORDER BY signal,lane LIMIT 10001;
