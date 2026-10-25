-- +goose Up
CREATE INDEX native_attempts_checkpoint_lookup ON native_attempts(organization_id, project_id, work_item_id, checkpoint_preserves_source, fencing_token DESC);

-- +goose Down
DROP INDEX native_attempts_checkpoint_lookup;
