-- name: RecordGetReleaseDelivery :one
SELECT * FROM release_deliveries
WHERE organization_id = $1 AND project_id = $2 AND work_item_id = $3;
