-- +goose Up
ALTER TABLE native_board_items ADD COLUMN page_window_mask integer NOT NULL DEFAULT 0;
ALTER TABLE native_board_items ADD COLUMN page_refresh_after timestamptz;
CREATE INDEX native_board_page_refresh_due ON native_board_items (organization_id,page_refresh_after,work_item_id) WHERE page_refresh_after IS NOT NULL;
CREATE TABLE native_board_page_summaries (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 audience text NOT NULL,
 archive_scope text NOT NULL,
 exclude_workspace boolean NOT NULL,
 kind text NOT NULL,
 state text NOT NULL,
 total bigint NOT NULL DEFAULT 0 CHECK (total >= 0),
 running bigint NOT NULL DEFAULT 0 CHECK (running >= 0),
 shipped_1h bigint NOT NULL DEFAULT 0 CHECK (shipped_1h >= 0),
 shipped_24h bigint NOT NULL DEFAULT 0 CHECK (shipped_24h >= 0),
 shipped_7d bigint NOT NULL DEFAULT 0 CHECK (shipped_7d >= 0),
 cancelled_1h bigint NOT NULL DEFAULT 0 CHECK (cancelled_1h >= 0),
 cancelled_24h bigint NOT NULL DEFAULT 0 CHECK (cancelled_24h >= 0),
 cancelled_7d bigint NOT NULL DEFAULT 0 CHECK (cancelled_7d >= 0),
 projected_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,audience,archive_scope,exclude_workspace,kind,state),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects (organization_id,id) ON DELETE CASCADE
);
ALTER TABLE native_board_page_summaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_page_summaries FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_page_summaries FROM PUBLIC,detent_platform_v2;
GRANT SELECT,INSERT,UPDATE,DELETE ON native_board_page_summaries TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON native_board_page_summaries TO detent_runtime_v2,detent_migration_v2
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE native_board_page_summaries;
DROP INDEX native_board_page_refresh_due;
ALTER TABLE native_board_items DROP COLUMN page_refresh_after;
ALTER TABLE native_board_items DROP COLUMN page_window_mask;
