-- +goose Up
-- +goose StatementBegin
CREATE TABLE conversation_attachments (
    organization_id text NOT NULL,
    conversation_id text NOT NULL,
    project_id text NOT NULL,
    id text NOT NULL,
    message_id text,
    principal_id text NOT NULL,
    name text NOT NULL,
    mime text NOT NULL,
    size bigint NOT NULL CHECK (size > 0),
    artifact_ref text NOT NULL,
    content_attachment_id text,
    created_at timestamptz NOT NULL,
    expires_at timestamptz,
    deleted_at timestamptz,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, artifact_ref),
    FOREIGN KEY (organization_id, conversation_id) REFERENCES conversations (organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, conversation_id, message_id) REFERENCES conversation_messages (organization_id, conversation_id, id),
    FOREIGN KEY (organization_id, content_attachment_id) REFERENCES conversation_content_objects (organization_id, id),
    CHECK (message_id IS NULL OR expires_at IS NULL)
);

CREATE INDEX conversation_attachments_message_idx ON conversation_attachments (organization_id, message_id, created_at, id) WHERE message_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX conversation_attachments_pending_idx ON conversation_attachments (organization_id, conversation_id, principal_id) WHERE message_id IS NULL AND deleted_at IS NULL;
CREATE INDEX conversation_attachments_sweep_idx ON conversation_attachments (organization_id, expires_at, id) WHERE message_id IS NULL AND deleted_at IS NULL;

ALTER TABLE conversation_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_attachments FORCE ROW LEVEL SECURITY;
REVOKE ALL ON conversation_attachments FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON conversation_attachments TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON conversation_attachments TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON conversation_attachments TO detent_platform USING (true) WITH CHECK (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM conversation_attachments) THEN
        RAISE EXCEPTION 'conversation attachment records require an explicit export before rollback';
    END IF;
END
$$;
DROP TABLE conversation_attachments;
-- +goose StatementEnd
