-- +goose Up
CREATE TABLE conversations (
    organization_id text NOT NULL,
    id text NOT NULL,
    creation_key text NOT NULL DEFAULT '',
    creation_hash text NOT NULL DEFAULT '',
    project_id text NOT NULL,
    owner_principal_id text NOT NULL,
    owner_subject text NOT NULL DEFAULT '',
    origin text NOT NULL CHECK (origin IN ('user', 'worker')),
    title text NOT NULL DEFAULT '',
    visibility text NOT NULL CHECK (visibility IN ('private', 'shared')),
    status text NOT NULL CHECK (status IN ('active', 'settled')),
    subject_work_item_id text,
    work_item_id text,
    linked_at timestamptz,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    provider_thread_id text NOT NULL DEFAULT '',
    provider_thread_runner_id text NOT NULL DEFAULT '',
    provider_thread_origin text NOT NULL DEFAULT '',
    execution_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    event_seq bigint NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    preferences_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    query_metadata_version integer NOT NULL DEFAULT 0,
    execution_status text,
    execution_lease_id text,
    execution_settled boolean,
    read_audience text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_message_at timestamptz,
    settled_at timestamptz,
    archived boolean NOT NULL DEFAULT false,
    content_attachment_id text NOT NULL DEFAULT '',
    conversion_state smallint NOT NULL DEFAULT 0,
    PRIMARY KEY (organization_id, id),
    CHECK ((creation_key = '') = (creation_hash = '')),
    FOREIGN KEY (organization_id, owner_principal_id) REFERENCES api_tokens (organization_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    FOREIGN KEY (organization_id, project_id, subject_work_item_id) REFERENCES issues (organization_id, project_id, native_id),
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id)
);

CREATE UNIQUE INDEX conversations_work_item_idx ON conversations (organization_id, project_id, work_item_id) WHERE work_item_id IS NOT NULL;
CREATE UNIQUE INDEX conversations_creation_key_idx ON conversations (organization_id, owner_principal_id, creation_key) WHERE creation_key <> '';
CREATE INDEX conversations_owner_activity_idx ON conversations (organization_id, project_id, owner_principal_id, updated_at);
CREATE INDEX conversations_activity_idx ON conversations (organization_id, project_id, COALESCE(last_message_at, updated_at), id);
CREATE INDEX conversations_settle_idx ON conversations (status, COALESCE(last_message_at, updated_at)) WHERE archived = false;
CREATE INDEX conversations_execution_lease_idx ON conversations (organization_id, execution_lease_id, execution_status);
CREATE INDEX conversations_record_payload_pending ON conversations (conversion_state) WHERE conversion_state=0;
CREATE INDEX conversations_record_payload_queued ON conversations (organization_id) WHERE conversion_state=1;
CREATE INDEX conversations_content_attachment_id_lookup ON conversations (content_attachment_id);

CREATE TABLE conversation_messages (
    organization_id text NOT NULL,
    id text NOT NULL,
    conversation_id text NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    role text NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
    kind text NOT NULL CHECK (kind IN ('text', 'answer', 'interrupt', 'continue', 'tool', 'status')),
    text text NOT NULL DEFAULT '',
    data_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    delivery text NOT NULL CHECK (delivery IN ('saved', 'queued', 'sending', 'sent', 'delivered', 'responding', 'completed', 'interrupted', 'rejected', 'failed', 'unknown')),
    attempt_id text NOT NULL DEFAULT '',
    thread_id text NOT NULL DEFAULT '',
    turn_id text NOT NULL DEFAULT '',
    provider_item_id text NOT NULL DEFAULT '',
    actor_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    command_key text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    content_attachment_id text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    actor_kind text,
    actor_principal_id text,
    text_bytes bigint,
    stored_text_bytes bigint,
    conversion_state smallint NOT NULL DEFAULT 0,
    query_metadata_version integer NOT NULL DEFAULT 0,
    tool_name text,
    tool_item_id text,
    tool_status text,
    operator_action_id text,
    operator_action_agent text,
    operator_action_result text,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, conversation_id, id),
    UNIQUE (organization_id, conversation_id, seq),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX conversation_messages_delivery_idx ON conversation_messages (organization_id, delivery);
