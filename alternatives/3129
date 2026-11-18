-- +goose Up
ALTER TABLE native_attempts ADD COLUMN claim_disposition text
    CHECK (claim_disposition IN ('', 'progress', 'human_action', 'permission_wait', 'recorded_acceptance'));

-- +goose Down
ALTER TABLE native_attempts DROP COLUMN claim_disposition;
