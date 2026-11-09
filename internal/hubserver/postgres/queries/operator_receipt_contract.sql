-- name: ReserveOperatorReceipt :execrows
INSERT INTO native_command_states(organization_id,actor_id,operation,command_key,request_hash,created_at,response_claimed,response_pending,reservation_project_id,reservation_correlation_id)
VALUES($1,$2,$3,$4,$5,$6,true,true,$7,$8)
ON CONFLICT DO NOTHING;

-- name: OperatorReceiptCustody :one
SELECT coalesce(reservation_project_id,'')::text AS project_id,
coalesce(reservation_correlation_id,'')::text AS correlation_id,reservation_claimed_at AS claimed_at
FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4;

-- name: ClaimReservedOperatorReceipt :execrows
UPDATE native_command_states SET reservation_claimed_at=sqlc.arg(claimed_at)::timestamptz
WHERE organization_id=sqlc.arg(organization_id) AND actor_id=sqlc.arg(actor_id)
AND operation=sqlc.arg(operation) AND command_key=sqlc.arg(command_key)
AND request_hash=sqlc.arg(request_hash) AND reservation_project_id=sqlc.arg(project_id)::text
AND reservation_correlation_id=sqlc.arg(correlation_id)::text AND reservation_claimed_at IS NULL
AND response_claimed AND response_pending AND NOT response_expired;

-- name: RetainOperatorReceiptReplay :execrows
UPDATE native_command_states SET response_replay_until_ns=0
WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4
AND request_hash=$5 AND reservation_project_id=$6 AND reservation_correlation_id=$7
AND NOT response_claimed AND NOT response_pending AND NOT response_expired;