CREATE INDEX conversation_messages_item_idx ON conversation_messages (organization_id, conversation_id, attempt_id, provider_item_id, seq);
CREATE INDEX conversation_messages_action_idx ON conversation_messages (organization_id, conversation_id, operator_action_id);
CREATE INDEX conversation_messages_action_result_idx ON conversation_messages (organization_id, conversation_id, operator_action_result);
CREATE INDEX conversation_messages_payload_pending_idx ON conversation_messages (organization_id, conversation_id) WHERE actor_principal_id IS NULL;
CREATE INDEX conversation_messages_record_payload_pending ON conversation_messages (conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_messages_record_payload_queued ON conversation_messages (organization_id, conversation_id) WHERE conversion_state=1;
CREATE INDEX conversation_messages_content_attachment_id_lookup ON conversation_messages (content_attachment_id);

CREATE TABLE conversation_questions (
    organization_id text NOT NULL,
    id text NOT NULL,
    conversation_id text NOT NULL,
    message_id text NOT NULL DEFAULT '',
    request_id text NOT NULL DEFAULT '',
    attempt_id text NOT NULL DEFAULT '',
    thread_id text NOT NULL DEFAULT '',
    turn_id text NOT NULL DEFAULT '',
    status text NOT NULL CHECK (status IN ('pending', 'sending', 'sent', 'answered', 'expired', 'unknown')),
    prompts_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    answers_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    answered_by text NOT NULL DEFAULT '',
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    content_attachment_id text NOT NULL DEFAULT '',
    conversion_state smallint NOT NULL DEFAULT 0,
    PRIMARY KEY (organization_id, conversation_id, id),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX conversation_questions_pending_idx ON conversation_questions (organization_id, conversation_id, status, created_at);
CREATE INDEX conversation_questions_outstanding_idx ON conversation_questions (organization_id, conversation_id, id) WHERE status IN ('pending', 'sending', 'sent');
CREATE INDEX conversation_questions_record_payload_pending ON conversation_questions (conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_questions_record_payload_queued ON conversation_questions (organization_id, conversation_id) WHERE conversion_state=1;
CREATE INDEX conversation_questions_content_attachment_id_lookup ON conversation_questions (content_attachment_id);

CREATE TABLE conversation_commands (
    organization_id text NOT NULL,
    conversation_id text NOT NULL,
    key text NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
    kind text NOT NULL CHECK (kind IN ('message', 'answer', 'interrupt', 'continue', 'cancel', 'retry')),
    request_hash text NOT NULL,
    receipt_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    content_attachment_id text NOT NULL DEFAULT '',
    conversion_state smallint NOT NULL DEFAULT 0,
    query_metadata_version integer NOT NULL DEFAULT 0,
    receipt_status text,
    PRIMARY KEY (organization_id, conversation_id, key),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX conversation_commands_record_payload_pending ON conversation_commands (conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_commands_record_payload_queued ON conversation_commands (organization_id, conversation_id) WHERE conversion_state=1;
CREATE INDEX conversation_commands_content_attachment_id_lookup ON conversation_commands (content_attachment_id);

CREATE TABLE conversation_events (
    organization_id text NOT NULL,
    conversation_id text NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    type text NOT NULL CHECK (type IN ('conversation.updated', 'message.accepted', 'message.delta', 'message.updated', 'question.opened', 'question.updated', 'execution.updated', 'command.receipt', 'heartbeat', 'closed')),
    body_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    content_attachment_id text NOT NULL DEFAULT '',
    conversion_state smallint NOT NULL DEFAULT 0,
    query_metadata_version integer NOT NULL DEFAULT 0,
    message_id text,
    object_id text,
    message_role text,
    message_kind text,
    attempt_id text,
    execution_status text,
    thread_id text,
    turn_id text,
    snapshot_title text,
    snapshot_visibility text,
    snapshot_status text,
    snapshot_preferences_key text,
    snapshot_model text,
    snapshot_effort text,
    snapshot_access text,
    snapshot_subject_work_item_id text,
    snapshot_work_item_id text,
    snapshot_linked_at timestamptz,
    delta_offset bigint,
    delta_bytes bigint,
    message_revision bigint,
    PRIMARY KEY (organization_id, conversation_id, seq),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX conversation_events_delta_message_idx ON conversation_events (organization_id, conversation_id, message_id, seq) WHERE type = 'message.delta';
CREATE INDEX conversation_events_message_update_idx ON conversation_events (organization_id, conversation_id, object_id, seq) WHERE type = 'message.updated';
CREATE INDEX conversation_events_execution_update_idx ON conversation_events (organization_id, conversation_id, attempt_id, execution_status, thread_id, turn_id, seq) WHERE type = 'execution.updated';
CREATE INDEX conversation_events_payload_pending_idx ON conversation_events (organization_id, conversation_id) WHERE content_attachment_id = '';
CREATE INDEX conversation_events_record_payload_pending ON conversation_events (conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_events_record_payload_queued ON conversation_events (organization_id, conversation_id) WHERE conversion_state=1;
CREATE INDEX conversation_events_content_attachment_id_lookup ON conversation_events (content_attachment_id);

CREATE TABLE conversation_starts (
    organization_id text NOT NULL,
    attempt_id text NOT NULL,
    conversation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, attempt_id),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE conversation_audience_events (
    organization_id text NOT NULL,
    id text NOT NULL,
    conversation_id text NOT NULL,
    actor_principal_id text NOT NULL,
    from_visibility text NOT NULL CHECK (from_visibility IN ('private', 'shared')),
    to_visibility text NOT NULL CHECK (to_visibility IN ('private', 'shared')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX conversation_audience_events_idx ON conversation_audience_events (organization_id, conversation_id, created_at);

CREATE TABLE conversation_turn_batches (
    organization_id text NOT NULL,
    attempt_id text NOT NULL,
    batch_key text NOT NULL CHECK (length(batch_key) BETWEEN 1 AND 128),
    event_seq bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, attempt_id, batch_key)
);

CREATE TABLE conversation_tool_revisions (
    organization_id text NOT NULL,
    conversation_id text NOT NULL,
    message_id text NOT NULL,
    revision bigint NOT NULL,
    text text NOT NULL,
    tool text NOT NULL,
    item_id text NOT NULL,
    status text NOT NULL,
    content_attachment_id text NOT NULL DEFAULT '',
    conversion_state smallint NOT NULL DEFAULT 0,
    PRIMARY KEY (organization_id, conversation_id, message_id, revision),
    FOREIGN KEY (organization_id, conversation_id, message_id) REFERENCES conversation_messages (organization_id, conversation_id, id) ON DELETE CASCADE
);

CREATE INDEX conversation_tool_revisions_payload_pending_idx ON conversation_tool_revisions (organization_id, conversation_id, message_id, revision) WHERE content_attachment_id = '';

CREATE TABLE message_references (
    organization_id text NOT NULL,
    message_id text NOT NULL,
    conversation_id text NOT NULL,
    position bigint NOT NULL CHECK (position >= 0),
    target_kind text NOT NULL CHECK (target_kind IN ('issue', 'conversation')),
    target_id text NOT NULL,
    label text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, message_id, target_kind, target_id),
    FOREIGN KEY (organization_id, conversation_id, message_id) REFERENCES conversation_messages (organization_id, conversation_id, id) ON DELETE CASCADE
);

CREATE INDEX message_references_target_idx ON message_references (organization_id, target_kind, target_id, created_at, message_id);
CREATE UNIQUE INDEX message_references_position_idx ON message_references (organization_id, conversation_id, message_id, position);

CREATE TABLE conversation_content_objects (
    organization_id text NOT NULL,
    id text NOT NULL,
    object_key text NOT NULL,
    sha256 text NOT NULL,
    compressed_size bigint NOT NULL CHECK (compressed_size >= 0),
    uncompressed_size bigint NOT NULL CHECK (uncompressed_size >= 0),
    codec text NOT NULL,
    schema_version integer NOT NULL CHECK (schema_version > 0),
    kind text NOT NULL,
    retention text NOT NULL,
    binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, object_key, sha256)
);

-- +goose StatementBegin
DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['conversations','conversation_messages','conversation_questions','conversation_commands','conversation_events','conversation_starts','conversation_audience_events','conversation_turn_batches','conversation_tool_revisions','message_references','conversation_content_objects'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', table_name);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO detent_runtime, detent_platform', table_name);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting(''detent.organization_id'', true), '''')) WITH CHECK (organization_id = NULLIF(current_setting(''detent.organization_id'', true), ''''))', table_name);
        EXECUTE format('CREATE POLICY platform_access ON %I TO detent_platform USING (true) WITH CHECK (true)', table_name);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE message_references;
DROP TABLE conversation_content_objects;
DROP TABLE conversation_tool_revisions;
DROP TABLE conversation_turn_batches;
DROP TABLE conversation_audience_events;
DROP TABLE conversation_starts;
DROP TABLE conversation_events;
DROP TABLE conversation_commands;
DROP TABLE conversation_questions;
DROP TABLE conversation_messages;
DROP TABLE conversations;
