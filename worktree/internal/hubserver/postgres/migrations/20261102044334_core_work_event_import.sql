-- +goose Up
ALTER TABLE runner_identity_events ADD COLUMN import_source_id bigint;
CREATE UNIQUE INDEX runner_identity_events_import_source_idx ON runner_identity_events (organization_id, import_source_id) WHERE import_source_id IS NOT NULL;

CREATE TABLE core_work_events (
    organization_id text NOT NULL,
    source_id bigint NOT NULL CHECK (source_id > 0),
    issue_id bigint NOT NULL,
    fencing_token bigint,
    machine_id text,
    session_id text,
    run_id text,
    kind text NOT NULL CHECK (btrim(kind) <> ''),
    payload_json text NOT NULL CHECK (jsonb_typeof(payload_json::jsonb) IS NOT NULL),
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, source_id),
    FOREIGN KEY (organization_id, issue_id) REFERENCES issues (organization_id, id),
    FOREIGN KEY (organization_id, fencing_token) REFERENCES leases (organization_id, fencing_token),
    FOREIGN KEY (organization_id, machine_id) REFERENCES machines (organization_id, id)
);

CREATE INDEX core_work_events_issue_idx ON core_work_events (organization_id, issue_id, source_id);
CREATE INDEX core_work_events_session_idx ON core_work_events (organization_id, session_id, source_id) WHERE session_id IS NOT NULL;

ALTER TABLE core_work_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE core_work_events FORCE ROW LEVEL SECURITY;
REVOKE ALL ON core_work_events FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON core_work_events TO detent_runtime, detent_platform;
CREATE POLICY tenant_isolation ON core_work_events TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON core_work_events TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE core_work_events OWNER TO detent_migration;

-- +goose Down
DROP TABLE core_work_events;
DROP INDEX runner_identity_events_import_source_idx;
ALTER TABLE runner_identity_events DROP COLUMN import_source_id;
