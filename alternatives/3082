-- +goose Up
ALTER TABLE backlog_admission_proposals
ADD COLUMN recommended_effort TEXT;

ALTER TABLE backlog_admission_proposals
ADD COLUMN effort_rationale TEXT;

-- +goose Down
ALTER TABLE backlog_admission_proposals
DROP COLUMN effort_rationale;

ALTER TABLE backlog_admission_proposals
DROP COLUMN recommended_effort;
