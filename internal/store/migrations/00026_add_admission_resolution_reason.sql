-- +goose Up
ALTER TABLE backlog_admission_proposals
ADD COLUMN resolution_reason TEXT;

-- +goose Down
ALTER TABLE backlog_admission_proposals
DROP COLUMN resolution_reason;
