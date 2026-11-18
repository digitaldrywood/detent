-- name: ReadScheduledDecision :one
SELECT * FROM scheduled_ci_decisions
WHERE organization_id=$1 AND project_id=$2 AND repository=$3 AND run_id=$4 AND run_attempt=$5 AND job_id=$6;

-- name: StoreScheduledDecision :exec
INSERT INTO scheduled_ci_decisions(organization_id,project_id,repository,head_sha,run_id,run_attempt,job_id,outcome,target,reason,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING;

-- name: ScheduledLandedWorkItems :many
SELECT work_item_id FROM change_requests
WHERE organization_id=$1 AND project_id=$2 AND landed_merge_sha=$3 AND landed_base_ref='develop' LIMIT 2;
