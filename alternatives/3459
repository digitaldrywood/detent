-- name: DuplicateProjectLock :one
SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE;

-- name: AdvanceDuplicateSource :execrows
UPDATE projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=$1 AND id=$2;
