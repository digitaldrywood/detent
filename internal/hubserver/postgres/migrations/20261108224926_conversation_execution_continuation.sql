-- +goose Up
-- +goose StatementBegin
ALTER TABLE conversations ADD COLUMN execution_continuation boolean;
CREATE TABLE conversation_continuation_recovery (
    organization_id text NOT NULL,
    snapshot_id text NOT NULL,
    record_kind text NOT NULL CHECK (record_kind IN ('source','conversation')),
    conversation_id text NOT NULL,
    catalog_digest text NOT NULL,
    project_id text,
    work_item_id text,
    source_row_id bigint,
    receipt_hash text,
    legacy_receipt_hash text,
    event_head bigint,
    continue_count bigint,
    boundary_sha256 text,
    plan_count bigint,
    source_id text,
    manifest_sha256 text,
    PRIMARY KEY (organization_id,snapshot_id,record_kind,conversation_id)
);
CREATE UNIQUE INDEX conversation_continuation_recovery_identity ON conversation_continuation_recovery(organization_id,conversation_id) WHERE record_kind='conversation';
ALTER TABLE conversation_continuation_recovery OWNER TO detent_migration;
REVOKE ALL ON conversation_continuation_recovery FROM PUBLIC,detent_runtime,detent_platform;

CREATE POLICY continuation_upgrade ON conversations TO detent_migration USING (true) WITH CHECK (true);
CREATE POLICY continuation_upgrade ON conversation_messages FOR SELECT TO detent_migration USING (true);
CREATE POLICY continuation_upgrade ON conversation_commands FOR SELECT TO detent_migration USING (true);
CREATE POLICY continuation_upgrade ON conversation_events FOR SELECT TO detent_migration USING (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE conversation_continuation_recovery;
DROP POLICY IF EXISTS continuation_upgrade ON conversation_events;
DROP POLICY IF EXISTS continuation_upgrade ON conversation_commands;
DROP POLICY IF EXISTS continuation_upgrade ON conversation_messages;
DROP POLICY IF EXISTS continuation_upgrade ON conversations;
ALTER TABLE conversations DROP COLUMN execution_continuation;
-- +goose StatementEnd
