-- name: HealthRecord :one
SELECT * FROM health_findings WHERE organization_id=$1 AND id=$2;

-- name: RecentHealthFingerprint :one
SELECT * FROM health_findings WHERE organization_id=$1 AND fingerprint=$2 AND (resolved_at IS NULL OR resolved_at>=$3)
ORDER BY resolved_at DESC NULLS FIRST,last_seen_at DESC,id LIMIT 1;

-- name: CreateHealthRecord :one
INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING *;

-- name: UpdateHealthRecord :one
UPDATE health_findings SET last_seen_at=sqlc.arg(last_seen_at),resolved_at=sqlc.narg(resolved_at),severity=sqlc.arg(severity),summary=sqlc.arg(summary),next_action=sqlc.arg(next_action),evidence_object_key=sqlc.arg(evidence_object_key),evidence_sha256=sqlc.arg(evidence_sha256),evidence_bytes=sqlc.arg(evidence_bytes),evidence_content_bytes=sqlc.arg(evidence_content_bytes),revision=revision+1,page_generation=page_generation+1,page_source_at=sqlc.arg(last_seen_at)
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND revision=sqlc.arg(expected_revision) AND last_seen_at<=sqlc.arg(last_seen_at) RETURNING *;

-- name: LinkHealthProject :exec
INSERT INTO health_finding_projects(organization_id,finding_id,project_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING;

-- name: LinkHealthIssue :exec
INSERT INTO health_finding_issues(organization_id,finding_id,project_id,work_item_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING;

-- name: HealthRecordProjects :many
SELECT * FROM health_finding_projects WHERE organization_id=$1 AND finding_id=$2 ORDER BY project_id LIMIT $3;

-- name: HealthRecordIssues :many
SELECT * FROM health_finding_issues WHERE organization_id=$1 AND finding_id=$2 AND work_item_id>$3 ORDER BY work_item_id,project_id LIMIT $4;

-- name: PutHealthBaseline :exec
INSERT INTO health_baseline_unavailable(organization_id,project_id,signal,lane) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING;

-- name: ClearHealthBaseline :exec
DELETE FROM health_baseline_unavailable WHERE organization_id=$1 AND project_id=$2 AND signal=$3 AND lane=$4;

-- name: PutHealthTick :exec
INSERT INTO health_detector_ticks(organization_id,last_tick_at) VALUES($1,$2) ON CONFLICT(organization_id) DO UPDATE SET last_tick_at=GREATEST(health_detector_ticks.last_tick_at,EXCLUDED.last_tick_at);

-- name: HealthTick :one
SELECT last_tick_at FROM health_detector_ticks WHERE organization_id=$1;
