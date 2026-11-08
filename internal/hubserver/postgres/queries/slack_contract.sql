-- name: SlackIntegration :one
SELECT * FROM slack_integrations WHERE organization_id=$1;

-- name: CreateSlackIntegration :one
INSERT INTO slack_integrations(organization_id,channel_name,enabled,updated_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING RETURNING *;

-- name: UpdateSlackIntegration :one
UPDATE slack_integrations SET channel_name=$2,enabled=$3,revision=revision+1,updated_at=$4 WHERE organization_id=$1 AND revision=$5 RETURNING *;

-- name: RecordSlackStatus :execrows
UPDATE slack_integrations SET last_status_code=sqlc.arg(status_code),last_delivery_failed=sqlc.arg(failed),last_success_at=CASE WHEN sqlc.arg(failed) THEN last_success_at ELSE sqlc.arg(at)::timestamptz END,last_failure_at=CASE WHEN sqlc.arg(failed) THEN sqlc.arg(at)::timestamptz ELSE last_failure_at END
WHERE organization_id=sqlc.arg(organization_id) AND revision=sqlc.arg(expected_revision);

-- name: EnqueueSlackDelivery :one
INSERT INTO health_slack_deliveries(organization_id,id,finding_id,event,event_at,queued_at,next_attempt_at) VALUES($1,$2,$3,$4,$5,$6,$6) ON CONFLICT DO NOTHING RETURNING *;

-- name: SlackDelivery :one
SELECT * FROM health_slack_deliveries WHERE organization_id=$1 AND id=$2;

-- name: PendingSlackDeliveries :many
SELECT d.* FROM health_slack_deliveries d JOIN slack_integrations s ON s.organization_id=d.organization_id AND s.enabled
WHERE d.organization_id=$1 AND d.completed_at IS NULL AND d.next_attempt_at<=$2 ORDER BY d.next_attempt_at,d.id LIMIT $3;

-- name: ClaimSlackDelivery :one
UPDATE health_slack_deliveries SET attempts=attempts+1,next_attempt_at=sqlc.arg(next_attempt_at)
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND attempts=sqlc.arg(expected_attempts) AND completed_at IS NULL AND next_attempt_at<=sqlc.arg(now)::timestamptz AND sqlc.arg(next_attempt_at)>sqlc.arg(now) RETURNING *;

-- name: FinishSlackDelivery :one
UPDATE health_slack_deliveries SET outcome=sqlc.arg(outcome),external_receipt=sqlc.arg(external_receipt),completed_at=CASE WHEN sqlc.arg(outcome)::text='committed' THEN sqlc.arg(at)::timestamptz ELSE NULL END,status_code=sqlc.arg(status_code),next_attempt_at=sqlc.arg(next_attempt_at)
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND attempts=sqlc.arg(expected_attempts) AND completed_at IS NULL RETURNING *;
