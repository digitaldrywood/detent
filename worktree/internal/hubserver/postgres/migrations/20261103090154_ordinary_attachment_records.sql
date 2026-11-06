-- +goose Up
-- +goose StatementBegin
CREATE TABLE attachments (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    id text NOT NULL,
    uploader text NOT NULL,
    name text NOT NULL,
    content_type text NOT NULL,
    size bigint NOT NULL CHECK (size > 0 AND size <= 20971520),
    sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    width integer NOT NULL DEFAULT 0 CHECK (width >= 0),
    height integer NOT NULL DEFAULT 0 CHECK (height >= 0),
    created_at timestamptz NOT NULL,
    work_item_id text,
    comment_id text,
    evidence_attempt_id text,
    evidence_caption text NOT NULL DEFAULT '',
    kind text NOT NULL DEFAULT 'attachment' CHECK (kind='attachment'),
    retention_class text NOT NULL DEFAULT '',
    object_key text NOT NULL DEFAULT '',
    uncompressed_size bigint NOT NULL DEFAULT 0 CHECK (uncompressed_size>=0),
    codec text NOT NULL DEFAULT '',
    schema_version bigint NOT NULL DEFAULT 0 CHECK (schema_version>=0),
    storage_binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    pending_content bytea,
    deleted_at timestamptz,
    object_deleted_at timestamptz,
    PRIMARY KEY (organization_id,id),
    UNIQUE (organization_id,project_id,id),
    FOREIGN KEY (organization_id,project_id) REFERENCES projects (organization_id,id),
    FOREIGN KEY (organization_id,uploader) REFERENCES api_tokens (organization_id,id),
    CHECK (comment_id IS NULL OR work_item_id IS NOT NULL),
    CHECK (evidence_attempt_id IS NULL OR work_item_id IS NOT NULL),
    CHECK (object_deleted_at IS NULL OR deleted_at IS NOT NULL)
);
CREATE INDEX attachments_evidence ON attachments (organization_id,project_id,work_item_id,evidence_attempt_id,id) WHERE deleted_at IS NULL;
CREATE TABLE attachment_item_references (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    attachment_id text NOT NULL,
    work_item_id text NOT NULL,
    comment_id text NOT NULL DEFAULT '',
    retain_with_work_item boolean NOT NULL DEFAULT false,
    PRIMARY KEY (organization_id,attachment_id,work_item_id,comment_id),
    FOREIGN KEY (organization_id,project_id,attachment_id) REFERENCES attachments (organization_id,project_id,id)
);
ALTER TABLE attachments OWNER TO detent_migration;
ALTER TABLE attachment_item_references OWNER TO detent_migration;
REVOKE ALL ON attachments,attachment_item_references FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON attachments,attachment_item_references TO detent_runtime;
ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachments FORCE ROW LEVEL SECURITY;
ALTER TABLE attachment_item_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachment_item_references FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON attachments TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON attachment_item_references TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'ordinary attachment records require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
