-- +goose Up
ALTER TABLE backlog_admission_declines
ADD COLUMN confidence REAL CHECK (confidence IS NULL OR (confidence >= 0.0 AND confidence <= 1.0));

ALTER TABLE backlog_admission_declines
ADD COLUMN failed_dimension TEXT NOT NULL DEFAULT '';

ALTER TABLE backlog_admission_declines
ADD COLUMN failed_criterion TEXT NOT NULL DEFAULT '';

ALTER TABLE backlog_admission_runs
ADD COLUMN proposal_reason TEXT;

-- +goose Down
ALTER TABLE backlog_admission_runs DROP COLUMN proposal_reason;
ALTER TABLE backlog_admission_declines DROP COLUMN failed_criterion;
ALTER TABLE backlog_admission_declines DROP COLUMN failed_dimension;
ALTER TABLE backlog_admission_declines DROP COLUMN confidence;
