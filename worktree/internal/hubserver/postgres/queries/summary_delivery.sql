-- name: DueSummaries :many
WITH due AS MATERIALIZED (
 SELECT d.* FROM daily_summary_preferences d WHERE d.organization_id=$1 AND d.enabled AND d.due_at<=$2 ORDER BY d.due_at,d.user_id LIMIT 16
)
SELECT p.user_id,p.organization_id,p.send_minute,p.time_zone,p.weekdays,p.window_end,p.token,m.email,m.principal_id,m.role,(m.active AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now_at)::timestamptz))::boolean AS eligible
FROM due p JOIN hosted_members m ON m.organization_id=p.organization_id AND m.user_id=p.user_id
JOIN api_tokens t ON t.organization_id=m.organization_id AND t.id=m.principal_id ORDER BY p.due_at,p.user_id;

-- name: SummaryStatus :one
SELECT status FROM daily_summary_receipts WHERE organization_id=$1 AND user_id=$2 AND window_end=$3;

-- name: ClaimSummary :execrows
INSERT INTO daily_summary_receipts(organization_id,user_id,window_end,status,recorded_at)
SELECT p.organization_id,p.user_id,p.window_end,sqlc.arg(status),sqlc.arg(recorded_at)::timestamptz FROM daily_summary_preferences p
WHERE p.organization_id=sqlc.arg(organization_id) AND p.user_id=sqlc.arg(user_id) AND p.window_end=sqlc.arg(window_end) AND p.enabled AND p.due_at<=sqlc.arg(recorded_at)
ON CONFLICT(organization_id,user_id,window_end) DO UPDATE SET status=excluded.status,attempts=daily_summary_receipts.attempts+1,recorded_at=excluded.recorded_at WHERE daily_summary_receipts.status='rejected';

-- name: FinishSummary :exec
UPDATE daily_summary_receipts SET status=$4,recorded_at=$5 WHERE organization_id=$1 AND user_id=$2 AND window_end=$3 AND status='uncertain';

-- name: RetrySummary :exec
UPDATE daily_summary_preferences SET due_at=sqlc.arg(now_at)::timestamptz+(LEAST(21600,30*(1<<LEAST(10,(SELECT r.attempts FROM daily_summary_receipts r WHERE r.organization_id=sqlc.arg(organization_id) AND r.user_id=sqlc.arg(user_id) AND r.window_end=sqlc.arg(window_end)))))+sqlc.arg(jitter)::bigint)*interval '1 second'
WHERE organization_id=sqlc.arg(organization_id) AND user_id=sqlc.arg(user_id) AND window_end=sqlc.arg(window_end);

-- name: AdvanceSummary :exec
UPDATE daily_summary_preferences SET window_end=$4,due_at=$5 WHERE organization_id=$1 AND user_id=$2 AND window_end=$3;
