-- +goose Up
ALTER TABLE attachments ADD COLUMN evidence_attempt_id TEXT REFERENCES native_attempts(id) ON DELETE SET NULL;
ALTER TABLE attachments ADD COLUMN evidence_caption TEXT CHECK (evidence_caption IS NULL OR length(CAST(evidence_caption AS BLOB)) BETWEEN 1 AND 1024);
CREATE INDEX attachments_attempt_evidence ON attachments(organization_id,project_id,evidence_attempt_id,work_item_id,created_at,id) WHERE evidence_attempt_id IS NOT NULL;

-- +goose Down
DROP INDEX attachments_attempt_evidence;
ALTER TABLE attachments DROP COLUMN evidence_caption;
ALTER TABLE attachments DROP COLUMN evidence_attempt_id;
