-- +goose Up
CREATE TABLE runner_selection_cursors (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    source_revision text NOT NULL,
    priority bigint NOT NULL,
    state_rank bigint NOT NULL,
    label_rank bigint NOT NULL,
    aging_boost bigint NOT NULL,
    landing_rank bigint NOT NULL,
    lane_rank bigint NOT NULL,
    unblocker_rank bigint NOT NULL,
    rank text NOT NULL,
    created_at timestamptz NOT NULL,
    work_item_id text NOT NULL,
    cursor_source_revision bigint NOT NULL CHECK (cursor_source_revision >= 0),
    revision bigint NOT NULL CHECK (revision > 0),
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

ALTER TABLE runner_selection_cursors OWNER TO detent_migration;
REVOKE ALL ON runner_selection_cursors FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_selection_cursors TO detent_runtime;
ALTER TABLE runner_selection_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_selection_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_selection_cursors
    TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE runner_selection_cursors;
