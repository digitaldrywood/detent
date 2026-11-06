-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY attachments_evidence_history_order ON attachments
    (organization_id,project_id,work_item_id,evidence_attempt_id,created_at,id);

-- +goose Down
DROP INDEX CONCURRENTLY attachments_evidence_history_order;
