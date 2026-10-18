-- +goose Up
ALTER TABLE project_dispatch_status
ADD COLUMN eligible_candidate_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE project_dispatch_status
DROP COLUMN eligible_candidate_count;
