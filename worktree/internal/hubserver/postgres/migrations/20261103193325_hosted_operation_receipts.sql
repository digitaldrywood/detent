-- +goose Up
CREATE TABLE native_command_states (
    organization_id text NOT NULL,
    actor_id text NOT NULL,
    operation text NOT NULL,
    command_key text NOT NULL,
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    response_claimed boolean NOT NULL DEFAULT false,
    response_pending boolean NOT NULL DEFAULT false,
    response_request_id text NOT NULL DEFAULT '',
    response_expired boolean NOT NULL DEFAULT false,
    response_replay_until_ns bigint NOT NULL DEFAULT 0,
    response_reconciliation_hash text NOT NULL DEFAULT '',
    source_snapshot_id text NOT NULL DEFAULT '',
    source_record_hash text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id, actor_id, operation, command_key),
    CHECK (NOT response_claimed OR (response_pending AND NOT response_expired AND response_request_id = ''))
);

CREATE INDEX native_commands_replay_expiry ON native_command_states (organization_id, response_replay_until_ns, actor_id, operation, command_key)
    WHERE NOT response_claimed AND NOT response_pending AND NOT response_expired AND response_replay_until_ns > 0
        AND (operation <> 'urgent_runner_update' OR response_reconciliation_hash <> '');
CREATE INDEX hosted_members_principal_idx ON hosted_members (organization_id, principal_id);

ALTER TABLE native_command_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_command_states FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_command_states FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_command_states TO detent_runtime;
CREATE POLICY tenant_isolation ON native_command_states TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'command receipt states require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
