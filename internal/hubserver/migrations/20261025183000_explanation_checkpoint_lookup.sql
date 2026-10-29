-- +goose Up
CREATE INDEX native_attempts_checkpoint_lookup ON native_attempts(organization_id, project_id, work_item_id, checkpoint_preserves_source, fencing_token DESC);
CREATE INDEX runner_identities_organization_order ON runner_identities(organization_id, id);

-- +goose Down
DROP INDEX native_attempts_checkpoint_lookup;
DROP INDEX runner_identities_organization_order;
