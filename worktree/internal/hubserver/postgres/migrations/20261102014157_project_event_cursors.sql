-- +goose Up
ALTER TABLE collaboration_events
    ADD COLUMN project_sequence bigint NOT NULL DEFAULT 0 CHECK (project_sequence >= 0);

CREATE TABLE project_event_cursors (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence >= 0),
    PRIMARY KEY (organization_id, project_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX collaboration_events_project_sequence_idx
    ON collaboration_events (organization_id, project_id, project_sequence, work_item_id);
CREATE INDEX issues_activity_cursor_idx
    ON issues (organization_id, project_id, last_activity_at, native_id);

ALTER TABLE project_event_cursors OWNER TO detent_migration;
REVOKE ALL ON project_event_cursors FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON project_event_cursors TO detent_runtime;
ALTER TABLE project_event_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_event_cursors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON project_event_cursors TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP INDEX collaboration_events_project_sequence_idx;
DROP INDEX issues_activity_cursor_idx;
DROP TABLE project_event_cursors;
ALTER TABLE collaboration_events DROP COLUMN project_sequence;
