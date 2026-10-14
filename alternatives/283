-- name: LaneLedgerIdentity :one
SELECT identity FROM lane_ledger_instance WHERE id = 1;

-- name: PrepareLaneWrite :one
INSERT INTO lane_ledger (project_id, issue_id, from_state, to_state, reason, written_at)
VALUES (?, ?, ?, ?, ?, ?) RETURNING *;

-- name: ResolveLaneWrite :execrows
UPDATE lane_ledger SET result = ?, resolved_at = ? WHERE id = ?;

-- name: RecordLaneWriteAction :execrows
UPDATE lane_ledger SET origin = ?, action_kind = ? WHERE id = ?;

-- name: LatestLaneWrite :one
SELECT * FROM lane_ledger WHERE project_id = ? AND issue_id = ? ORDER BY id DESC LIMIT 1;

-- name: LaneObservation :one
SELECT * FROM lane_observations WHERE project_id = ? AND issue_id = ?;

-- name: SaveLaneObservation :exec
INSERT INTO lane_observations (project_id, issue_id, state, entered_at, origin)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (project_id, issue_id) DO UPDATE SET state = excluded.state, entered_at = excluded.entered_at, origin = excluded.origin;
