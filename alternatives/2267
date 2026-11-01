-- +goose Up
DROP INDEX collaboration_events_conversion_idx;
CREATE INDEX collaboration_events_conversion_idx ON collaboration_events (organization_id, project_id, work_item_id, id, sequence)
 WHERE payload_format_version=1 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision');
CREATE TABLE storage_conversion_cursors (
 organization_id text NOT NULL,
 family text NOT NULL CHECK (family IN ('runs', 'versions', 'evidence')),
 turn bigint NOT NULL DEFAULT 0,
 positioned boolean NOT NULL DEFAULT false,
 key_1 text NOT NULL DEFAULT '',
 key_2 text NOT NULL DEFAULT '',
 key_3 text NOT NULL DEFAULT '',
 key_4 text NOT NULL DEFAULT '',
 sequence bigint NOT NULL DEFAULT 0,
 examined_rows bigint NOT NULL DEFAULT 0,
 examined_bytes bigint NOT NULL DEFAULT 0,
 oversized_rows bigint NOT NULL DEFAULT 0,
 PRIMARY KEY (organization_id, family)
);
ALTER TABLE storage_conversion_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE storage_conversion_cursors FORCE ROW LEVEL SECURITY;
REVOKE ALL ON storage_conversion_cursors FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON storage_conversion_cursors TO detent_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON storage_conversion_cursors TO detent_platform;
CREATE POLICY tenant_isolation ON storage_conversion_cursors TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON storage_conversion_cursors TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP INDEX collaboration_events_conversion_idx;
CREATE INDEX collaboration_events_conversion_idx ON collaboration_events (organization_id, project_id, work_item_id, id, sequence) WHERE payload_format_version=1;
DROP TABLE storage_conversion_cursors;
