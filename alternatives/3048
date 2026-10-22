-- +goose Up
-- Reuse operator application events as durable command receipts.
CREATE UNIQUE INDEX workflow_operator_retry_identity_idx
ON workflow_phase_events(json_extract(metadata_json, '$.retry_identity'))
WHERE phase_type = 'operator_action' AND json_valid(metadata_json) AND json_extract(metadata_json, '$.retry_identity') IS NOT NULL;

-- +goose Down
DROP INDEX workflow_operator_retry_identity_idx;
