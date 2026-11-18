-- +goose Up
-- +goose StatementBegin
CREATE TABLE archive_objects (
    organization_id text NOT NULL,
    id text NOT NULL,
    object_key text NOT NULL,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    compressed_size bigint NOT NULL CHECK (compressed_size >= 0),
    uncompressed_size bigint NOT NULL CHECK (uncompressed_size >= 0),
    codec text NOT NULL,
    schema_version integer NOT NULL CHECK (schema_version > 0),
    kind text NOT NULL,
    retention text NOT NULL,
    binding_id text NOT NULL DEFAULT '',
    customer_owned boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, object_key, sha256)
);

CREATE TABLE archive_object_owners (
    organization_id text NOT NULL,
    archive_id text NOT NULL,
    owner_kind text NOT NULL,
    owner_id text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, archive_id, owner_kind, owner_id),
    FOREIGN KEY (organization_id, archive_id) REFERENCES archive_objects (organization_id, id) ON DELETE CASCADE,
    CHECK (owner_kind <> ''),
    CHECK (owner_id <> '')
);

CREATE INDEX archive_object_owners_owner_idx ON archive_object_owners (organization_id, owner_kind, owner_id);

CREATE TABLE archive_usage_counters (
    organization_id text PRIMARY KEY,
    compressed_bytes bigint NOT NULL DEFAULT 0 CHECK (compressed_bytes >= 0)
);

INSERT INTO archive_objects (organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at)
SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at
FROM conversation_content_objects;

INSERT INTO archive_object_owners (organization_id,archive_id,owner_kind,owner_id,created_at)
SELECT organization_id,id,'conversation-content',id,created_at FROM conversation_content_objects;

INSERT INTO archive_usage_counters (organization_id,compressed_bytes)
SELECT organization_id,COALESCE(sum(compressed_size) FILTER (WHERE NOT customer_owned),0)
FROM archive_objects
GROUP BY organization_id;

ALTER TABLE conversation_content_objects ADD COLUMN archive_id text;
UPDATE conversation_content_objects SET archive_id=id;
ALTER TABLE conversation_content_objects ALTER COLUMN archive_id SET NOT NULL;
ALTER TABLE conversation_content_objects ADD CONSTRAINT conversation_content_archive_fk FOREIGN KEY (organization_id,archive_id) REFERENCES archive_objects (organization_id,id);

DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['archive_objects','archive_object_owners','archive_usage_counters'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', table_name);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO detent_runtime, detent_platform', table_name);
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY['archive_objects','archive_object_owners','archive_usage_counters'] LOOP
        EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting(''detent.organization_id'', true), '''')) WITH CHECK (organization_id = NULLIF(current_setting(''detent.organization_id'', true), ''''))', table_name);
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY['archive_objects','archive_object_owners','archive_usage_counters'] LOOP
        EXECUTE format('CREATE POLICY platform_access ON %I TO detent_platform USING (true) WITH CHECK (true)', table_name);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM archive_objects o
        WHERE NOT EXISTS (
            SELECT 1 FROM conversation_content_objects c
            WHERE c.organization_id=o.organization_id AND c.archive_id=o.id
        )
    ) OR EXISTS (
        SELECT 1 FROM archive_object_owners
        WHERE owner_kind <> 'conversation-content'
    ) THEN
        RAISE EXCEPTION 'shared archive records require an explicit export before rollback';
    END IF;
END
$$;
ALTER TABLE conversation_content_objects DROP CONSTRAINT conversation_content_archive_fk;
ALTER TABLE conversation_content_objects DROP COLUMN archive_id;
DROP TABLE archive_usage_counters;
DROP TABLE archive_object_owners;
DROP TABLE archive_objects;
-- +goose StatementEnd
