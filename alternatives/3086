-- +goose Up
ALTER TABLE native_attempts ADD COLUMN runtime_heartbeat_at timestamptz;
ALTER TABLE native_attempts ADD COLUMN runtime_summary_sequence bigint;
ALTER TABLE native_attempts ADD CONSTRAINT native_attempt_runtime_summary_current CHECK (runtime_summary_sequence IS NULL OR (runtime_summary_sequence > 0 AND runtime_summary_sequence<=sequence));
INSERT INTO native_board_dirty(organization_id,project_id,work_item_id)
SELECT DISTINCT a.organization_id,i.project_id,i.native_id FROM native_attempts a
JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
WHERE a.status='running' AND l.released_at IS NULL AND l.expires_at>now() AND i.native_id<>''
ON CONFLICT DO NOTHING;

-- +goose Down
ALTER TABLE native_attempts DROP CONSTRAINT native_attempt_runtime_summary_current;
ALTER TABLE native_attempts DROP COLUMN runtime_summary_sequence;
ALTER TABLE native_attempts DROP COLUMN runtime_heartbeat_at;
